package queries_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/usmanvirk/query-plans/internal/plan"
	"github.com/usmanvirk/query-plans/internal/queries"
)

// The fixture is built once for the whole package. 600,000 rows take about
// five seconds to load, and doing that per test would make the suite
// something nobody runs.
var (
	once   sync.Once
	shared *pgxpool.Pool
)

func db(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}
	once.Do(func() {
		pool, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Fatalf("connecting: %v", err)
		}
		if err := queries.Load(context.Background(), pool); err != nil {
			t.Fatalf("loading the fixture: %v", err)
		}
		shared = pool
	})
	if shared == nil {
		t.Fatal("the fixture failed to load")
	}
	return shared
}

// withIndexes and withoutIndexes bracket a test. The tests do not run in
// parallel for this reason: they change the schema under each other.
func withIndexes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := queries.AddIndexes(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
}

func withoutIndexes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := queries.DropIndexes(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
}

func explain(t *testing.T, pool *pgxpool.Pool, q string, args ...any) *plan.Plan {
	t.Helper()
	p, err := queries.Explain(context.Background(), pool, q, args...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBrowseUsesTheCompositeIndexAndDoesNotSort is the main one. The index
// has to serve the filter and return the rows already in price order, so
// there is no sort node at all and only the twenty rows asked for are read.
func TestBrowseUsesTheCompositeIndexAndDoesNotSort(t *testing.T) {
	pool := db(t)
	withIndexes(t, pool)

	pair := queries.All()[0]
	p := explain(t, pool, pair.Fast, pair.FastArgs...)

	if err := plan.Require(p,
		plan.NoSeqScanOn("listings"),
		plan.UsesIndex("listings_browse_idx"),
		plan.NoSort(),
		plan.MaxRowsRead(50),
		plan.MaxRowsDiscarded(0),
	); err != nil {
		t.Fatal(err)
	}
}

// TestBrowseWithoutTheIndexIsWhatItLooksLike. The requirements are only
// worth anything if they fail on the query they were written against, so
// the same assertions are run against the unindexed schema and expected to
// fail.
func TestBrowseWithoutTheIndexFailsTheSameRequirements(t *testing.T) {
	pool := db(t)
	withoutIndexes(t, pool)
	t.Cleanup(func() { withIndexes(t, pool) })

	pair := queries.All()[0]
	p := explain(t, pool, pair.Slow, pair.SlowArgs...)

	err := plan.Require(p,
		plan.NoSeqScanOn("listings"),
		plan.UsesIndex("listings_browse_idx"),
		plan.NoSort(),
		plan.MaxRowsRead(50),
	)
	if err == nil {
		t.Fatal("the unindexed query satisfied every requirement, so the requirements check nothing")
	}

	// And the shape of the problem is what it is supposed to be: the rows
	// are read and thrown away rather than never read.
	if got := p.RowsDiscarded(); got < 10000 {
		t.Fatalf("only %.0f rows were discarded, so this is not the problem the index fixes", got)
	}
	if len(p.SeqScans()) == 0 {
		t.Fatal("no sequential scan without the index, so the fixture is too small to be realistic")
	}
}

func TestEmailLookupNeedsAnExpressionIndex(t *testing.T) {
	pool := db(t)
	withIndexes(t, pool)

	pair := queries.All()[1]
	p := explain(t, pool, pair.Fast, pair.FastArgs...)

	if err := plan.Require(p,
		plan.NoSeqScanOn("sellers"),
		plan.UsesIndex("sellers_lower_email_idx"),
		plan.MaxRowsRead(5),
	); err != nil {
		t.Fatal(err)
	}
}

// TestAnIndexOnTheColumnDoesNotServeAnIndexOnTheExpression is the lesson of
// that pair, asserted rather than written in a comment.
func TestAnIndexOnTheColumnDoesNotServeTheExpression(t *testing.T) {
	pool := db(t)
	ctx := context.Background()
	withoutIndexes(t, pool)
	t.Cleanup(func() { withIndexes(t, pool) })

	if _, err := pool.Exec(ctx, `CREATE INDEX sellers_email_plain_idx ON sellers (email)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DROP INDEX IF EXISTS sellers_email_plain_idx`)
	})
	if _, err := pool.Exec(ctx, `ANALYZE sellers`); err != nil {
		t.Fatal(err)
	}

	pair := queries.All()[1]
	p := explain(t, pool, pair.Slow, pair.SlowArgs...)

	if len(p.SeqScans()) == 0 {
		t.Fatalf("an index on (email) served lower(email), which it cannot:\n%s", p)
	}
}

// TestKeysetPaginationCostsTheSameOnEveryPage. OFFSET reads and discards
// everything it skips, so page 500 costs five hundred times page one.
func TestKeysetPaginationReadsOnlyThePage(t *testing.T) {
	pool := db(t)
	withIndexes(t, pool)

	pair := queries.All()[2]
	p := explain(t, pool, pair.Fast, pair.FastArgs...)

	if err := plan.Require(p,
		plan.UsesIndex("listings_created_at_id_idx"),
		plan.NoSort(),
		plan.MaxRowsRead(50),
	); err != nil {
		t.Fatal(err)
	}
}

func TestOffsetPaginationReadsEverythingItSkips(t *testing.T) {
	pool := db(t)
	withIndexes(t, pool)

	pair := queries.All()[2]
	p := explain(t, pool, pair.Slow, pair.SlowArgs...)

	// Even with the index available, OFFSET 10000 has to walk past ten
	// thousand rows. That is the point: this one is not fixed by an index,
	// it is fixed by changing the query.
	if got := p.RowsRead(); got < 10000 {
		t.Fatalf("OFFSET 10000 read only %.0f rows, which is not what OFFSET does", got)
	}
}

// TestTheObviousDeN1RewriteIsWorseThanTheN1 is the finding this repository
// is most worth reading for. Replacing a correlated subquery with a joined
// aggregate is the standard advice, and here it is 600 times slower,
// because the aggregate runs over the whole events table before the filter
// on seller narrows anything.
func TestTheGroupedJoinBeatsTheUnrestrictedAggregate(t *testing.T) {
	pool := db(t)
	withIndexes(t, pool)

	pair := queries.All()[3]
	slow := explain(t, pool, pair.Slow, pair.SlowArgs...)
	fast := explain(t, pool, pair.Fast, pair.FastArgs...)

	if err := plan.Require(fast,
		plan.MaxRowsRead(2000),
		plan.MaxRowsDiscarded(100),
	); err != nil {
		t.Fatal(err)
	}

	// The slow one reads most of the events table to answer a question
	// about one seller.
	if slow.RowsRead() < 100000 {
		t.Fatalf("the unrestricted aggregate read %.0f rows, so it is not aggregating the whole table",
			slow.RowsRead())
	}
	if slow.RowsRead() < fast.RowsRead()*20 {
		t.Fatalf("the rewrite reads %.0f rows against %.0f, which is not the gap this test is about",
			fast.RowsRead(), slow.RowsRead())
	}
}

// TestEveryPairImprovesOnRowsRead is the sweep: for each pair, the fixed
// form must read fewer rows than the original. Rows rather than time,
// because rows are the same number on a laptop and on a runner.
func TestEveryPairReadsFewerRowsAfterItsFix(t *testing.T) {
	pool := db(t)

	for _, pair := range queries.All() {
		t.Run(pair.Name, func(t *testing.T) {
			withoutIndexes(t, pool)
			slow := explain(t, pool, pair.Slow, pair.SlowArgs...)

			withIndexes(t, pool)
			fast := explain(t, pool, pair.Fast, pair.FastArgs...)

			slowWork := slow.RowsRead() + slow.RowsDiscarded()
			fastWork := fast.RowsRead() + fast.RowsDiscarded()

			if fastWork >= slowWork {
				t.Fatalf("after the fix the query touches %.0f rows, against %.0f before:\n\nbefore:\n%s\nafter:\n%s",
					fastWork, slowWork, slow, fast)
			}
			t.Logf("%s: %.0f rows touched before, %.0f after", pair.Name, slowWork, fastWork)
		})
	}
	withIndexes(t, pool)
}

func TestTheFixtureIsBigEnoughToBeRealistic(t *testing.T) {
	pool := db(t)
	ctx := context.Background()

	var listings, events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM listings`).Scan(&listings); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if listings != queries.Listings || events != queries.Events {
		t.Fatalf("the fixture holds %d listings and %d events", listings, events)
	}

	// Below about 50,000 rows the planner picks a sequential scan because
	// it genuinely is cheapest, and then every assertion here is measuring
	// the fixture rather than the query.
	if queries.Listings < 50000 {
		t.Fatal("the fixture is too small for the planner to make realistic choices")
	}
}

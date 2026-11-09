// Command planbench loads the fixture and shows each query before and
// after its fix: the plan, the rows read, and the time.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/usmanvirk/query-plans/internal/plan"
	"github.com/usmanvirk/query-plans/internal/queries"
)

func main() {
	var (
		dsn     = flag.String("database-url", os.Getenv("DATABASE_URL"), "Postgres DSN")
		load    = flag.Bool("load", false, "rebuild the fixture first, which takes a few seconds")
		verbose = flag.Bool("v", false, "print the full plan for each query")
		repeat  = flag.Int("repeat", 5, "how many times to run each query when timing it")
	)
	flag.Parse()

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "database-url is required")
		os.Exit(2)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fail(err)
	}
	defer pool.Close()

	if *load {
		fmt.Printf("loading %d sellers, %d listings, %d events\n",
			queries.Sellers, queries.Listings, queries.Events)
		start := time.Now()
		if err := queries.Load(ctx, pool); err != nil {
			fail(err)
		}
		fmt.Printf("loaded in %s\n\n", time.Since(start).Round(time.Millisecond))
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "query\tstate\trows read\tdiscarded\tp50\tplan")

	for _, pair := range queries.All() {
		// Before: no fix indexes at all.
		if err := queries.DropIndexes(ctx, pool); err != nil {
			fail(err)
		}
		slowPlan, slowTime := measure(ctx, pool, pair.Slow, pair.SlowArgs, *repeat)

		// After: the indexes, and the rewritten query where there is one.
		if err := queries.AddIndexes(ctx, pool); err != nil {
			fail(err)
		}
		fastPlan, fastTime := measure(ctx, pool, pair.Fast, pair.FastArgs, *repeat)

		row(w, pair.Name, "before", slowPlan, slowTime)
		row(w, "", "after", fastPlan, fastTime)
		fmt.Fprintln(w, "\t\t\t\t\t")

		if *verbose {
			w.Flush()
			fmt.Printf("\n== %s\n%s\n%s\nbefore:\n%s\nafter:\n%s\n",
				pair.Name, pair.Problem, pair.Fix, slowPlan, fastPlan)
		}
	}
	w.Flush()
}

func row(w *tabwriter.Writer, name, state string, p *plan.Plan, d time.Duration) {
	top := p.Root.NodeType
	if len(p.SeqScans()) > 0 {
		top += " (seq scan)"
	} else if idx := p.Indexes(); len(idx) > 0 {
		top = idx[0]
	}
	fmt.Fprintf(w, "%s\t%s\t%.0f\t%.0f\t%s\t%s\n",
		name, state, p.RowsRead(), p.RowsDiscarded(), d.Round(10*time.Microsecond), top)
}

// measure runs the query a few times and takes the median, because the
// first run pays for a cold cache and reporting that as the number would
// flatter every "after" in this table.
func measure(ctx context.Context, pool *pgxpool.Pool, q string, args []any, n int) (*plan.Plan, time.Duration) {
	var p *plan.Plan
	times := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		got, err := queries.Explain(ctx, pool, q, args...)
		if err != nil {
			fail(err)
		}
		times = append(times, time.Duration(got.ExecutionTime*float64(time.Millisecond)))
		_ = start
		p = got
	}
	for i := 1; i < len(times); i++ {
		for j := i; j > 0 && times[j] < times[j-1]; j-- {
			times[j], times[j-1] = times[j-1], times[j]
		}
	}
	return p, times[len(times)/2]
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "planbench:", err)
	os.Exit(1)
}

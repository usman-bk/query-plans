# query-plans

_Built with AI assistance._

Four queries that are fine on ten thousand rows and not fine on two hundred
thousand, with the fix for each, and tests that assert the **plan** rather
than the time.

Go 1.23, Postgres.

```
internal/plan/plan.go      reading EXPLAIN (ANALYZE, FORMAT JSON)
internal/plan/require.go   the assertions, and failures you can act on
internal/queries/          the four pairs, the schema, the fixture
cmd/planbench/
```

## Why not just time it

The usual way to stop a query regressing is to measure how long it takes,
and it does not work. A laptop, a shared runner and the production box give
three different numbers, so the threshold is either so loose it catches
nothing or so tight it fails on a busy afternoon, and either way somebody
deletes the test.

Worse, timing cannot see the problem that matters. A query that is fast on
ten thousand rows and quadratic above a million passes every run until the
day it does not.

A sequential scan over a million rows is a sequential scan whatever the
machine is doing, and it is visible long before anybody notices the latency.
So the tests assert things like this:

    plan.Require(p,
        plan.NoSeqScanOn("listings"),
        plan.UsesIndex("listings_browse_idx"),
        plan.NoSort(),
        plan.MaxRowsRead(50),
        plan.MaxRowsDiscarded(0),
    )

Every requirement is checked, not just the first, because fixing one at a
time against a query that breaks three is three round trips through CI. The
failure prints the whole plan, because a test that says only "the plan
changed" is one people switch off.

## The four, measured

200,000 listings, 400,000 events, 2,000 sellers. Rows touched is reads plus
rows read and then discarded by a filter, counted once per loop.

| | rows touched before | after | p50 before | after |
|---|---|---|---|---|
| browse | 200,000 | 20 | 7.8 ms | 0.10 ms |
| email lookup | 2,000 | 1 | 0.36 ms | 0.01 ms |
| deep pagination | 200,000 | 20 | 24.0 ms | 0.01 ms |
| events per listing | 400,164 | 392 | 116 ms | 0.18 ms |

Rows is the number the tests assert on. Time is in the table because it is
what anybody asks about first, and it is the number not to write a test
against.

### browse: the column order in the index is the whole fix

    WHERE status = $1 AND category_id = $2 ORDER BY price_cents LIMIT 20

Without an index: a sequential scan reads 200,000 rows, throws away 198,990,
sorts the survivors and returns twenty.

    CREATE INDEX listings_browse_idx
        ON listings (status, category_id, price_cents, id)
        WHERE status = 'available';

The filter is served by the leading columns and the rows come back already
in price order, so there is **no sort node at all** and exactly twenty rows
are read. The same three columns as `(price_cents, status, category_id)`
would be useless for the filter. An index on `(status)` alone still sorts
every match.

Partial on `available`, because that is the only status anybody browses and
sold rows are most of the table.

The query text does not change. That is worth noticing: most of these are
fixed without touching the application.

### email lookup: an index on the column is not an index on the expression

    WHERE lower(email) = $1

An index on `(email)` is not used, because the indexed value and the
compared value are different expressions. A test creates that index and
asserts the plan still contains a sequential scan, so the claim is checked
rather than asserted in a comment.

    CREATE INDEX sellers_lower_email_idx ON sellers (lower(email));

Storing the address already folded is better where it is possible, and is a
migration rather than an index.

### deep pagination: OFFSET reads everything it skips

    ORDER BY created_at DESC, id DESC LIMIT 20 OFFSET 10000

reads 10,020 rows to return 20, and page 500 costs five hundred times page
one. No index fixes this; the query has to change.

    WHERE (created_at, id) < ($1, $2)
    ORDER BY created_at DESC, id DESC LIMIT 20

Every page costs the same. The row comparison is one index seek, not three
OR'd conditions, and the index is declared `DESC` to match so it is read
forwards rather than backwards.

### events per listing: the textbook fix made it 600 times slower

This is the one worth reading.

The starting point is a correlated subquery, one count per listing, and the
standard advice is to replace it with a join. Written the obvious way:

    LEFT JOIN (
      SELECT listing_id, count(*) AS views
        FROM events WHERE kind = 'view' GROUP BY listing_id
    ) e ON e.listing_id = l.id
    WHERE l.seller_id = $1

the subquery aggregates **the whole events table** before the filter on
seller narrows anything. 400,164 rows touched, 116 ms, to answer a question
about one seller's hundred listings.

    LEFT JOIN events e ON e.listing_id = l.id AND e.kind = 'view'
    WHERE l.seller_id = $1
    GROUP BY l.id, l.title

392 rows, 0.18 ms. The filter reaches the events through the join, so only
the relevant ones are read, and `(listing_id, kind)` serves both halves of
the join condition.

A `LEFT JOIN LATERAL` measures the same. The point is not which of the two
to pick, it is that "replace the subquery with a join" is not advice, it is
a direction, and the measurement decides.

## Running the benchmark

    createdb plans
    DATABASE_URL=postgres://localhost/plans?sslmode=disable \
      go run ./cmd/planbench -load

    DATABASE_URL=... go run ./cmd/planbench -v     # with the full plans

`-load` builds the fixture, which takes about five and a half seconds
through COPY. The same 600,000 rows one INSERT at a time is minutes, and
that difference decides whether the suite is something anybody runs.

## Test layout

    go test ./...                      17 cases, no database
    DATABASE_URL=... go test ./...     30, adding the Postgres suite

The plan parser and the requirements are tested against literal EXPLAIN
output committed in the test file, so they need no database. The query
tests need one, because what they check is what this Postgres chooses to do.

The requirements are also run against the **unfixed** query and expected to
fail. A requirement that passes whatever it is given is not checking
anything, and that is a thing to test rather than to assume.

## Caveats

The fixture is one shape of data. A planner choice depends on the
distribution as much as the size, and an index that wins on this fixture can
lose on a table where one category holds nine tenths of the rows.

No `pg_stat_statements`, so there is nothing here about finding which
queries are worth looking at. This is the part after that.

Plans change between major versions of Postgres. These assertions are
written against 16, and a version bump is exactly the sort of thing they are
supposed to catch.

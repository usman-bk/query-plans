// Package queries holds the four query pairs this repository is about, and
// the harness that builds a database big enough for the planner to make
// realistic choices.
package queries

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/usmanvirk/query-plans/internal/plan"
)

//go:embed schema.sql
var Schema string

//go:embed indexes.sql
var Indexes string

// Rows is how much data the harness builds.
//
// 200,000 listings is enough that a sequential scan is clearly wrong and a
// missing index is clearly visible in the plan, and small enough that the
// whole thing loads in a few seconds on a laptop or a CI runner. Below about
// 50,000 the planner starts choosing a sequential scan because it genuinely
// is cheapest, and then the tests are measuring the fixture rather than the
// query.
const (
	Sellers  = 2000
	Listings = 200000
	Events   = 400000
)

// Pair is one query, written the way it usually gets written first, and
// then the way it should be.
type Pair struct {
	Name string
	// Problem is what is wrong with Slow, in one line.
	Problem string
	// Fix names the index or the rewrite.
	Fix string

	Slow     string
	Fast     string
	SlowArgs []any
	FastArgs []any
}

// All is the set. Each one is a mistake that is easy to make, survives code
// review, and is invisible until the table grows.
func All() []Pair {
	return []Pair{
		{
			Name:    "browse",
			Problem: "filters on two columns and orders by a third, with no index covering the combination, so every matching row is read and then sorted",
			Fix:     "a composite index on (status, category_id, price_cents) that serves the filter and returns the rows already in order",
			Slow: `SELECT id, title, price_cents
			         FROM listings
			        WHERE status = $1 AND category_id = $2
			        ORDER BY price_cents
			        LIMIT 20`,
			SlowArgs: []any{"available", 7},
			// The same query. The fix is the index, not the text, which is
			// the point worth making: most of these are fixed without
			// touching the application at all.
			Fast: `SELECT id, title, price_cents
			         FROM listings
			        WHERE status = $1 AND category_id = $2
			        ORDER BY price_cents
			        LIMIT 20`,
			FastArgs: []any{"available", 7},
		},
		{
			Name:     "email lookup",
			Problem:  "lower(email) is a different expression from email, so an index on the column cannot be used and the whole table is read",
			Fix:      "an index on the expression, lower(email)",
			Slow:     `SELECT id, name FROM sellers WHERE lower(email) = $1`,
			SlowArgs: []any{"seller1234@example.com"},
			Fast:     `SELECT id, name FROM sellers WHERE lower(email) = $1`,
			FastArgs: []any{"seller1234@example.com"},
		},
		{
			Name:    "deep pagination",
			Problem: "OFFSET 10000 reads ten thousand rows and throws them away before returning twenty, and page 500 costs five hundred times page one",
			Fix:     "keyset pagination: ask for the rows after the last one seen, which costs the same on every page",
			Slow: `SELECT id, title, created_at
			         FROM listings
			        ORDER BY created_at DESC, id DESC
			        LIMIT 20 OFFSET 10000`,
			Fast: `SELECT id, title, created_at
			         FROM listings
			        WHERE (created_at, id) < ($1, $2)
			        ORDER BY created_at DESC, id DESC
			        LIMIT 20`,
			// The cursor a client would send back, standing in for the last
			// row of page 500.
			FastArgs: []any{mustTime("2026-01-01T00:00:00Z"), int64(100000)},
		},
		{
			Name: "events per listing",
			Problem: "the textbook fix for an N+1: replace the correlated subquery with a join. " +
				"The subquery aggregates the whole events table before the join narrows it to one seller, " +
				"so a page that touched a hundred listings now groups four hundred thousand rows",
			Fix: "join the events directly and group once, so the filter on seller reaches the events " +
				"through the join and only the relevant rows are read",
			Slow: `SELECT l.id, l.title, coalesce(e.views, 0) AS views
			         FROM listings l
			         LEFT JOIN (
			               SELECT listing_id, count(*) AS views
			                 FROM events
			                WHERE kind = 'view'
			                GROUP BY listing_id
			         ) e ON e.listing_id = l.id
			        WHERE l.seller_id = $1`,
			SlowArgs: []any{int64(42)},
			Fast: `SELECT l.id, l.title, count(e.id) AS views
			         FROM listings l
			         LEFT JOIN events e ON e.listing_id = l.id AND e.kind = 'view'
			        WHERE l.seller_id = $1
			        GROUP BY l.id, l.title`,
			FastArgs: []any{int64(42)},
		},
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Explain runs EXPLAIN on a query and parses the result.
//
// ANALYZE, so the plan carries what actually happened rather than what the
// planner guessed. An estimate of 20 rows against an actual of 200,000 is
// itself one of the more useful things to see, and only ANALYZE shows it.
func Explain(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) (*plan.Plan, error) {
	var raw []byte
	err := pool.QueryRow(ctx,
		"EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query, args...).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("explaining %q: %w", firstLine(query), err)
	}
	return plan.Parse(query, raw)
}

// ExplainOnly runs EXPLAIN without ANALYZE, for a statement that should not
// actually be executed.
func ExplainOnly(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) (*plan.Plan, error) {
	var raw []byte
	err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+query, args...).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("explaining %q: %w", firstLine(query), err)
	}
	return plan.Parse(query, raw)
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

package queries

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Load builds the fixture: the schema, the rows, and the statistics.
//
// Loaded with COPY rather than INSERT. 600,000 rows one statement at a time
// is minutes; through the copy protocol it is a couple of seconds, and the
// difference decides whether the test suite is something anybody runs.
func Load(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, Schema); err != nil {
		return fmt.Errorf("schema: %w", err)
	}

	rng := rand.New(rand.NewSource(20261023))

	sellers := make([][]any, 0, Sellers)
	for i := 0; i < Sellers; i++ {
		sellers = append(sellers, []any{
			int64(i),
			fmt.Sprintf("Seller %d", i),
			// Mixed case on purpose, which is why the lookup has to fold it.
			fmt.Sprintf("Seller%d@Example.com", i),
			[]string{"DE", "NL", "ES", "SE", "FR"}[i%5],
			float32(3.5 + rng.Float64()*1.5),
			rng.Intn(5000),
		})
	}
	if err := copyIn(ctx, pool, "sellers",
		[]string{"id", "name", "email", "country", "rating", "rating_count"}, sellers); err != nil {
		return err
	}

	// Most listings are sold. That is both true of a marketplace and the
	// reason the partial index on 'available' is worth having.
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	listings := make([][]any, 0, Listings)
	for i := 0; i < Listings; i++ {
		status := "sold"
		switch {
		case i%5 == 0:
			status = "available"
		case i%17 == 0:
			status = "reserved"
		}
		listings = append(listings, []any{
			int64(i),
			int64(rng.Intn(Sellers)),
			rng.Intn(40),
			status,
			int16(1 + rng.Intn(3)),
			int64(5000 + rng.Intn(150000)),
			fmt.Sprintf("Refurbished device %d", i),
			base.Add(time.Duration(i) * time.Minute),
		})
	}
	if err := copyIn(ctx, pool, "listings",
		[]string{"id", "seller_id", "category_id", "status", "grade", "price_cents", "title", "created_at"},
		listings); err != nil {
		return err
	}

	events := make([][]any, 0, Events)
	for i := 0; i < Events; i++ {
		kind := "view"
		if i%4 == 0 {
			kind = "watch"
		}
		events = append(events, []any{
			int64(rng.Intn(Listings)),
			kind,
			base.Add(time.Duration(i) * time.Second),
		})
	}
	if err := copyIn(ctx, pool, "events",
		[]string{"listing_id", "kind", "at"}, events); err != nil {
		return err
	}

	// Without this the planner is working from defaults and chooses badly
	// for reasons that have nothing to do with the query, which would make
	// every assertion below meaningless.
	if _, err := pool.Exec(ctx, "ANALYZE sellers, listings, events"); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	return nil
}

// AddIndexes applies the fixes and refreshes the statistics.
func AddIndexes(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, Indexes); err != nil {
		return fmt.Errorf("indexes: %w", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE sellers, listings, events"); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	return nil
}

// DropIndexes removes them again, so the before and after run against the
// same rows rather than against two differently generated fixtures.
func DropIndexes(ctx context.Context, pool *pgxpool.Pool) error {
	for _, name := range []string{
		"listings_browse_idx", "sellers_lower_email_idx",
		"listings_created_at_id_idx", "events_listing_kind_idx",
	} {
		if _, err := pool.Exec(ctx, "DROP INDEX IF EXISTS "+name); err != nil {
			return fmt.Errorf("dropping %s: %w", name, err)
		}
	}
	if _, err := pool.Exec(ctx, "ANALYZE sellers, listings, events"); err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	return nil
}

func copyIn(ctx context.Context, pool *pgxpool.Pool, table string, cols []string, rows [][]any) error {
	_, err := pool.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("loading %s: %w", table, err)
	}
	return nil
}

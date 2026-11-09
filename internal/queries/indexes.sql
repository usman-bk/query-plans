-- One index per fix, each named after the query it is for.

-- 1. Browse: filter on status and category, order by price, take 20.
--
-- Column order matters and is the whole lesson. (status, category_id,
-- price_cents) serves the filter from the leading columns and then hands
-- the rows back already in price order, so there is no sort at all. The
-- same three columns as (price_cents, status, category_id) would be useless
-- for the filter, and an index on (status) alone still leaves a sort of
-- every matching row.
--
-- Partial on 'available', because that is the only status anybody browses
-- and the sold rows are the majority of the table. A partial index here is
-- about a fifth of the size of the full one.
CREATE INDEX IF NOT EXISTS listings_browse_idx
    ON listings (status, category_id, price_cents, id)
    WHERE status = 'available';

-- 2. Login: a lookup on a case folded email.
--
-- An index on (email) is not used by WHERE lower(email) = $1, because the
-- indexed value and the compared value are different expressions. The index
-- has to be on the expression. The alternative, storing email already
-- lowercased, is better where it is possible and is a migration rather than
-- an index.
CREATE INDEX IF NOT EXISTS sellers_lower_email_idx ON sellers (lower(email));

-- 3. Keyset pagination: everything after a given (created_at, id).
--
-- Descending, matching the query's ORDER BY, so the index is read forwards
-- rather than backwards. Postgres can scan an index backwards and it gives
-- up some read ahead to do it.
CREATE INDEX IF NOT EXISTS listings_created_at_id_idx
    ON listings (created_at DESC, id DESC);

-- 4. Counting events per listing for one seller.
--
-- (listing_id, kind) rather than (listing_id) alone, so the kind filter is
-- satisfied by the index instead of by reading the row and discarding most
-- of them.
CREATE INDEX IF NOT EXISTS events_listing_kind_idx ON events (listing_id, kind);

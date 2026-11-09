-- The tables are a cut down marketplace: listings a customer browses, the
-- sellers behind them, and the events the listing pages generate. Big enough
-- that the planner makes realistic choices, small enough to build in a few
-- seconds.

DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS listings;
DROP TABLE IF EXISTS sellers;

CREATE TABLE sellers (
    id            bigint PRIMARY KEY,
    name          text   NOT NULL,
    email         text   NOT NULL,
    country       char(2) NOT NULL,
    rating        real   NOT NULL,
    rating_count  int    NOT NULL
);

CREATE TABLE listings (
    id          bigint      PRIMARY KEY,
    seller_id   bigint      NOT NULL REFERENCES sellers(id),
    category_id int         NOT NULL,
    status      text        NOT NULL,
    grade       smallint    NOT NULL,
    price_cents bigint      NOT NULL,
    title       text        NOT NULL,
    created_at  timestamptz NOT NULL
);

CREATE TABLE events (
    id         bigserial   PRIMARY KEY,
    listing_id bigint      NOT NULL REFERENCES listings(id),
    kind       text        NOT NULL,
    at         timestamptz NOT NULL
);

-- Only the primary keys and the foreign key indexes exist at this point.
-- Everything the queries need is added by indexes.sql, so the before and
-- after can be measured against the same data.
CREATE INDEX listings_seller_id_idx ON listings (seller_id);
CREATE INDEX events_listing_id_idx ON events (listing_id);

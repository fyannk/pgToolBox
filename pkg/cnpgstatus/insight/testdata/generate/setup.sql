\set ON_ERROR_STOP 1
CREATE ROLE cnpg_metrics_exporter LOGIN IN ROLE pg_monitor;
CREATE DATABASE app;
CREATE DATABASE billing;
\c app
CREATE EXTENSION pg_stat_statements;
-- P1.1 threshold: 10000 analyzed rows, scale 0.2, base 50 -> 2050 (the
-- old left-associative expression gave (50 + 0.2) * 10000 = 502000).
CREATE TABLE accounts (id int PRIMARY KEY, balance int) WITH (autovacuum_enabled = false);
INSERT INTO accounts SELECT g, 0 FROM generate_series(1, 10000) g;
ANALYZE accounts;
UPDATE accounts SET balance = 1 WHERE id <= 3000;
-- PG18 table-level cap: min(500, 2050) = 500. Created on 14 too, without the option.
CREATE TABLE capped (id int, v int) WITH (autovacuum_enabled = false);
INSERT INTO capped SELECT g, 0 FROM generate_series(1, 10000) g;
ANALYZE capped;
UPDATE capped SET v = 1 WHERE id <= 2500;
SELECT current_setting('server_version_num')::int >= 180000 AS pg18 \gset
\if :pg18
ALTER TABLE capped SET (autovacuum_vacuum_max_threshold = 500);
\endif
-- Never analyzed: reltuples -1 -> threshold unknown.
CREATE TABLE fresh (id int) WITH (autovacuum_enabled = false, toast.autovacuum_enabled = false);
INSERT INTO fresh SELECT g FROM generate_series(1, 2600) g;
DELETE FROM fresh WHERE id <= 2400;
-- TOAST: dead chunks in documents' TOAST table.
CREATE TABLE documents (id int PRIMARY KEY, body text) WITH (autovacuum_enabled = false, toast.autovacuum_enabled = false);
ALTER TABLE documents ALTER COLUMN body SET STORAGE EXTERNAL;
INSERT INTO documents SELECT g, repeat(md5(g::text), 200) FROM generate_series(1, 400) g;
DELETE FROM documents WHERE id <= 300;

-- P1.2 duplicates.
CREATE TABLE customers (id int PRIMARY KEY, email text);
ALTER TABLE customers ADD CONSTRAINT customers_email_key UNIQUE (email);
ALTER TABLE customers ADD CONSTRAINT customers_email_key2 UNIQUE (email);  -- both back constraints: reported, not droppable
CREATE UNIQUE INDEX customers_id_copy ON customers (id);                   -- copy of the PK: droppable, keep the PK
CREATE TABLE orders (id bigint PRIMARY KEY, customer_id int REFERENCES customers (id), note text, status text,
                     created timestamptz DEFAULT now());
CREATE INDEX orders_customer_idx ON orders (customer_id);                  -- serves the FK: fk_support
CREATE INDEX orders_customer_idx2 ON orders (customer_id);                 -- duplicate
CREATE INDEX orders_lower_note ON orders (lower(note));
CREATE INDEX orders_lower_note2 ON orders (lower(note));                   -- duplicate expression, other :location
CREATE INDEX orders_upper_note ON orders (upper(note));                    -- not a duplicate
CREATE INDEX orders_open ON orders (status) WHERE status = 'open';
CREATE INDEX orders_open2 ON orders (status) WHERE status = 'open';        -- duplicate predicate
CREATE INDEX orders_closed ON orders (status) WHERE status = 'closed';     -- other predicate
CREATE INDEX orders_created ON orders (created);
CREATE INDEX orders_created_desc ON orders (created DESC);                 -- other indoption
CREATE INDEX orders_note_c ON orders (note COLLATE "C");
CREATE INDEX orders_note_pattern ON orders (note text_pattern_ops);        -- other opclass
CREATE TABLE ledger (id int, ref text);
ALTER TABLE ledger ADD CONSTRAINT ledger_ref_key UNIQUE (ref) DEFERRABLE INITIALLY DEFERRED;
CREATE UNIQUE INDEX ledger_ref_idx ON ledger (ref);                        -- immediate vs deferrable: not a duplicate
CREATE TABLE products (sku text NOT NULL);
CREATE UNIQUE INDEX products_sku_a ON products (sku);
CREATE UNIQUE INDEX products_sku_b ON products (sku);
CREATE TABLE lines (sku text REFERENCES products (sku));                   -- one of the two is the FK's target: keep it
CREATE TABLE rooms (r circle, EXCLUDE USING gist (r WITH &&));
CREATE INDEX rooms_r ON rooms USING gist (r);                              -- duplicate of the exclusion index
CREATE TABLE events (id int, at date) PARTITION BY RANGE (at);
CREATE TABLE events_2026 PARTITION OF events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
CREATE INDEX events_at ON events (at);                                     -- partitioned: never "unused" itself
CREATE TABLE broken (a int);
INSERT INTO broken VALUES (1), (1);
SELECT current_setting('server_version_num')::int >= 150000 AS pg15 \gset
\if :pg15
CREATE TABLE tags (name text);
CREATE UNIQUE INDEX tags_name_nnd ON tags (name) NULLS NOT DISTINCT;
CREATE UNIQUE INDEX tags_name ON tags (name);                              -- NULLS DISTINCT: not a duplicate
\endif
-- The plain unique index is older (lower oid): the constraint's index is kept all the same.
CREATE TABLE vouchers (code text NOT NULL);
CREATE UNIQUE INDEX vouchers_code_first ON vouchers (code);
ALTER TABLE vouchers ADD CONSTRAINT vouchers_code_key UNIQUE (code);
ANALYZE vouchers;
CREATE TABLE replid (id int NOT NULL);
CREATE UNIQUE INDEX replid_id ON replid (id);
CREATE UNIQUE INDEX replid_id2 ON replid (id);
ALTER TABLE replid REPLICA IDENTITY USING INDEX replid_id2;                -- keep the replica identity
ANALYZE customers, orders, ledger, products, lines, rooms, events, replid;
SELECT count(*) FROM orders WHERE customer_id = 1;
SELECT count(*) FROM accounts WHERE id = 1;
SELECT pg_create_logical_replication_slot('fixture_slot', 'pgoutput');
\c billing
CREATE EXTENSION pg_stat_statements;
CREATE TABLE invoices (id int PRIMARY KEY, total numeric, issued date);
CREATE INDEX invoices_issued ON invoices (issued);
INSERT INTO invoices SELECT g, g, date '2026-01-01' + (g % 300) FROM generate_series(1, 5000) g;
ANALYZE invoices;

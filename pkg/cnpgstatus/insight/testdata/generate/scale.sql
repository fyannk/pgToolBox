CREATE DATABASE many;
\c many
DO $$
BEGIN
  FOR i IN 1..20000 LOOP
    EXECUTE format('CREATE TABLE t%s (id int PRIMARY KEY, a int, b text, c int)', i);
    EXECUTE format('CREATE INDEX t%s_a ON t%s (a)', i, i);
    EXECUTE format('CREATE INDEX t%s_lb ON t%s (lower(b)) WHERE c > 0', i, i);
    EXECUTE format('CREATE INDEX t%s_ac ON t%s (a, c)', i, i);
    IF i % 50 = 0 THEN
      EXECUTE format('CREATE INDEX t%s_lb2 ON t%s (lower(b)) WHERE c > 0', i, i);
    END IF;
    IF i % 1000 = 0 THEN COMMIT; END IF;
  END LOOP;
END $$;

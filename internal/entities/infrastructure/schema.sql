-- Generation-only shape of PostgreSQL's built-in connection TLS view.
-- Never embedded or applied as an application migration.
CREATE VIEW pg_stat_ssl AS SELECT 0::integer AS pid, false::boolean AS ssl;

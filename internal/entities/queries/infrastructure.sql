-- name: DatabaseVersion :one
SELECT current_setting('server_version_num')::integer AS version;

-- name: DatabaseIdentity :one
SELECT cardinality(current_schemas(false))::integer AS schema_count,
       coalesce(current_schema(), '')::text AS schema_name,
       current_user::text AS role_name;

-- name: DatabaseFacts :one
SELECT current_user::text AS role_name, current_database()::text AS database_name,
       current_setting('server_version_num')::integer AS server_version,
       coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false)::boolean AS tls,
       has_database_privilege(current_user, current_database(), 'CREATE')::boolean AS can_create_database,
       coalesce(has_schema_privilege(current_user, current_schema(), 'CREATE'), false)::boolean AS can_create_schema;

-- name: ApplicationMigrationCatalog :one
SELECT cardinality(current_schemas(false))::integer AS schema_count,
       coalesce(current_schema(), '')::text AS schema_name,
       current_user::text AS role_name,
       (to_regclass('eregion_schema_migrations') IS NOT NULL)::boolean AS legacy_ledger,
       EXISTS (SELECT 1 FROM pg_catalog.pg_tables WHERE
         (current_schema() IS NULL OR schemaname = current_schema()) AND tablename = 'goose_db_version')::boolean AS ledger_exists;

-- name: UnexpectedMigrationObjects :one
SELECT EXISTS (
  SELECT 1 FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = ANY(current_schemas(false))
  AND c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S', 'i', 'I')
  AND ((sqlc.arg(application_pending)::boolean AND c.relname LIKE 'river\_%' ESCAPE '\') OR
    (c.relname LIKE 'eregion\_%' ESCAPE '\' AND NOT (
      (c.relkind IN ('r', 'p') AND c.relname IN (SELECT jsonb_array_elements_text(sqlc.arg(tables)::text::jsonb)))
      OR (c.relkind IN ('i', 'I') AND EXISTS (
        SELECT 1 FROM pg_catalog.pg_index i
        JOIN pg_catalog.pg_class owner ON owner.oid = i.indrelid
        WHERE i.indexrelid = c.oid AND owner.relname IN (SELECT jsonb_array_elements_text(sqlc.arg(tables)::text::jsonb))
        AND (c.relname || ':' || owner.relname IN (SELECT jsonb_array_elements_text(sqlc.arg(indexes)::text::jsonb)) OR EXISTS (
          SELECT 1 FROM pg_catalog.pg_constraint con
          WHERE con.conindid = c.oid AND con.conrelid = owner.oid AND con.contype IN ('p', 'u')
        ))
      ))
    )))
  UNION ALL
  SELECT 1 FROM pg_catalog.pg_type t
  JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
  WHERE n.nspname = ANY(current_schemas(false))
  AND ((sqlc.arg(application_pending)::boolean AND t.typname LIKE 'river\_%' ESCAPE '\') OR
    (t.typname LIKE 'eregion\_%' ESCAPE '\' AND NOT (
      t.typtype = 'c' AND t.typname IN (SELECT jsonb_array_elements_text(sqlc.arg(tables)::text::jsonb)) AND EXISTS (
        SELECT 1 FROM pg_catalog.pg_class owner
        WHERE owner.oid = t.typrelid AND owner.relkind IN ('r', 'p') AND owner.relname = t.typname
      )
    )))
  UNION ALL
  SELECT 1 FROM pg_catalog.pg_proc p
  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = ANY(current_schemas(false))
  AND (p.proname LIKE 'eregion\_%' ESCAPE '\' OR (sqlc.arg(application_pending)::boolean AND p.proname LIKE 'river\_%' ESCAPE '\'))
  UNION ALL
  SELECT 1 FROM pg_catalog.pg_trigger tr
  JOIN pg_catalog.pg_class owner ON owner.oid = tr.tgrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = owner.relnamespace
  WHERE n.nspname = ANY(current_schemas(false)) AND NOT tr.tgisinternal
  AND (tr.tgname LIKE 'eregion\_%' ESCAPE '\' OR owner.relname IN (SELECT jsonb_array_elements_text(sqlc.arg(tables)::text::jsonb)))
)::boolean AS unexpected;

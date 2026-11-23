# Disposable PostgreSQL 18 / PgBouncer integration fixture

This fixture uses trust authentication and is **only for an isolated local
test**. It creates its own Docker network and containers, publishes only the
two PgBouncer ports to loopback, and does not publish PostgreSQL. It does not
model Vaire's authentication or TLS. Its transaction-pool settings reproduce
Vaire's relevant `max_prepared_statements`, `server_reset_query` and ignored
startup parameters.

With Docker access and the repository root as the current directory:

```sh
docker network create eregion-pgbouncer-test
docker run -d --name eregion-pgbouncer-test-pg \
  --network eregion-pgbouncer-test --network-alias postgres \
  -e POSTGRES_HOST_AUTH_METHOD=trust -e POSTGRES_DB=eregion postgres:18-alpine
docker run -d --name eregion-pgbouncer-test-tx \
  --network eregion-pgbouncer-test -p 127.0.0.1::6432 \
  -v "$PWD/testdata/pgbouncer-transaction/transaction.ini:/etc/pgbouncer/pgbouncer.ini:ro" \
  -v "$PWD/testdata/pgbouncer-transaction/userlist.txt:/etc/pgbouncer/userlist.txt:ro" \
  --entrypoint /usr/bin/pgbouncer \
  edoburu/pgbouncer@sha256:8638aa563607256d3c7440437534195f1066a92b68015d89ae9d6b935b444a6d \
  /etc/pgbouncer/pgbouncer.ini
docker run -d --name eregion-pgbouncer-test-session \
  --network eregion-pgbouncer-test -p 127.0.0.1::6432 \
  -v "$PWD/testdata/pgbouncer-transaction/session.ini:/etc/pgbouncer/pgbouncer.ini:ro" \
  -v "$PWD/testdata/pgbouncer-transaction/userlist.txt:/etc/pgbouncer/userlist.txt:ro" \
  --entrypoint /usr/bin/pgbouncer \
  edoburu/pgbouncer@sha256:8638aa563607256d3c7440437534195f1066a92b68015d89ae9d6b935b444a6d \
  /etc/pgbouncer/pgbouncer.ini
```

Wait until `docker exec eregion-pgbouncer-test-pg pg_isready -U postgres -d eregion`
succeeds, then create the two roles and grants:

```sh
docker exec -i eregion-pgbouncer-test-pg psql -U postgres -d eregion -v ON_ERROR_STOP=1 <<'SQL'
CREATE ROLE eregion_migrate LOGIN CREATEDB;
CREATE ROLE eregion_app LOGIN;
GRANT CONNECT ON DATABASE eregion TO eregion_migrate, eregion_app;
GRANT USAGE, CREATE ON SCHEMA public TO eregion_migrate;
GRANT USAGE ON SCHEMA public TO eregion_app;
ALTER DEFAULT PRIVILEGES FOR ROLE eregion_migrate IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO eregion_app;
ALTER DEFAULT PRIVILEGES FOR ROLE eregion_migrate IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO eregion_app;
SQL
```

The test migrates the fresh database through the session pool, then uses the
unchanged transaction URL for readiness, backend reassignment on one client
connection, durable target claim lifecycle and contention (including backend
reassignment while a claim is held), runtime startup, submission and River completion. It writes data
and refuses a database with existing migration ledgers. The names and ports
below are for this disposable fixture only.

```sh
session_port=$(docker port eregion-pgbouncer-test-session 6432/tcp | sed 's/.*://')
transaction_port=$(docker port eregion-pgbouncer-test-tx 6432/tcp | sed 's/.*://')
EREGION_TEST_PGBOUNCER_MIGRATION_URL="postgres://eregion_migrate@127.0.0.1:${session_port}/eregion?sslmode=disable" \
EREGION_TEST_PGBOUNCER_RUNTIME_URL="postgres://eregion_app@127.0.0.1:${transaction_port}/eregion?sslmode=disable" \
  go test ./internal/controlplane -run '^TestIntegrationPgBouncerTransactionRuntime$' -count=1 -v
```

To repeat, remove and recreate the fixture. Cleanup:

```sh
docker rm -f eregion-pgbouncer-test-session eregion-pgbouncer-test-tx eregion-pgbouncer-test-pg
docker network rm eregion-pgbouncer-test
```

With `EREGION_TEST_PGBOUNCER_DOCKER=1` on an isolated Docker daemon, this test
also exercises resource ownership and BuildStop contention. It does not prove
that every runtime path is safe through transaction pooling. Migrations still
require a direct or session-pooled URL.

The durable claims do not exclude old binaries that acquire session locks.
Use a fresh installation for this fixture. A live cutover must exclude old
workers and upload entry points and account for their external effects before
starting the new writers; do not assume mixed-version safety.

Vaire ignores `search_path` and `options` startup
parameters on that pool; readiness still requires exactly one existing
installation schema, so URL search-path parameters cannot establish it there.

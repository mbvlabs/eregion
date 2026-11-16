# Eregion

## Testing

We don't do unit tests, and focus on integration tests with one clear boundary:
no simulated server, Docker/Pack command scripts, or node-dispatcher emulation

We test code across the CLI, API, application services, background workers, and PostgreSQL.

Always use a containerised DB for integration test.

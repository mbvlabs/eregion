// Package resources owns PostgreSQL application resources and their logical
// backups. It does not own the control-plane database, River, API handlers, or
// command-line presentation.
//
// A Service is configured for one explicit application host and one injectable
// Runner. Every external program is started with an argument list through
// exec.CommandContext; no request value is ever interpreted by a shell. Service
// results and backup manifests contain credential references and filesystem
// paths, never credential plaintext. The password itself lives only in a
// protected file that the database container mounts read-only.
//
// Plan returns a non-mutating description, including the exact confirmation
// phrase for destructive work. Execute revalidates the request and rechecks the
// confirmation; a plan is not an authorization token. A control-plane database
// is refused at both the Plan and Execute boundaries and cannot be destroyed or
// replaced through the ordinary application-resource path.
//
// Backups are pg_dump custom-format archives in a dedicated directory. A
// manifest records the SHA-256 digest and the resource identity. Verification
// recomputes the digest and runs pg_restore --list inside the database
// container. Restore verifies the archive, takes and verifies a safety backup,
// restores into a temporary database, and only then performs a rename cutover.
// A durable restore journal makes an interrupted cutover recoverable. A failed
// restore rolls the previous database back and reports whether recovery
// succeeded.
package resources

// Package host implements role-aware, resumable host configuration for
// eregion control-plane, application, and builder nodes.
//
// Bootstrap is deliberately split from deployment: it runs on one host, uses
// the existing engine Check/Apply journal for resumability, and does not need
// a database for progress. The database is only required by the control-plane
// role's migration step, which runs after the local journal has recorded every
// earlier step.
//
// The package never prints a credential. Secret file contents are written with
// mode 0600, secret inputs are represented to the engine only as hashes, and
// shell output goes through the engine's redaction registry.
package host

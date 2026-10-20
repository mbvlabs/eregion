// Package engine adapts eru's Check/Apply execution model for eregion node work.
// It is deliberately independent of PostgreSQL, River, SSH, and WireGuard.
//
// A step is a versioned, idempotent unit. Check reports whether the node already
// matches it; Apply performs the mutation once. The journal records the last
// attempt and its canonical input hash. Locks and the journal are node-local:
// they coordinate processes on one host, not multiple hosts or control planes.
package engine

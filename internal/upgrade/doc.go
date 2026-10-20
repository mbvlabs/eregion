// Package upgrade implements signed release verification, bounded downloads,
// compatibility checks, and atomic operator/node binary replacement with a
// journal and explicit rollback.
//
// The package is deliberately narrower than a release manager: it does not
// restart services, execute a downloaded binary, coordinate live control-plane
// instances, or prove high availability. Apply expects a manifest that the
// caller has already verified with Manifest.Verify.
package upgrade

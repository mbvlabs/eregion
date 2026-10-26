// Package release fetches, verifies, and caches signed eregion releases.
//
// A channel manifest names one version and the exact SHA256 of every supported
// platform binary. The manifest, its sigstore bundle, and the binary are all
// checked before the binary is cached; nothing here installs or runs it.
package release

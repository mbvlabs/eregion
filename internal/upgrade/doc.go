// Package upgrade plans and applies a local self-update of the eregion CLI.
//
// It never upgrades a node: a node runs the CLI's exact build and is moved by
// `eregion apply --auto-upgrade-nodes`, which uses FetchNodeBinary below.
package upgrade

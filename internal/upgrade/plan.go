package upgrade

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/release"
)

var managedPrefixes = []string{
	"/opt/homebrew/Cellar",
	"/usr/local/Cellar",
	"/nix/store",
}

// Version is one installed or target release identity.
type Version struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
}

// Warning is a non-fatal change the operator should know before installing.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Plan is the local upgrade decision before download.
type Plan struct {
	Target   string    `json:"target"`
	From     Version   `json:"from"`
	To       Version   `json:"to"`
	Changed  bool      `json:"changed"`
	Warnings []Warning `json:"warnings,omitempty"`
}

// ResolveSelf returns the real path of the running binary.
func ResolveSelf() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	if managedByPackageManager(resolved) {
		return "", contract.NewFault(
			contract.PreconditionFailed,
			"this binary is managed by a package manager",
			"Upgrade with the package manager that installed eregion.",
		)
	}
	return resolved, nil
}

func managedByPackageManager(path string) bool {
	for _, prefix := range managedPrefixes {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

// SelectChannel returns the logical release channel to follow.
func SelectChannel(flag string) (string, error) {
	return selectChannel(flag, buildinfo.Channel, buildinfo.Version)
}

func selectChannel(flag, channel, version string) (string, error) {
	if strings.TrimSpace(flag) != "" {
		switch flag {
		case release.ChannelEdge, release.ChannelRC, release.ChannelStable:
			return flag, nil
		default:
			return "", contract.NewFault(contract.ValidationFailed, "invalid release channel", "Use edge, rc or stable.")
		}
	}
	if channel == "dev" {
		return "", contract.NewFault(
			contract.InputRequired,
			"upgrade needs --channel",
			"A dev build has no channel to follow. Pass --channel stable, --channel rc or --channel edge.",
		)
	}
	if logical, ok := release.VersionChannel(version); ok {
		return logical, nil
	}
	return channel, nil
}

// SelectVersionChannel selects the logical channel for an explicitly pinned version.
func SelectVersionChannel(flag, version string) (string, error) {
	if flag == "" && version != "" {
		if channel, ok := release.VersionChannel(version); ok {
			return channel, nil
		}
		return "", contract.NewFault(contract.ValidationFailed, "invalid release version", "Pass a valid release version.")
	}
	return SelectChannel(flag)
}

// PreferFinal selects a newer verified final without altering the rc pointer.
func PreferFinal(rc, final release.Manifest) release.Manifest {
	if release.CompareVersions(final.Version, rc.Version) > 0 {
		return final
	}
	return rc
}

// Decide compares the running binary with a verified manifest. requested is
// true when the operator named a version or channel. A default selection never
// moves a stable/tagged release backward; an explicit selection may downgrade
// but warns.
func Decide(from Version, m release.Manifest, requested bool) Plan {
	plan := Plan{
		From: from,
		To:   Version{Version: m.Version, Channel: m.Channel},
	}
	if from.Version == m.Version && from.Channel == m.Channel {
		return plan
	}
	_, fromTagged := release.ParseStable(from.Version)
	_, toTagged := release.ParseStable(m.Version)
	tagged := fromTagged && toTagged
	older := cmpSemver(m.Version, from.Version) < 0
	if tagged && !requested && older {
		plan.To = from
		return plan
	}
	plan.Changed = true
	if tagged && older {
		plan.Warnings = append(plan.Warnings, Warning{Code: "downgrade", Message: "installing an older release"})
	}
	if tagged {
		fromV, okFrom := release.ParseStable(from.Version)
		toV, okTo := release.ParseStable(m.Version)
		if okFrom && okTo && fromV.Major != toV.Major {
			plan.Warnings = append(plan.Warnings, Warning{
				Code:    "major_line_changed",
				Message: fmt.Sprintf("switching major line from %s to %s", fromV.Line(), toV.Line()),
			})
		}
	}
	if from.Channel != "" && from.Channel != "dev" && from.Channel != m.Channel {
		plan.Warnings = append(plan.Warnings, Warning{
			Code:    "channel_changed",
			Message: fmt.Sprintf("switching channel from %s to %s", from.Channel, m.Channel),
		})
	}
	return plan
}

// cmpSemver orders stable versions by semver precedence, so
// 1.0.0-alpha.2 < 1.0.0-alpha.10 < 1.0.0-beta.1 < 1.0.0-rc.1 < 1.0.0.
func cmpSemver(a, b string) int {
	return release.CompareVersions(a, b)
}

// FetchNodeBinary resolves a channel (or an explicit version) manifest and
// returns the verified linux/<arch> binary for it. This is the download half of
// `apply --auto-upgrade-nodes`; it installs nothing and starts no process.
func FetchNodeBinary(ctx context.Context, channel, version, goarch string) (release.Manifest, string, error) {
	if goarch != "amd64" && goarch != "arm64" {
		return release.Manifest{}, "", contract.NewFault(
			contract.ValidationFailed,
			"node releases support amd64 and arm64 only",
			"Pass the node's architecture: amd64 or arm64.",
		)
	}
	if strings.TrimSpace(channel) == "" {
		return release.Manifest{}, "", contract.NewFault(
			contract.InputRequired,
			"node upgrade needs a release channel",
			"Pass the channel the operator CLI follows, or an explicit version.",
		)
	}
	client := &release.Client{}
	manifest, err := client.Resolve(ctx, channel, version)
	if err != nil {
		return release.Manifest{}, "", err
	}
	path, err := client.Fetch(ctx, manifest, "linux", goarch)
	if err != nil {
		return release.Manifest{}, "", err
	}
	return manifest, path, nil
}

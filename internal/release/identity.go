package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	ChannelStable = "stable"
	ChannelEdge   = "edge"
	ChannelRC     = "rc"

	Issuer = "https://token.actions.githubusercontent.com"
)

var edgeVersionPattern = regexp.MustCompile(`^edge-[0-9a-f]{12}$`)

func currentBaseURL() string {
	return buildinfo.ReleaseBaseURL
}

// ChannelRoot is the HTTPS origin for a channel's install.sh and pointer
// manifest. stable is the site root, a major line such as stable-1.x lives
// under /1.x and edge under /edge.
func ChannelRoot(channel string) (string, error) {
	base := strings.TrimRight(currentBaseURL(), "/")
	switch channel {
	case ChannelStable:
		return base, nil
	case ChannelEdge:
		return base + "/edge", nil
	case ChannelRC:
		return base + "/rc", nil
	}
	if major, ok := lineMajor(channel); ok {
		return base + "/" + strconv.Itoa(major) + ".x", nil
	}
	return "", invalidChannel(channel)
}

// SigningChannel maps the internal major partition to its logical channel.
func SigningChannel(channel string) string {
	if _, ok := lineMajor(channel); ok {
		return ChannelStable
	}
	return channel
}

// Identity returns the exact certificate SAN for a channel and version. The
// workflow path and ref are part of the trust decision: an edge binary is
// signed only by a push to master, a stable one only by a v* tag.
func Identity(channel, version string) (string, error) {
	if err := matchVersion(channel, version); err != nil {
		return "", err
	}
	if edgeVersionPattern.MatchString(version) {
		return "https://github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master", nil
	}
	return "https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v" + version, nil
}

func matchVersion(channel, version string) error {
	logical, ok := VersionChannel(version)
	if !ok {
		return fmt.Errorf("release version %q is invalid", version)
	}
	switch channel {
	case ChannelRC, ChannelStable, ChannelEdge:
		if logical != channel {
			return fmt.Errorf("release version %q is not on the %s channel", version, channel)
		}
		return nil
	}
	major, ok := lineMajor(channel)
	if !ok {
		return invalidChannel(channel)
	}
	v, ok := ParseStable(version)
	if !ok {
		return fmt.Errorf("stable release version %q is invalid", version)
	}
	if v.Major != major || logical != ChannelStable {
		return fmt.Errorf("release version %q is not on the %s channel", version, channel)
	}
	return nil
}

func invalidChannel(channel string) error {
	return &contract.Fault{
		Code:    contract.ValidationFailed,
		Message: "release channel " + channel + " is invalid",
		Hint:    "Use stable, rc or edge.",
		Details: map[string]any{"channel": channel},
	}
}

func versionRoot(version string) string {
	return strings.TrimRight(currentBaseURL(), "/") + "/releases/" + version
}

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
	}
	if major, ok := lineMajor(channel); ok {
		return base + "/" + strconv.Itoa(major) + ".x", nil
	}
	return "", invalidChannel(channel)
}

// SigningChannel is the channel a channel's manifests are built and signed
// for: a major line is published by the stable workflow, so its manifests
// say stable.
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
	switch SigningChannel(channel) {
	case ChannelStable:
		return "https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v" + version, nil
	case ChannelEdge:
		return "https://github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master", nil
	default:
		return "", invalidChannel(channel)
	}
}

func matchVersion(channel, version string) error {
	switch channel {
	case ChannelStable:
		if _, ok := ParseStable(version); !ok {
			return fmt.Errorf("stable release version %q is invalid", version)
		}
		return nil
	case ChannelEdge:
		if !edgeVersionPattern.MatchString(version) {
			return fmt.Errorf("edge release version %q is invalid", version)
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
	if v.Major != major {
		return fmt.Errorf("release version %q is not on the %s channel", version, channel)
	}
	return nil
}

func invalidChannel(channel string) error {
	return &contract.Fault{
		Code:    contract.ValidationFailed,
		Message: "release channel " + channel + " is invalid",
		Hint:    "Use stable, stable-<major>.x (for example stable-1.x) or edge.",
		Details: map[string]any{"channel": channel},
	}
}

func versionRoot(version string) string {
	return strings.TrimRight(currentBaseURL(), "/") + "/releases/" + version
}

package buildpacks

import "fmt"

// TargetPlatform renders the explicit OCI platform for a build node. Pack and
// the lifecycle must select the build node's own architecture for the
// operator-supplied explicit builder: a remote publish without a platform can
// otherwise pick the builder index's default architecture instead of this
// node's.
func TargetPlatform(architecture string) (string, error) {
	switch architecture {
	case "amd64", "arm64":
		return "linux/" + architecture, nil
	default:
		return "", fmt.Errorf(
			"buildpacks publishing is not available for host architecture %s",
			architecture,
		)
	}
}

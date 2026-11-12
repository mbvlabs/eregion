package buildpacks

import "fmt"

// Profile pins the default builder and run image for an executing build node.
type Profile struct {
	Builder  string
	RunImage string
}

// TargetPlatform renders the explicit OCI platform for a build node. Pack and
// the lifecycle must select the build node's own architecture for both the
// pinned default builder and an explicit builder such as Heroku's: a remote
// publish without a platform can otherwise pick the builder index's default
// architecture instead of this node's.
func TargetPlatform(architecture string) (string, error) {
	switch architecture {
	case "amd64", "arm64":
		return "linux/" + architecture, nil
	default:
		return "", fmt.Errorf("Buildpacks publishing is not available for host architecture %s", architecture)
	}
}

// ProfileForArchitecture selects the reference's pinned noble images. It must
// be called on the build node, not the operator's machine.
func ProfileForArchitecture(architecture string) (Profile, error) {
	switch architecture {
	case "amd64":
		return Profile{
			Builder:  "docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:44aa874655716b237568f3c8b98fea1eae9984bf0e85bffe6847c286b610c767",
			RunImage: "docker.io/paketobuildpacks/ubuntu-noble-run@sha256:7b47badbf2e31f418d56204bcc25ef8d7e5dd8e8f824fcf87119031bd36db882",
		}, nil
	case "arm64":
		return Profile{
			Builder:  "docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:29a6861e7a6cac353f34478fcf5cfd2fe606f9bcb2e431bc73033e05fb4d722b",
			RunImage: "docker.io/paketobuildpacks/ubuntu-noble-run@sha256:0f3957568f45877ccbc7cfe6df8b4662037759bb0b543a55e24d35d7cb275c52",
		}, nil
	default:
		return Profile{}, fmt.Errorf("default Buildpacks builder is not available for host architecture %s", architecture)
	}
}

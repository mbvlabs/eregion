package buildpacks

import "fmt"

// Profile is the reference Go runtime stack for an executing build node.
type Profile struct {
	Builder   string
	Buildpack string
	RunImage  string
}

const NodeEngineBuildpack = "paketo-buildpacks/node-engine@8.4.1"

// ProfileForArchitecture selects the reference's pinned noble images. It must
// be called on the build node, not the operator's machine.
func ProfileForArchitecture(architecture string) (Profile, error) {
	switch architecture {
	case "amd64":
		return Profile{
			Builder:   "docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:44aa874655716b237568f3c8b98fea1eae9984bf0e85bffe6847c286b610c767",
			Buildpack: "docker.io/paketobuildpacks/go@sha256:506156f52901969a3dcca288793f86800a57b540c1d5f63329bd3f22b677e659",
			RunImage:  "docker.io/paketobuildpacks/ubuntu-noble-run@sha256:7b47badbf2e31f418d56204bcc25ef8d7e5dd8e8f824fcf87119031bd36db882",
		}, nil
	case "arm64":
		return Profile{
			Builder:   "docker.io/paketobuildpacks/ubuntu-noble-builder@sha256:29a6861e7a6cac353f34478fcf5cfd2fe606f9bcb2e431bc73033e05fb4d722b",
			Buildpack: "docker.io/paketobuildpacks/go@sha256:123f547694d9e3ae99e332e9388612c604d6fdfb72dbdfe0f0be674389519bd6",
			RunImage:  "docker.io/paketobuildpacks/ubuntu-noble-run@sha256:0f3957568f45877ccbc7cfe6df8b4662037759bb0b543a55e24d35d7cb275c52",
		}, nil
	default:
		return Profile{}, fmt.Errorf("Go Buildpacks are not approved for host architecture %s", architecture)
	}
}

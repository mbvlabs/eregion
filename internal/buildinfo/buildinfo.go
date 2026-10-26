package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Release builds set these using -ldflags -X. Schema versions are independent.
var (
	Version = "dev"
	Commit  = "unknown"
	Channel = "dev"
	BuiltAt = ""
)

// ReleaseBaseURL is the public release origin: channel manifests, install.sh
// and versioned artifacts live below it. Release scripts bake the same value
// into manifests, and a mismatch fails verification.
//
// Publish workflows refuse a RELEASE_BASE_URL variable that differs from it.
const ReleaseBaseURL = "https://eregion.mbvlabs.com"

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Channel string `json:"channel"`
	BuiltAt string `json:"built_at,omitempty"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Dirty   bool   `json:"dirty"`
}

func Current() Info {
	info := Info{Version: Version, Commit: Commit, Channel: Channel, BuiltAt: BuiltAt, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if Commit == "unknown" {
					info.Commit = setting.Value
				}
			case "vcs.modified":
				info.Dirty = setting.Value == "true"
			}
		}
	}
	return info
}

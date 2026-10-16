package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Release builds may set these using -ldflags -X. Schema versions are independent.
var (
	Version = "dev"
	Commit  = "unknown"
	BuiltAt = ""
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at,omitempty"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Dirty   bool   `json:"dirty"`
}

func Current() Info {
	info := Info{Version: Version, Commit: Commit, BuiltAt: BuiltAt, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH}
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

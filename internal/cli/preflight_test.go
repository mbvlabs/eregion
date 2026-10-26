package cli

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

// probe is the base of a healthy amd64 Debian 13 server.
func probe() remoteFacts {
	return remoteFacts{
		ID: "debian", VersionID: "13", PrettyName: "Debian GNU/Linux 13 (trixie)",
		Arch: "x86_64", UID: "0", MemMiB: 2048, DiskMiB: 20 * 1024, Tar: "/usr/bin/tar",
	}
}

func TestRemotePreflightSupportMatrix(t *testing.T) {
	supported := []struct {
		id, version, arch string
	}{
		{"debian", "13", "x86_64"},
		{"debian", "13", "aarch64"},
		{"ubuntu", "26.04", "x86_64"},
		{"ubuntu", "26.04", "aarch64"},
	}
	for _, test := range supported {
		facts := probe()
		facts.ID, facts.VersionID, facts.Arch = test.id, test.version, test.arch
		platform, err := facts.preflight(false)
		if err != nil {
			t.Fatalf("%s %s %s was refused: %v", test.id, test.version, test.arch, err)
		}
		if !platform.Supported() {
			t.Fatalf("%s %s %s did not resolve to a supported platform: %+v", test.id, test.version, test.arch, platform)
		}
	}

	refused := []remoteFacts{
		{ID: "debian", VersionID: "12", PrettyName: "Debian 12", Arch: "x86_64", UID: "0", Tar: "/usr/bin/tar"},
		{ID: "ubuntu", VersionID: "24.04", PrettyName: "Ubuntu 24.04", Arch: "x86_64", UID: "0", Tar: "/usr/bin/tar"},
		{ID: "debian", VersionID: "13", PrettyName: "Debian 13", Arch: "riscv64", UID: "0", Tar: "/usr/bin/tar"},
	}
	for _, facts := range refused {
		if _, err := facts.preflight(false); err == nil {
			t.Fatalf("unsupported host was accepted: %+v", facts)
		}
	}
}

func TestRemotePreflightRefusalsCarryTheFix(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*remoteFacts)
		code   contract.Code
		reason string
	}{
		{"not root", func(f *remoteFacts) { f.UID = "1000" }, contract.PreconditionFailed, "not_root"},
		{"too little RAM", func(f *remoteFacts) { f.MemMiB = 512 }, contract.PreconditionFailed, "ram_too_small"},
		{"too little disk", func(f *remoteFacts) { f.DiskMiB = 1024 }, contract.PreconditionFailed, "disk_too_small"},
		{"no tar", func(f *remoteFacts) { f.Tar = "" }, contract.MissingTool, ""},
		{"existing state", func(f *remoteFacts) { f.Existing = []string{"/etc/eregion/node.json"} }, contract.PreconditionFailed, "existing_state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := probe()
			test.change(&facts)
			_, err := facts.preflight(false)
			fault := contract.Classify(err)
			if err == nil || fault.Code != test.code {
				t.Fatalf("got %v", err)
			}
			if test.code == contract.MissingTool {
				if install, _ := fault.Details["install_command"].(string); install == "" {
					t.Fatal("missing_tool carries no install command")
				}
			}
			if test.reason != "" {
				if reason, _ := fault.Details["reason"].(string); reason != test.reason {
					t.Fatalf("reason %v", fault.Details)
				}
			}
		})
	}
	if _, err := probe().preflight(false); err != nil {
		t.Fatal(err)
	}
	facts := probe()
	facts.Existing = []string{"/etc/eregion/node.json"}
	if _, err := facts.preflight(true); err != nil {
		t.Fatalf("a matching resume was refused: %v", err)
	}
}

func TestRemoteFactsParseArchAndMissingOS(t *testing.T) {
	facts, err := parseRemoteFacts("id=ubuntu\nversion=26.04\narch=aarch64\nuid=0\ntar=/usr/bin/tar\nmem_kib=2097152\ndisk_kib=10485760\n")
	if err != nil {
		t.Fatal(err)
	}
	if facts.platform().Arch != "arm64" || facts.MemMiB != 2048 || facts.DiskMiB != 10240 {
		t.Fatalf("facts: %+v", facts)
	}
	if _, err := parseRemoteFacts("arch=amd64\n"); err == nil {
		t.Fatal("missing os-release was accepted")
	}
}

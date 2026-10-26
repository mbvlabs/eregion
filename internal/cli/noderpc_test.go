package cli

import "testing"

func TestNodeRPCCommand(t *testing.T) {
	if got := nodeRPCCommand("admin"); got != "sudo -n /usr/local/bin/eregion node rpc" {
		t.Fatalf("admin command: %q", got)
	}
	if got := nodeRPCCommand("root"); got != "/usr/local/bin/eregion node rpc" {
		t.Fatalf("root command: %q", got)
	}
}

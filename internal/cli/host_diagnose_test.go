package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/host"
)

func TestPlanTextNamesTheControlDatabaseFailureAndFix(t *testing.T) {
	text := planText(host.Plan{
		Node: "main", Total: 1, WouldChange: 1,
		Steps: []host.PlanEntry{{
			ID: "host-database-v1", Complete: false,
			Error: "control database unreachable (dns_resolution) at db.example.com:5432: the database hostname did not resolve",
			Hint:  "Check the database hostname in EREGION_DATABASE_URL and DNS resolution on the node.",
		}},
	})
	for _, want := range []string{"dns_resolution", "db.example.com:5432", "fix:", "DNS resolution"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plan output lacks %q:\n%s", want, text)
		}
	}
}

func TestStatusTextNamesTheControlDatabaseFailureAndFix(t *testing.T) {
	text := statusText(host.NodeStatus{
		Node: "main", Role: "main", NodeVersion: "dev",
		SchemaError: "control database unreachable (tcp_connect) at db.example.com:5432: the TCP connection was refused",
		SchemaFix:   "Check that PostgreSQL listens on that host and port, and that the firewall allows outbound TCP.",
	})
	for _, want := range []string{"tcp_connect", "db.example.com:5432", "schema fix", "firewall"} {
		if !strings.Contains(text, want) {
			t.Fatalf("status output lacks %q:\n%s", want, text)
		}
	}
}

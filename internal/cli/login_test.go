package cli

import "testing"

func TestHandoffRecordRoundTrips(t *testing.T) {
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----"
	h := &secretsHandoff{node: "main", groups: []handoffGroup{
		{"Main node main", []handoffField{{"WireGuard address", "10.78.0.1"}, {"SSH host key", "ssh-ed25519 SHA256:x"}}},
		{"Operator", []handoffField{{"WireGuard address", "10.78.0.100"}, {"SSH private key", "\n" + pem}}},
		{"Control plane API", []handoffField{{"URL", "http://127.0.0.1:8080 (on the server)"}, {"Context", "main"}}},
	}}
	rec := parseHandoffRecord(h.body())
	for _, c := range []struct{ section, label, want string }{
		{"Main node", "WireGuard address", "10.78.0.1"},
		{"Operator", "WireGuard address", "10.78.0.100"},
		{"Operator", "SSH private key", pem},
		{"Main node", "SSH host key", "ssh-ed25519 SHA256:x"},
		{"Control plane API", "Context", "main"},
	} {
		if got := rec.field(c.section, c.label); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.section, c.label, got, c.want)
		}
	}
}

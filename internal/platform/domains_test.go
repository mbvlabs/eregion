package platform

import "testing"

func TestClassifyDNS(t *testing.T) {
	tests := []struct {
		name     string
		expected []string
		observed []string
		want     DNSState
	}{
		{name: "no expected address stays pending", observed: []string{"203.0.113.10"}, want: DNSPending},
		{name: "matching answer", expected: []string{"203.0.113.10"}, observed: []string{"203.0.113.10"}, want: DNSOK},
		{name: "ipv6 matching answer", expected: []string{"2001:db8::10"}, observed: []string{"2001:db8::10"}, want: DNSOK},
		{name: "missing answer", expected: []string{"203.0.113.10"}, want: DNSWrong},
		{name: "wrong answer", expected: []string{"203.0.113.10"}, observed: []string{"203.0.113.11"}, want: DNSWrong},
		{name: "proxied answer", expected: []string{"203.0.113.10"}, observed: []string{"104.16.0.1"}, want: DNSProxied},
		{name: "extra answer still matches", expected: []string{"203.0.113.10"}, observed: []string{"203.0.113.10", "198.51.100.2"}, want: DNSOK},
	}
	for _, test := range tests {
		if got := ClassifyDNS(test.expected, test.observed); got != test.want {
			t.Errorf("%s: got %s, want %s", test.name, got, test.want)
		}
	}
}

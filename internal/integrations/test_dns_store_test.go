package integrations

import (
	"context"
	"strings"
	"sync"

	"github.com/mbvlabs/eregion/internal/contract"
)

// testDNSStore is the in-memory credential/zone source used by unit tests. The
// production path is the PostgreSQL adapter in controlplane.
type testDNSStore struct {
	mu         sync.Mutex
	zones      []DNSZone
	tokens     map[string]string
	zoneReads  int
	tokenReads int
	failToken  error
}

func newTestDNSStore(zones ...DNSZone) *testDNSStore {
	return &testDNSStore{zones: append([]DNSZone(nil), zones...), tokens: map[string]string{}}
}

func (s *testDNSStore) withToken(zoneID, token string) *testDNSStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[zoneID] = token
	return s
}

func (s *testDNSStore) DNSZone(_ context.Context, zoneID, zoneName string) (DNSZone, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zoneReads++
	zoneID = strings.ToLower(strings.TrimSpace(zoneID))
	zoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneName), "."))
	for _, zone := range s.zones {
		if zoneID != "" && zone.ZoneID != zoneID {
			continue
		}
		if zoneName != "" && !strings.EqualFold(zone.ZoneName, zoneName) && zone.ZoneID != zoneName {
			continue
		}
		return zone, true, nil
	}
	return DNSZone{}, false, nil
}

func (s *testDNSStore) DNSZones(context.Context) ([]DNSZone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]DNSZone(nil), s.zones...), nil
}

func (s *testDNSStore) DNSCredential(_ context.Context, provider, zoneID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenReads++
	if s.failToken != nil {
		return nil, s.failToken
	}
	if provider != DNSProviderCloudflare {
		return nil, contract.NewFault(contract.ValidationFailed, "unsupported DNS provider", "Only Cloudflare DNS is supported.")
	}
	token, ok := s.tokens[zoneID]
	if !ok {
		return nil, contract.NewFault(contract.NotFound, "provider credential was not found", "Configure it with eregion domain cloudflare configure, then retry.")
	}
	return []byte(token), nil
}

func (s *testDNSStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.zoneReads, s.tokenReads
}

package host

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestManagedRegistryEndpointRequiresTheManagedSetting(t *testing.T) {
	values := map[string]string{}
	getenv := func(key string) string { return values[key] }
	if _, err := ManagedRegistryEndpoint(getenv); contract.Classify(err).Code != contract.PreconditionFailed || !strings.Contains(err.Error(), RegistryListenEnv) {
		t.Fatalf("missing managed registry endpoint = %v, want an explicit %s refusal", err, RegistryListenEnv)
	}
	values[RegistryListenEnv] = "registry.example:5000"
	if _, err := ManagedRegistryEndpoint(getenv); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("remote managed registry endpoint = %v, want a loopback refusal", err)
	}
	values[RegistryListenEnv] = "127.0.0.1:5500"
	endpoint, err := ManagedRegistryEndpoint(getenv)
	if err != nil || endpoint != "127.0.0.1:5500" {
		t.Fatalf("managed registry endpoint = %q %v", endpoint, err)
	}
}

package cli

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/host"
)

func TestManagedRegistryEndpointForAdmissionIsStrictWhenConfigured(t *testing.T) {
	t.Setenv(host.RegistryListenEnv, "")
	endpoint, err := managedRegistryEndpointForAdmission()
	if err != nil || endpoint != "" {
		t.Fatalf("unconfigured admission endpoint = %q, %v", endpoint, err)
	}
	t.Setenv(host.RegistryListenEnv, "127.0.0.1:5000")
	endpoint, err = managedRegistryEndpointForAdmission()
	if err != nil || endpoint != "127.0.0.1:5000" {
		t.Fatalf("managed admission endpoint = %q, %v", endpoint, err)
	}
	t.Setenv(host.RegistryListenEnv, "registry.example.com:5000")
	if _, err := managedRegistryEndpointForAdmission(); err == nil {
		t.Fatal("a non-loopback managed registry endpoint was accepted")
	}
}

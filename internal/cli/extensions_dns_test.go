package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestParseCloudflareZones(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	zones, err := parseCloudflareZones(" "+id+"=Example.COM. , fedcba9876543210fedcba9876543210:other.example ", "/run/secrets/cloudflare-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(zones) != 2 {
		t.Fatalf("zones = %+v", zones)
	}
	if zones[0].ZoneID != id || zones[0].ZoneName != "example.com" || zones[0].TokenFile != "/run/secrets/cloudflare-token" {
		t.Fatalf("first zone = %+v", zones[0])
	}
	if zones[1].ZoneID != "fedcba9876543210fedcba9876543210" || zones[1].ZoneName != "other.example" {
		t.Fatalf("second zone = %+v", zones[1])
	}

	for _, value := range []string{
		"example.com",
		id,
		id + "=not a name",
		id + "=example.com," + id + "=other.example",
		"nothex=example.com",
	} {
		if _, err := parseCloudflareZones(value, "/run/secrets/cloudflare-token"); err == nil {
			t.Fatalf("unsafe zone configuration accepted: %q", value)
		}
	}
	if _, err := parseCloudflareZones(id+"=example.com", ""); contract.Classify(err).Code != contract.InputRequired {
		t.Fatalf("missing token file = %v", err)
	}
	zones, err = parseCloudflareZones("", "/run/secrets/cloudflare-token")
	if err != nil || zones != nil {
		t.Fatalf("empty zone configuration = %+v, %v", zones, err)
	}
}

func TestStartExtensionEnvironmentListsDNSConfiguration(t *testing.T) {
	var joined = strings.Join(extensionEnvironment, " ")
	for _, name := range []string{"EREGION_DNS_ADMINS", "EREGION_CLOUDFLARE_ZONES", "EREGION_CLOUDFLARE_TOKEN_FILE"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("%s is not advertised by the serve/worker environment", name)
		}
	}
}

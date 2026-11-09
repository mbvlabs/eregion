package cli

import (
	"strings"
	"testing"
)

func TestStartExtensionEnvironmentHasNoCloudflareFileOrZoneConfiguration(t *testing.T) {
	var joined = strings.Join(extensionEnvironment, " ")
	for _, name := range []string{"EREGION_CLOUDFLARE_ZONES", "EREGION_CLOUDFLARE_TOKEN_FILE"} {
		if strings.Contains(joined, name) {
			t.Fatalf("%s must not configure provider credentials after alpha.6", name)
		}
	}
	if !strings.Contains(joined, "EREGION_DNS_ADMINS") {
		t.Fatal("EREGION_DNS_ADMINS is still the generic integration action allowlist")
	}
}

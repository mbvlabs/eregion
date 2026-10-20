package vault

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestPinnedReference(t *testing.T) {
	ref, err := ParseReference("vault:api/production/DATABASE_URL@3")
	if err != nil || ref.Namespace.Key() != "api/production/DATABASE_URL" || ref.Revision != 3 {
		t.Fatalf("reference=%+v err=%v", ref, err)
	}
	for _, text := range []string{"plaintext", "vault:api/production/DATABASE_URL@0", "vault:api/production/DATABASE_URL@03", "vault:api/production/DATABASE_URL@+3", "vault:api/production/DATABASE_URL@3@4", "vault:api/production/DATABASE_URL@9223372036854775808"} {
		if _, err := ParseReference(text); err == nil {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestLegacyResolverCannotReadSecretsWithoutEnvironment(t *testing.T) {
	r := NewEnvironmentResolver(nil)
	_, err := r.Resolve(context.Background(), map[string]string{"TOKEN": "vault:other/production/TOKEN@1"})
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("namespace-free resolution = %v", err)
	}
}

package platform

import "testing"

func TestReleaseCommandDigestBindsArgvAndTimeout(t *testing.T) {
	base := CommandSpec{Argv: []string{"bin/migrate", "--verbose"}, TimeoutSeconds: 120}
	if got, want := ReleaseCommandDigest(base), ReleaseCommandDigest(CommandSpec{Argv: []string{"bin/migrate", "--verbose"}, TimeoutSeconds: 120}); got != want {
		t.Fatalf("identical commands have different digests: %s != %s", got, want)
	}
	for _, changed := range []CommandSpec{
		{Argv: []string{"bin/migrate"}, TimeoutSeconds: 120},
		{Argv: []string{"bin/migrate", "--verbose"}, TimeoutSeconds: 60},
		{Argv: []string{"bin/migrate", "--verbose", "--force"}, TimeoutSeconds: 120},
	} {
		if ReleaseCommandDigest(base) == ReleaseCommandDigest(changed) {
			t.Fatalf("changed command %+v kept the digest", changed)
		}
	}
}

package controlplane

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestCheckResolvedGitSourceRefusesARevisionChange(t *testing.T) {
	const (
		sha      = "0123456789abcdef0123456789abcdef01234567"
		gitURL   = "https://github.com/acme/app.git"
		imageRef = "example/app:1"
		conflict = "environment source changed during submission"
	)
	gitSource := platform.SourceBinding{Kind: "git", URL: gitURL, Ref: "main"}
	imageSource := platform.SourceBinding{Kind: "image", Image: imageRef}
	resolvedGit := &resolvedGitRef{Kind: "git", URL: gitURL, Ref: "main", SHA: sha}
	resolvedImage := &resolvedGitRef{Kind: "image"}

	for _, tc := range []struct {
		name      string
		resolved  *resolvedGitRef
		source    platform.SourceBinding
		override  string
		wantError bool
	}{
		{name: "unchanged git source", resolved: resolvedGit, source: gitSource},
		{name: "ref changed without override", resolved: resolvedGit, source: platform.SourceBinding{Kind: "git", URL: gitURL, Ref: "other"}, wantError: true},
		{name: "ref changed with override", resolved: resolvedGit, source: platform.SourceBinding{Kind: "git", URL: gitURL, Ref: "other"}, override: "main"},
		{name: "url changed", resolved: resolvedGit, source: platform.SourceBinding{Kind: "git", URL: "https://github.com/acme/other.git", Ref: "main"}, wantError: true},
		{name: "git became image", resolved: resolvedGit, source: imageSource, wantError: true},
		{name: "image became git", resolved: resolvedImage, source: gitSource, wantError: true},
		{name: "override differs from the resolution", resolved: resolvedGit, source: gitSource, override: "other", wantError: true},
		{name: "no resolution", resolved: nil, source: gitSource, wantError: true},
		{name: "unchanged image source", resolved: resolvedImage, source: imageSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkResolvedGitSource(tc.resolved, tc.source, tc.override)
			if !tc.wantError {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if code := contract.Classify(err).Code; code != contract.Conflict || !strings.Contains(err.Error(), conflict) {
				t.Fatalf("refusal = %s, %v; want %s containing %q", code, err, contract.Conflict, conflict)
			}
		})
	}
}

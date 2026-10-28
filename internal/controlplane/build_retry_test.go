package controlplane

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestRetryBuildSourceUsesOnlyTheExactCommit(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	gitSource := platform.Source{Type: "git", URL: "https://github.com/acme/app.git", Ref: "main"}

	if _, _, err := retryBuildSource(gitSource, ""); err == nil {
		t.Fatal("a git retry without a commit was accepted")
	} else if code := contract.Classify(err).Code; code != contract.PreconditionFailed || !strings.Contains(err.Error(), "no exact commit") {
		t.Fatalf("missing commit refusal = %s, %v", code, err)
	}
	if _, _, err := retryBuildSource(gitSource, "main"); err == nil {
		t.Fatal("a branch name was accepted as the retry commit")
	}

	source, commit, err := retryBuildSource(gitSource, sha)
	if err != nil || commit != sha || source.Ref != sha {
		t.Fatalf("retry source = %+v commit=%q err=%v; want ref %q", source, commit, err, sha)
	}

	imageSource := platform.Source{Type: "image", Image: "example/app:1"}
	source, commit, err = retryBuildSource(imageSource, "")
	if err != nil || commit != "" || source.Image != "example/app:1" {
		t.Fatalf("image retry = %+v commit=%q err=%v", source, commit, err)
	}
}

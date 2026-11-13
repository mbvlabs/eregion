package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/release"
	fulcio "github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

// This exercises the production adapter and Git proofs, with a test-only
// cosign executable and fake AWS CLI; it does not test Sigstore cryptography.
func TestProductionCompositionConflictAndTrust(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	git("commit", "--allow-empty", "-qm", "first")
	first := git("rev-parse", "HEAD")
	git("tag", "v1.0.0-beta.1")
	git("commit", "--allow-empty", "-qm", "second")
	second := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/master", second)
	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })
	bin := filepath.Join(dir, "bin")
	bucket := filepath.Join(dir, "bucket")
	if err := os.MkdirAll(bin, 0755); err != nil {
		t.Fatal(err)
	}
	aws := `#!/bin/sh
set -eu
op="$2"; shift 2
key=""; body=""; dest=""
while [ "$#" -gt 0 ]; do
  case "$1" in --key) shift; key="$1";; --body) shift; body="$1";; esac
  dest="$1"; shift
done
if [ "$op" = get-object ]; then
  [ -f "$TEST_BUCKET/$key" ] || { echo 'An error occurred (NoSuchKey)' >&2; exit 1; }
  cp "$TEST_BUCKET/$key" "$dest"
  etag="$(sha256sum "$dest" | cut -d' ' -f1)"
  jq -nc --arg e "\"$etag\"" '{ETag:$e}'
elif [ "$op" = put-object ]; then
  echo put >>"$TEST_PUTS"
  if [ -f "$TEST_CONFLICT" ]; then
    if [ "${TEST_ALWAYS_CONFLICT:-}" != 1 ]; then rm "$TEST_CONFLICT"; fi
    cp "$TEST_WINNER" "$TEST_BUCKET/edge/manifest.json"
    echo 'An error occurred (PreconditionFailed)' >&2; exit 1
  fi
  cp "$body" "$TEST_BUCKET/$key"
else exit 1; fi
`
	cosign := `#!/bin/sh
set -eu
bundle=""; identity=""
while [ "$#" -gt 0 ]; do
  case "$1" in --bundle) shift; bundle="$1";; --certificate-identity) shift; identity="$1";; esac
  shift
done
[ "$(jq -r .testIdentity "$bundle")" = "$identity" ] && [ "$(jq -r .testValid "$bundle")" = true ]
`
	for name, data := range map[string]string{"aws": aws, "cosign": cosign} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_BUCKET", bucket)
	t.Setenv("TEST_PUTS", filepath.Join(dir, "puts"))
	t.Setenv("TEST_WINNER", filepath.Join(dir, "winner"))
	t.Setenv("TEST_CONFLICT", filepath.Join(dir, "conflict"))
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	manifest := func(version string) []byte {
		base := buildinfo.ReleaseBaseURL + "/releases/" + version
		m := release.Manifest{SchemaVersion: 1, Channel: "edge", Version: version, ReleaseBaseURL: base, Artifacts: map[string]release.Artifact{}}
		for _, platform := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
			m.Artifacts[platform] = release.Artifact{URL: base + "/" + platform + "/eregion", SHA256: strings.Repeat("a", 64)}
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	bundle := func(version, commit, trigger, identity string, valid bool) []byte {
		t.Helper()
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		digestDER, _ := asn1.Marshal(commit)
		triggerDER, _ := asn1.Marshal(trigger)
		template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			ExtraExtensions: []pkix.Extension{{Id: fulcio.OIDSourceRepositoryDigest, Value: digestDER}, {Id: fulcio.OIDBuildTrigger, Value: triggerDER}}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, priv.Public(), priv)
		if err != nil {
			t.Fatal(err)
		}
		if identity == "" {
			identity, err = release.Identity("edge", version)
			if err != nil {
				t.Fatal(err)
			}
		}
		data, err := json.Marshal(map[string]any{"verificationMaterial": map[string]any{"certificate": map[string]any{"rawBytes": der}}, "testIdentity": identity, "testValid": valid})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	firstVersion, winnerVersion := "edge-"+first[:12], "edge-"+second[:12]
	candidateVersion := "1.0.0-beta.1"
	candidateRoot := filepath.Join(dir, "candidate")
	candidatePath := filepath.Join(candidateRoot, "releases", candidateVersion, "manifest.json")
	write(candidatePath, manifest(candidateVersion))
	write(candidatePath+".sigstore.json", bundle(candidateVersion, first, "push", "", true))
	for _, v := range []struct{ version, commit string }{{firstVersion, first}, {winnerVersion, second}} {
		path := filepath.Join(bucket, "releases", v.version, "manifest.json")
		write(path, manifest(v.version))
		write(path+".sigstore.json", bundle(v.version, v.commit, "push", "", true))
	}
	write(filepath.Join(dir, "winner"), manifest(winnerVersion))
	pointerPath := filepath.Join(bucket, "edge", "manifest.json")
	store, candidate, cleanup, err := newProductionStore(context.Background(), "bucket", "edge/manifest.json", "https://example.invalid", "edge", candidateRoot, candidateVersion, first)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	decide := func(p pointer) (bool, error) { return edgePromotes(p, candidate, gitAncestor) }
	for _, tc := range []struct {
		name, commit, trigger, identity string
		valid                           bool
	}{
		{"candidate wrong signer", first, "push", "wrong", true},
		{"candidate bad signature", first, "push", "", false},
		{"candidate wrong commit", second, "push", "", true},
		{"candidate wrong event", first, "workflow_dispatch", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(candidatePath+".sigstore.json", bundle(candidateVersion, tc.commit, tc.trigger, tc.identity, tc.valid))
			write(filepath.Join(dir, "puts"), nil)
			if _, err := store.verify(context.Background(), store.manifest, store.bundle, candidateVersion); err == nil {
				t.Fatal("untrusted candidate accepted")
			}
			data, err := os.ReadFile(filepath.Join(dir, "puts"))
			if err != nil || len(data) != 0 {
				t.Fatalf("untrusted candidate reached PUT: %q %v", data, err)
			}
		})
	}
	write(candidatePath+".sigstore.json", bundle(candidateVersion, first, "push", "", true))
	write(pointerPath, manifest(firstVersion))
	write(filepath.Join(dir, "conflict"), []byte("1"))
	write(filepath.Join(dir, "puts"), nil)
	moved, err := advance(context.Background(), store, decide)
	if err != nil || moved {
		t.Fatalf("conflict result: moved=%v err=%v", moved, err)
	}
	puts, err := os.ReadFile(filepath.Join(dir, "puts"))
	if err != nil || strings.Count(string(puts), "put\n") != 1 {
		t.Fatalf("expected one conditional PUT, got %q %v", puts, err)
	}
	got, err := os.ReadFile(pointerPath)
	if err != nil || string(got) != string(manifest(winnerVersion)) {
		t.Fatal("winner was overwritten")
	}
	for _, tc := range []struct {
		name    string
		pointer []byte
		bundle  []byte
	}{
		{"pointer mismatch", append(manifest(firstVersion), ' '), bundle(firstVersion, first, "push", "", true)},
		{"wrong signer", manifest(firstVersion), bundle(firstVersion, first, "push", "wrong", true)},
		{"bad signature", manifest(firstVersion), bundle(firstVersion, first, "push", "", false)},
		{"wrong commit", manifest(firstVersion), bundle(firstVersion, second, "push", "", true)},
		{"wrong event", manifest(firstVersion), bundle(firstVersion, first, "workflow_dispatch", "", true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write(pointerPath, tc.pointer)
			write(filepath.Join(bucket, "releases", firstVersion, "manifest.json.sigstore.json"), tc.bundle)
			write(filepath.Join(dir, "puts"), nil)
			if _, err := advance(context.Background(), store, decide); err == nil {
				t.Fatal("untrusted pointer accepted")
			}
			data, err := os.ReadFile(filepath.Join(dir, "puts"))
			if err != nil || len(data) != 0 {
				t.Fatal(fmt.Sprintf("untrusted pointer reached PUT: %q %v", data, err))
			}
		})
	}
	write(pointerPath, manifest(firstVersion))
	write(filepath.Join(bucket, "releases", firstVersion, "manifest.json.sigstore.json"), bundle(firstVersion, first, "push", "", true))
	write(filepath.Join(dir, "winner"), manifest(firstVersion))
	write(filepath.Join(dir, "conflict"), []byte("1"))
	write(filepath.Join(dir, "puts"), nil)
	t.Setenv("TEST_ALWAYS_CONFLICT", "1")
	if _, err := advance(context.Background(), store, decide); err == nil {
		t.Fatal("unbounded conflicts unexpectedly succeeded")
	}
	puts, err = os.ReadFile(filepath.Join(dir, "puts"))
	if err != nil || strings.Count(string(puts), "put\n") != 5 {
		t.Fatalf("expected five bounded conditional PUTs, got %q %v", puts, err)
	}
}

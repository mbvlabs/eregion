package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	fulcio "github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

func TestCertificateClaims(t *testing.T) {
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	tests := []struct {
		name string
		ext  fulcio.Extensions
		ok   bool
	}{
		{"modern", fulcio.Extensions{SourceRepositoryDigest: sha, BuildTrigger: "push"}, true},
		{"legacy", fulcio.Extensions{GithubWorkflowSHA: sha, GithubWorkflowTrigger: "push"}, true},
		{"both", fulcio.Extensions{SourceRepositoryDigest: sha, GithubWorkflowSHA: sha, BuildTrigger: "push", GithubWorkflowTrigger: "push"}, true},
		{"missing sha", fulcio.Extensions{BuildTrigger: "push"}, false},
		{"abbreviated", fulcio.Extensions{SourceRepositoryDigest: sha[:12], BuildTrigger: "push"}, false},
		{"malformed", fulcio.Extensions{SourceRepositoryDigest: strings.Repeat("z", 40), BuildTrigger: "push"}, false},
		{"conflicting", fulcio.Extensions{SourceRepositoryDigest: sha, GithubWorkflowSHA: other, BuildTrigger: "push"}, false},
		{"missing trigger", fulcio.Extensions{SourceRepositoryDigest: sha}, false},
		{"wrong trigger", fulcio.Extensions{SourceRepositoryDigest: sha, BuildTrigger: "workflow_dispatch"}, false},
		{"conflicting trigger", fulcio.Extensions{SourceRepositoryDigest: sha, BuildTrigger: "push", GithubWorkflowTrigger: "pull_request"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCertificateClaims(tc.ext, sha)
			if (err == nil) != tc.ok {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestCertificateExtensions(t *testing.T) {
	sha := strings.Repeat("a", 40)
	der := func(s string) []byte {
		b, err := asn1.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tests := []struct {
		name string
		ext  []pkix.Extension
		ok   bool
	}{
		{"modern", []pkix.Extension{{Id: fulcio.OIDSourceRepositoryDigest, Value: der(sha)}, {Id: fulcio.OIDBuildTrigger, Value: der("push")}}, true},
		{"legacy", []pkix.Extension{{Id: fulcio.OIDGitHubWorkflowSHA, Value: []byte(sha)}, {Id: fulcio.OIDGitHubWorkflowTrigger, Value: []byte("push")}}, true},
		{"empty modern alongside valid legacy", []pkix.Extension{{Id: fulcio.OIDSourceRepositoryDigest, Value: der("")}, {Id: fulcio.OIDGitHubWorkflowSHA, Value: []byte(sha)}, {Id: fulcio.OIDBuildTrigger, Value: der("push")}}, false},
		{"malformed modern", []pkix.Extension{{Id: fulcio.OIDSourceRepositoryDigest, Value: []byte{0xff}}, {Id: fulcio.OIDBuildTrigger, Value: der("push")}}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtraExtensions: tc.ext}
			raw, err := x509.CreateCertificate(rand.Reader, cert, cert, key.Public(), key)
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := json.Marshal(map[string]any{"verificationMaterial": map[string]any{"certificate": map[string]any{"rawBytes": raw}}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "bundle")
			if err := os.WriteFile(path, bundle, 0600); err != nil {
				t.Fatal(err)
			}
			if err := checkCertificateCommit(path, sha); (err == nil) != tc.ok {
				t.Fatalf("got %v", err)
			}
		})
	}
}

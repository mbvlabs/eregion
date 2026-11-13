package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/release"
	fulcio "github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

type productionStore struct {
	bucket, key, endpoint, channel, dir string
	manifest, bundle, installer         string
}

func command(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nol fixed executable and separate arguments
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%s: %s: %w", name, strings.TrimSpace(string(exit.Stderr)), err)
		}
	}
	return out, err
}

func awsCode(err error, codes ...string) bool {
	if err == nil {
		return false
	}
	for _, code := range codes {
		if strings.Contains(err.Error(), "("+code+")") {
			return true
		}
	}
	return false
}

func (s *productionStore) get(ctx context.Context, key, dest string) (string, bool, error) {
	raw, err := command(ctx, "aws", "s3api", "get-object", "--bucket", s.bucket, "--key", key,
		"--endpoint-url", s.endpoint, "--output", "json", dest)
	if awsCode(err, "NoSuchKey", "404", "NotFound") {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var result struct{ ETag string }
	if err := json.Unmarshal(raw, &result); err != nil || result.ETag == "" {
		return "", false, fmt.Errorf("missing or invalid ETag for %s", key)
	}
	return result.ETag, true, nil
}

func (s *productionStore) verify(ctx context.Context, manifest, bundle, expectedVersion string) (pointer, error) {
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return pointer{}, err
	}
	var m release.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return pointer{}, err
	}
	identity, err := release.Identity(s.channel, m.Version)
	if err != nil {
		return pointer{}, err
	}
	if err := release.Validate(m, s.channel, expectedVersion); err != nil {
		return pointer{}, err
	}
	if _, err := command(ctx, "cosign", "verify-blob", manifest, "--bundle", bundle,
		"--certificate-identity", identity, "--certificate-oidc-issuer", release.Issuer); err != nil {
		return pointer{}, err
	}
	commit, err := gitVersionCommit(ctx, m.Version)
	if err != nil {
		return pointer{}, err
	}
	if err := checkCertificateCommit(bundle, commit); err != nil {
		return pointer{}, err
	}
	return pointer{version: m.Version, commit: commit}, nil
}

func newProductionStore(ctx context.Context, bucket, key, endpoint, channel, root, version, checkout string) (*productionStore, pointer, func(), error) {
	dir, err := os.MkdirTemp("", "eregion-promote-")
	if err != nil {
		return nil, pointer{}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	s := &productionStore{bucket: bucket, key: key, endpoint: endpoint, channel: channel, dir: dir,
		manifest: filepath.Join(root, "releases", version, "manifest.json"),
		bundle:   filepath.Join(root, "releases", version, "manifest.json.sigstore.json")}
	installerDir := filepath.Dir(key)
	if installerDir == "." {
		installerDir = ""
	}
	s.installer = filepath.Join(root, installerDir, "install.sh")
	p, err := s.verify(ctx, s.manifest, s.bundle, version)
	if err == nil && p.commit != checkout {
		err = fmt.Errorf("candidate commit differs from workflow checkout")
	}
	if err != nil {
		cleanup()
		return nil, pointer{}, nil, err
	}
	return s, p, cleanup, nil
}

func (s *productionStore) read(ctx context.Context) (pointer, error) {
	path := filepath.Join(s.dir, "current")
	etag, exists, err := s.get(ctx, s.key, path)
	if err != nil || !exists {
		return pointer{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return pointer{}, err
	}
	var peek struct{ Version string }
	if err := json.Unmarshal(raw, &peek); err != nil {
		return pointer{}, err
	}
	if _, err := release.Identity(s.channel, peek.Version); err != nil {
		return pointer{}, err
	}
	immutable := filepath.Join(s.dir, "immutable")
	bundle := filepath.Join(s.dir, "bundle")
	for key, dest := range map[string]string{
		"releases/" + peek.Version + "/manifest.json":               immutable,
		"releases/" + peek.Version + "/manifest.json.sigstore.json": bundle,
	} {
		if _, ok, err := s.get(ctx, key, dest); err != nil {
			return pointer{}, err
		} else if !ok {
			return pointer{}, fmt.Errorf("missing immutable object %s", key)
		}
	}
	trusted, err := os.ReadFile(immutable)
	if err != nil || !bytes.Equal(raw, trusted) {
		return pointer{}, fmt.Errorf("pointer differs from immutable manifest")
	}
	p, err := s.verify(ctx, immutable, bundle, peek.Version)
	p.etag = etag
	return p, err
}

func (s *productionStore) write(ctx context.Context, etag string) error {
	condition, value := "--if-match", etag
	if etag == "" {
		condition, value = "--if-none-match", "*"
	}
	_, err := command(ctx, "aws", "s3api", "put-object", "--bucket", s.bucket, "--key", s.key,
		"--body", s.manifest, condition, value, "--cache-control", "no-cache",
		"--content-type", "application/json", "--endpoint-url", s.endpoint)
	if awsCode(err, "PreconditionFailed", "ConditionalRequestConflict", "412", "409") {
		return errConflict
	}
	return err
}

func (s *productionStore) publishCompatibility(ctx context.Context) error {
	current, err := s.read(ctx)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(s.manifest)
	if err != nil {
		return err
	}
	observed, err := os.ReadFile(filepath.Join(s.dir, "current"))
	if err != nil || current.etag == "" || !bytes.Equal(raw, observed) {
		return nil // A newer publisher won; do not intentionally replace its files.
	}
	prefix := strings.TrimSuffix(s.key, "manifest.json")
	for _, f := range []struct{ path, key, contentType string }{
		{s.installer, prefix + "install.sh", "text/x-shellscript"},
		{s.bundle, prefix + "manifest.json.sigstore.json", "application/json"},
	} {
		etag, exists, err := s.get(ctx, f.key, filepath.Join(s.dir, "compatibility"))
		if err != nil {
			return err
		}
		condition, value := "--if-none-match", "*"
		if exists {
			condition, value = "--if-match", etag
		}
		_, err = command(ctx, "aws", "s3api", "put-object", "--bucket", s.bucket, "--key", f.key,
			"--body", f.path, condition, value, "--cache-control", "no-cache",
			"--content-type", f.contentType, "--endpoint-url", s.endpoint)
		if awsCode(err, "PreconditionFailed", "ConditionalRequestConflict", "412", "409") {
			return nil // Another publisher updated this compatibility object.
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func gitAncestor(a, b string) (bool, error) {
	err := exec.Command("git", "merge-base", "--is-ancestor", a, b).Run()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func gitVersionCommit(ctx context.Context, version string) (string, error) {
	ref := "refs/tags/v" + version + "^{commit}"
	if strings.HasPrefix(version, "edge-") {
		prefix := strings.TrimPrefix(version, "edge-")
		raw, err := command(ctx, "git", "rev-parse", "--disambiguate="+prefix)
		if err != nil {
			return "", err
		}
		objects := strings.Fields(string(raw))
		if len(objects) != 1 {
			return "", fmt.Errorf("edge commit prefix is missing or ambiguous")
		}
		ref = objects[0] + "^{commit}"
	}
	raw, err := command(ctx, "git", "rev-parse", "--verify", ref)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(raw))
	ok, err := gitAncestor(commit, "refs/remotes/origin/master")
	if err != nil || !ok {
		return "", fmt.Errorf("release commit is not provably on master: %v", err)
	}
	return commit, nil
}

func checkCertificateCommit(path, commit string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var b struct {
		VerificationMaterial struct {
			Certificate struct{ RawBytes []byte } `json:"certificate"`
			Chain       struct {
				Certificates []struct{ RawBytes []byte } `json:"certificates"`
			} `json:"x509CertificateChain"`
		} `json:"verificationMaterial"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return err
	}
	der := b.VerificationMaterial.Certificate.RawBytes
	if len(der) == 0 && len(b.VerificationMaterial.Chain.Certificates) > 0 {
		der = b.VerificationMaterial.Chain.Certificates[0].RawBytes
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, e := range cert.Extensions {
		if e.Id.Equal(fulcio.OIDSourceRepositoryDigest) || e.Id.Equal(fulcio.OIDGitHubWorkflowSHA) ||
			e.Id.Equal(fulcio.OIDBuildTrigger) || e.Id.Equal(fulcio.OIDGitHubWorkflowTrigger) {
			key := e.Id.String()
			if seen[key] {
				return fmt.Errorf("duplicate signer certificate extension %s", key)
			}
			seen[key] = true
		}
	}
	ext, err := fulcio.ParseExtensions(cert.Extensions)
	if err != nil {
		return err
	}
	for _, claim := range []struct {
		oid   string
		value string
	}{
		{fulcio.OIDSourceRepositoryDigest.String(), ext.SourceRepositoryDigest},
		{fulcio.OIDGitHubWorkflowSHA.String(), ext.GithubWorkflowSHA},
		{fulcio.OIDBuildTrigger.String(), ext.BuildTrigger},
		{fulcio.OIDGitHubWorkflowTrigger.String(), ext.GithubWorkflowTrigger},
	} {
		if seen[claim.oid] && claim.value == "" {
			return fmt.Errorf("empty signer certificate extension %s", claim.oid)
		}
	}
	return validateCertificateClaims(ext, commit)
}

var fullGitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func validateCertificateClaims(ext fulcio.Extensions, commit string) error {
	if !fullGitSHA.MatchString(commit) {
		return fmt.Errorf("Git commit is not a full source SHA")
	}
	found := false
	for _, sha := range []string{ext.SourceRepositoryDigest, ext.GithubWorkflowSHA} {
		if sha == "" {
			continue
		}
		found = true
		sha = strings.TrimPrefix(sha, "sha1:")
		if !fullGitSHA.MatchString(sha) || sha != commit {
			return fmt.Errorf("signer certificate source commit does not match Git")
		}
	}
	if !found {
		return fmt.Errorf("signer certificate source commit is missing")
	}
	found = false
	for _, trigger := range []string{ext.BuildTrigger, ext.GithubWorkflowTrigger} {
		if trigger == "" {
			continue
		}
		found = true
		if trigger != "push" {
			return fmt.Errorf("signer certificate workflow trigger is not push")
		}
	}
	if !found {
		return fmt.Errorf("signer certificate workflow trigger is missing")
	}
	return nil
}

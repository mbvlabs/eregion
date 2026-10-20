package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testDownloader() *Downloader {
	d := NewDownloader(nil)
	d.allowLoopback = true
	return d
}

func TestFetchRejectsRedirect(t *testing.T) {
	targetHits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits++
		_, _ = w.Write([]byte("secret"))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := testDownloader().Fetch(context.Background(), redirect.URL, 1024); err == nil {
		t.Fatal("redirect was followed")
	}
	if targetHits != 0 {
		t.Fatalf("redirect target was contacted %d times", targetHits)
	}
}

func TestDownloadRejectsUnsafeURLs(t *testing.T) {
	d := NewDownloader(nil)
	for _, raw := range []string{
		"http://127.0.0.1/artifact",
		"https://user:pass@releases.example.com/artifact",
		"https://releases.example.com/artifact#fragment",
		"https://127.0.0.1/artifact",
		"https://[::1]/artifact",
		"https://10.0.0.1/artifact",
		"https://192.168.1.1/artifact",
		"https://192.0.2.1/artifact",
		"https://203.0.113.1/artifact",
		"https://169.254.169.254/latest/meta-data",
		"https://localhost/artifact",
		"file:///etc/passwd",
	} {
		if _, err := d.Fetch(context.Background(), raw, 1024); err == nil {
			t.Fatalf("unsafe URL accepted: %s", raw)
		}
	}
	if err := ValidateArtifactURL("https://releases.example.com/a.tgz"); err != nil {
		t.Fatalf("public HTTPS artifact URL rejected: %v", err)
	}
	if err := ValidateArtifactURL("https://10.0.0.1/a.tgz"); err == nil {
		t.Fatal("private artifact URL validated")
	}
}

func TestDownloadRejectsUnlistedHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	d := NewDownloader([]string{"releases.example.com"})
	d.allowLoopback = true
	if _, err := d.Fetch(context.Background(), server.URL, 1024); err == nil {
		t.Fatal("host outside the allowlist was contacted")
	}
}

func TestDownloadEnforcesSignedSizeAndDigest(t *testing.T) {
	content := []byte("archive-content")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	dir := t.TempDir()
	d := testDownloader()
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	path, err := d.DownloadVerified(context.Background(), server.URL, sha, int64(len(content)), dir)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != string(content) {
		t.Fatalf("stored artifact mismatch: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := d.DownloadVerified(context.Background(), server.URL, sha, int64(len(content))+1, dir); err == nil {
		t.Fatal("size mismatch accepted")
	}
	tampered := sha
	if tampered[0] == '0' {
		tampered = "1" + tampered[1:]
	} else {
		tampered = "0" + tampered[1:]
	}
	if _, err := d.DownloadVerified(context.Background(), server.URL, tampered, int64(len(content)), dir); err == nil {
		t.Fatal("digest mismatch accepted")
	}
}

func TestDownloadRejectsOversizedObject(t *testing.T) {
	content := []byte("0123456789")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write(content)
			flusher.Flush()
		} else {
			_, _ = w.Write(content)
		}
	}))
	defer server.Close()
	_, err := testDownloader().DownloadVerified(context.Background(), server.URL, digest([]byte("small")), 4, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("oversized object accepted: %v", err)
	}
}

func TestFetchManifestBound(t *testing.T) {
	content := make([]byte, MaxManifestBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(content)
	}))
	defer server.Close()
	if _, err := testDownloader().FetchManifest(context.Background(), server.URL); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestCopyVerifiedFile(t *testing.T) {
	content := []byte("offline-archive")
	source := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(source, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sha := digest(content)
	stored, err := CopyVerifiedFile(source, sha, int64(len(content)), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stored); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyVerifiedFile(source, digest([]byte("other")), int64(len(content)), t.TempDir()); err == nil {
		t.Fatal("offline artifact digest mismatch accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(source, link); err == nil {
		if _, err := CopyVerifiedFile(link, sha, int64(len(content)), t.TempDir()); err == nil {
			t.Fatal("symlinked offline artifact accepted")
		}
	}
}

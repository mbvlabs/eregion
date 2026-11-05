package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

const reloadToken = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func reloadPrincipal(token, subject string, scopes ...string) Principal {
	hash := sha256.Sum256([]byte(token))
	return Principal{
		Actor:       contract.Actor{Kind: "ci", Subject: subject},
		TokenSHA256: hex.EncodeToString(hash[:]),
		Scopes:      scopes,
	}
}

func encodeStore(t *testing.T, principals []Principal) []byte {
	t.Helper()
	raw, err := json.Marshal(principals)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeStoreAtomic(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := writeStoreFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeStoreFile(path string, raw []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func TestLoadAuthPicksUpCreatedTokenWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeStoreAtomic(t, path, encodeStore(t, []Principal{}))
	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	header := "Bearer " + token
	if _, err := auth.authenticate(header, "operations:read"); contract.Classify(err).Code != contract.Unauthenticated {
		t.Fatalf("an empty store authenticated a token: %v", err)
	}
	writeStoreAtomic(t, path, encodeStore(t, []Principal{reloadPrincipal(token, "created", "operations:read")}))
	if _, err := auth.authenticate(header, "operations:read"); err != nil {
		t.Fatalf("a token created after startup was refused without a restart: %v", err)
	}
}

func TestLoadAuthRejectsRevokedTokenOnNextRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	revokedHeader := "Bearer " + token
	keptHeader := "Bearer " + reloadToken
	writeStoreAtomic(t, path, encodeStore(t, []Principal{
		reloadPrincipal(token, "revoked", "operations:read"),
		reloadPrincipal(reloadToken, "kept", "operations:read"),
	}))
	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.authenticate(revokedHeader, "operations:read"); err != nil {
		t.Fatalf("token was not authenticated before revocation: %v", err)
	}
	writeStoreAtomic(t, path, encodeStore(t, []Principal{reloadPrincipal(reloadToken, "kept", "operations:read")}))
	if _, err := auth.authenticate(revokedHeader, "operations:read"); contract.Classify(err).Code != contract.Unauthenticated {
		t.Fatalf("a revoked token was still accepted: %v", err)
	}
	if _, err := auth.authenticate(keptHeader, "operations:read"); err != nil {
		t.Fatalf("the remaining token was refused: %v", err)
	}
}

func TestLoadAuthFailsClosedOnMalformedRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	header := "Bearer " + token
	writeStoreAtomic(t, path, encodeStore(t, []Principal{reloadPrincipal(token, "created", "operations:read")}))
	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.authenticate(header, "operations:read"); err != nil {
		t.Fatalf("valid token refused: %v", err)
	}
	writeStoreAtomic(t, path, []byte(`[{"actor":`))
	if _, err := auth.authenticate(header, "operations:read"); contract.Classify(err).Code != contract.Unauthenticated {
		t.Fatalf("malformed store authenticated a previously valid token: %v", err)
	}
	writeStoreAtomic(t, path, encodeStore(t, []Principal{reloadPrincipal(token, "created", "operations:read")}))
	if _, err := auth.authenticate(header, "operations:read"); err != nil {
		t.Fatalf("a repaired store was not reloaded: %v", err)
	}
}

func TestLoadAuthRechecksPermissionsOnReload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("credential permissions require Unix file modes")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	header := "Bearer " + token
	writeStoreAtomic(t, path, encodeStore(t, []Principal{reloadPrincipal(token, "created", "operations:read")}))
	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.authenticate(header, "operations:read"); contract.Classify(err).Code != contract.Unauthenticated {
		t.Fatalf("a world-readable store kept authenticating: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.authenticate(header, "operations:read"); err != nil {
		t.Fatalf("an owner-only store was not reloaded: %v", err)
	}
}

func TestAuthStampTracksOwnershipAndChangeTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeStoreAtomic(t, path, encodeStore(t, []Principal{}))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stamp := stampFor(info)
	if !stamp.ownershipAvailable {
		t.Skip("ownership and change time are unavailable on this platform")
	}
	for name, change := range map[string]func(*authFileStamp){
		"uid":   func(s *authFileStamp) { s.uid++ },
		"gid":   func(s *authFileStamp) { s.gid++ },
		"ctime": func(s *authFileStamp) { s.ctimeNS++ },
	} {
		changed := stamp
		change(&changed)
		if stamp.equal(changed) {
			t.Fatalf("a %s-only change left the stamp equal", name)
		}
	}
}

func TestOpenFailureRepairedBeforePublicationIsRetryable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission failures require Unix modes")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can open a mode 0000 file")
	}
	path := filepath.Join(t.TempDir(), "auth.json")
	writeStoreAtomic(t, path, encodeStore(t, []Principal{reloadPrincipal(token, "created", "operations:read")}))
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	repaired := false
	var repairErr error
	_, stamp, err := readAuthFileWith(path, func() {
		// The unreadable open has already failed; repair permissions before the
		// caller would otherwise stamp the now-valid path.
		repairErr = os.Chmod(path, 0o600)
		repaired = true
	})
	if repairErr != nil {
		t.Fatal(repairErr)
	}
	if !repaired {
		t.Fatal("the unreadable store was opened before the repair seam")
	}
	if err == nil {
		t.Fatal("a mode 0000 store was opened")
	}
	if stamp.present {
		t.Fatalf("an open failure cached the repaired file stamp: %+v", stamp)
	}
	// Publication of this zero-stamp failure must retry the repaired file on
	// the next authentication rather than denying forever.
	auth := &Auth{path: path}
	auth.snapshot.Store(&authSnapshot{err: err, stamp: stamp})
	if _, err := auth.authenticate("Bearer "+token, "operations:read"); err != nil {
		t.Fatalf("the repaired store stayed denied: %v", err)
	}
}

func TestUnstableReadReturnsRetryableStamp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	first := encodeStore(t, []Principal{reloadPrincipal(token, "first", "operations:read")})
	second := encodeStore(t, []Principal{
		reloadPrincipal(token, "first", "operations:read"),
		reloadPrincipal(reloadToken, "second", "operations:read"),
	})
	writeStoreAtomic(t, path, first)
	var writeErr error
	_, stamp, err := readAuthFileWith(path, func() {
		// Rewrite in place so the already-open descriptor's post-read stat sees
		// a different version than the bytes that were parsed.
		writeErr = os.WriteFile(path, second, 0o600)
	})
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if err == nil {
		t.Fatal("a file changed during its read was accepted")
	}
	if stamp.present {
		t.Fatalf("an unstable read cached the post-write stamp: %+v", stamp)
	}
	// The zero stamp must force the next authentication to retry the now
	// valid file rather than leaving it permanently denied.
	auth := &Auth{path: path}
	auth.snapshot.Store(&authSnapshot{err: err, stamp: stamp})
	if _, err := auth.authenticate("Bearer "+reloadToken, "operations:read"); err != nil {
		t.Fatalf("a retryable unstable-read failure stayed poisoned: %v", err)
	}
}

func TestAuthConcurrentReloadIsRaceSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	first := encodeStore(t, []Principal{reloadPrincipal(token, "first", "operations:read")})
	second := encodeStore(t, []Principal{reloadPrincipal(reloadToken, "second", "operations:read")})
	writeStoreAtomic(t, path, first)
	auth, err := LoadAuth(path)
	if err != nil {
		t.Fatal(err)
	}
	headers := []string{"Bearer " + token, "Bearer " + reloadToken}
	done := make(chan struct{})
	writeErr := make(chan error, 1)
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			raw := second
			if i%2 == 0 {
				raw = first
			}
			if err := writeStoreFile(path, raw, 0o600); err != nil {
				writeErr <- err
				return
			}
		}
	}()
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func(header string) {
			defer readers.Done()
			for j := 0; j < 500; j++ {
				_, err := auth.authenticate(header, "operations:read")
				if err != nil && contract.Classify(err).Code != contract.Unauthenticated {
					t.Errorf("concurrent authentication returned %v", err)
					return
				}
			}
		}(headers[i%len(headers)])
	}
	readers.Wait()
	close(done)
	writer.Wait()
	select {
	case err := <-writeErr:
		t.Fatal(err)
	default:
	}
}

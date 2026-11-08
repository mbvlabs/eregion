package integrations

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"golang.org/x/sys/unix"
)

func TestCredentialFileAuthorizationDistinctUIDAndGID(t *testing.T) {
	const serviceGID = uint32(23456)
	rootPolicy := credentialFilePolicy{CallerUID: 0, CallerGID: 0, ServiceGID: serviceGID, ServiceGIDKnown: true}
	workerPolicy := credentialFilePolicy{CallerUID: 34567, CallerGID: serviceGID, ServiceGID: serviceGID, ServiceGIDKnown: true}
	cases := []struct {
		name    string
		mode    os.FileMode
		fileUID uint32
		fileGID uint32
		policy  credentialFilePolicy
		want    bool
	}{
		{name: "root reads root-owned service-group 0640", mode: 0o640, fileUID: 0, fileGID: serviceGID, policy: rootPolicy, want: true},
		{name: "worker reads same root-owned service-group 0640", mode: 0o640, fileUID: 0, fileGID: serviceGID, policy: workerPolicy, want: true},
		{name: "worker rejected for another group", mode: 0o640, fileUID: 0, fileGID: serviceGID + 1, policy: workerPolicy, want: false},
		{name: "root rejected when group is not service group", mode: 0o640, fileUID: 0, fileGID: serviceGID + 1, policy: rootPolicy, want: false},
		{name: "root rejected when service group is unresolved", mode: 0o640, fileUID: 0, fileGID: serviceGID, policy: credentialFilePolicy{CallerUID: 0, CallerGID: 0}, want: false},
		{name: "root rejected for gid zero service group", mode: 0o640, fileUID: 0, fileGID: 0, policy: credentialFilePolicy{CallerUID: 0, CallerGID: 0, ServiceGIDKnown: true}, want: false},
		{name: "worker rejected for primary gid zero", mode: 0o640, fileUID: 0, fileGID: 0, policy: credentialFilePolicy{CallerUID: 34567, CallerGID: 0, ServiceGIDKnown: true}, want: false},
		{name: "root reads root-owned 0600", mode: 0o600, fileUID: 0, fileGID: 0, policy: rootPolicy, want: true},
		{name: "worker allowed owner-only 0600", mode: 0o600, fileUID: 34567, fileGID: serviceGID, policy: workerPolicy, want: true},
		{name: "worker rejected for another owner's 0600", mode: 0o600, fileUID: 0, fileGID: 0, policy: workerPolicy, want: false},
		{name: "root rejected for service-user-owned 0600", mode: 0o600, fileUID: 34567, fileGID: serviceGID, policy: rootPolicy, want: false},
		{name: "group-writable 0660 rejected", mode: 0o660, fileUID: 0, fileGID: serviceGID, policy: workerPolicy, want: false},
		{name: "world-readable 0644 rejected", mode: 0o644, fileUID: 0, fileGID: serviceGID, policy: workerPolicy, want: false},
		{name: "owner-read-only 0400 rejected by policy", mode: 0o400, fileUID: 34567, fileGID: serviceGID, policy: workerPolicy, want: false},
		{name: "setuid bit rejected", mode: 0o640 | os.ModeSetuid, fileUID: 0, fileGID: serviceGID, policy: rootPolicy, want: false},
		{name: "setgid bit rejected", mode: 0o640 | os.ModeSetgid, fileUID: 0, fileGID: serviceGID, policy: rootPolicy, want: false},
		{name: "sticky bit rejected", mode: 0o640 | os.ModeSticky, fileUID: 0, fileGID: serviceGID, policy: rootPolicy, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := credentialFileAllowed(test.mode, test.fileUID, test.fileGID, test.policy); got != test.want {
				t.Fatalf("credentialFileAllowed(%v, uid=%d, gid=%d) = %t, want %t", test.mode, test.fileUID, test.fileGID, got, test.want)
			}
		})
	}
}

func TestCredentialDirectoryAuthorizationDistinctUID(t *testing.T) {
	const serviceGID = uint32(23456)
	workerPolicy := credentialFilePolicy{CallerUID: 34567, CallerGID: serviceGID, ServiceGID: serviceGID, ServiceGIDKnown: true}
	rootPolicy := credentialFilePolicy{CallerUID: 0, CallerGID: 0, ServiceGID: serviceGID, ServiceGIDKnown: true}
	cases := []struct {
		name   string
		mode   uint32
		uid    uint32
		policy credentialFilePolicy
		want   bool
	}{
		{name: "root-owned 0750 service directory", mode: unix.S_IFDIR | 0o750, uid: 0, policy: workerPolicy, want: true},
		{name: "root-owned sticky world-writable ancestor", mode: unix.S_IFDIR | unix.S_ISVTX | 0o777, uid: 0, policy: workerPolicy, want: true},
		{name: "caller-owned group-writable ancestor rejected", mode: unix.S_IFDIR | 0o775, uid: 34567, policy: workerPolicy, want: false},
		{name: "caller-owned private directory", mode: unix.S_IFDIR | 0o700, uid: 34567, policy: workerPolicy, want: true},
		{name: "another owner's group-writable directory", mode: unix.S_IFDIR | 0o775, uid: 45678, policy: workerPolicy, want: false},
		{name: "world-writable non-sticky directory", mode: unix.S_IFDIR | 0o777, uid: 34567, policy: workerPolicy, want: false},
		{name: "non-root ancestor for root reader", mode: unix.S_IFDIR | 0o700, uid: 34567, policy: rootPolicy, want: false},
		{name: "non-directory rejected", mode: unix.S_IFREG | 0o750, uid: 0, policy: workerPolicy, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := credentialDirectoryAllowed(test.mode, test.uid, test.policy); got != test.want {
				t.Fatalf("credentialDirectoryAllowed(%o, uid=%d) = %t, want %t", test.mode, test.uid, got, test.want)
			}
		})
	}
}

func TestReadSecretFileRejectsUnsafeAncestor(t *testing.T) {
	base := t.TempDir()
	unsafe := filepath.Join(base, "unsafe")
	if err := os.Mkdir(unsafe, 0o700); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(unsafe, "credentials")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(nested, "cloudflare-token")
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(path, 4096); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unsafe ancestor = %v", err)
	}
}

func TestReadSecretFileRejectsGroupWritableAncestorSubstitution(t *testing.T) {
	base := t.TempDir()
	ancestor := filepath.Join(base, "ancestor")
	if err := os.Mkdir(ancestor, 0o700); err != nil {
		t.Fatal(err)
	}
	approved := filepath.Join(base, "approved")
	if err := os.Mkdir(approved, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(approved, "cloudflare-token")
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A group member could replace this child entry when the ancestor is
	// group-writable. The ancestor policy must reject before following it.
	child := filepath.Join(ancestor, "credentials")
	if err := os.Symlink(approved, child); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(filepath.Join(child, "cloudflare-token"), 4096); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("group-writable ancestor substitution = %v", err)
	}
}

func TestReadSecretFileRejectsSymlinkedAncestor(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(realDir, "cloudflare-token")
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(filepath.Join(link, "cloudflare-token"), 4096); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("symlinked ancestor = %v", err)
	}
}

func TestReadSecretFileRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cloudflare-token")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadSecretFile(path, 4096)
		done <- err
	}()
	select {
	case err := <-done:
		if contract.Classify(err).Code != contract.PreconditionFailed {
			t.Fatalf("FIFO credential file = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FIFO credential open blocked instead of failing safely")
	}
}

func TestReadSecretFileRejectsGroupOrWorldWritableDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cloudflare-token")
	if err := os.WriteFile(path, []byte("token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(path, 4096); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("group-writable immediate credential directory = %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecretFile(path, 4096); contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("world-writable credential directory = %v", err)
	}
}

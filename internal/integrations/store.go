package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/mbvlabs/eregion/internal/contract"
)

// GrantStore persists devices grants. It is not a queue: operations remain in
// eregion_operations and River. The store only owns access-domain state.
type GrantStore interface {
	List(context.Context) ([]DeviceGrant, error)
	Get(context.Context, string) (DeviceGrant, bool, error)
	Put(context.Context, DeviceGrant) error
}

// MemoryGrantStore is test-only. Production must use FileGrantStore or an
// equivalent durable implementation.
type MemoryGrantStore struct {
	mu     sync.Mutex
	grants map[string]DeviceGrant
}

func NewMemoryGrantStore() *MemoryGrantStore {
	return &MemoryGrantStore{grants: map[string]DeviceGrant{}}
}

func (s *MemoryGrantStore) List(context.Context) ([]DeviceGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]DeviceGrant, 0, len(s.grants))
	for _, grant := range s.grants {
		result = append(result, copyGrant(grant))
	}
	return result, nil
}

func (s *MemoryGrantStore) Get(_ context.Context, id string) (DeviceGrant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	grant, ok := s.grants[id]
	return copyGrant(grant), ok, nil
}

func (s *MemoryGrantStore) Put(_ context.Context, grant DeviceGrant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grants[grant.ID] = copyGrant(grant)
	return nil
}

// FileGrantStore stores grants in one atomic JSON file with mode 0600. It is
// safe for one control-plane process; a multi-process deployment needs a shared
// database implementation of GrantStore.
type FileGrantStore struct {
	mu   sync.Mutex
	path string
}

func NewFileGrantStore(path string) (*FileGrantStore, error) {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || strings.Contains(path, "..") {
		return nil, contract.NewFault(contract.ValidationFailed, "access grant store path must be a safe absolute file", "Use a root-owned 0600 state file outside the web root.")
	}
	store := &FileGrantStore{path: filepath.Clean(path)}
	if _, err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *FileGrantStore) List(context.Context) ([]DeviceGrant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	grants, err := s.load()
	if err != nil {
		return nil, err
	}
	result := make([]DeviceGrant, 0, len(grants))
	for _, grant := range grants {
		result = append(result, copyGrant(grant))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (s *FileGrantStore) Get(_ context.Context, id string) (DeviceGrant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	grants, err := s.load()
	if err != nil {
		return DeviceGrant{}, false, err
	}
	grant, ok := grants[id]
	return copyGrant(grant), ok, nil
}

func (s *FileGrantStore) Put(_ context.Context, grant DeviceGrant) error {
	if !grantIDPattern.MatchString(grant.ID) {
		return contract.NewFault(contract.ValidationFailed, "invalid access grant ID", "Use a deterministic grant ID derived from the operation.")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	grants, err := s.load()
	if err != nil {
		return err
	}
	grants[grant.ID] = copyGrant(grant)
	return s.write(grants)
}

func (s *FileGrantStore) load() (map[string]DeviceGrant, error) {
	result := map[string]DeviceGrant{}
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, storeUnavailable(err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "access grant store is not a private regular file", "Use a root-owned 0600 regular file; symlinks and group-readable files are rejected.")
	}
	if info.Size() > 16<<20 {
		return nil, contract.NewFault(contract.PreconditionFailed, "access grant store is too large", "Inspect and archive the state file.")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, storeUnavailable(err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return result, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var grants []DeviceGrant
	if err := decoder.Decode(&grants); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "access grant store is corrupt", "Restore the state file from a known-good backup; do not guess grants.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, contract.NewFault(contract.Unavailable, "access grant store has trailing data", "Restore the state file from a known-good backup.")
	}
	for _, grant := range grants {
		if !grantIDPattern.MatchString(grant.ID) || !subjectPattern.MatchString(grant.Subject) || !deviceIDPattern.MatchString(grant.DeviceID) || len(grant.Routes) == 0 {
			return nil, contract.NewFault(contract.Unavailable, "access grant store contains an invalid record", "Restore the state file from a known-good backup.")
		}
		for _, route := range grant.Routes {
			if err := validateRoute(route); err != nil {
				return nil, contract.NewFault(contract.Unavailable, "access grant store contains an invalid route", "Restore the state file from a known-good backup.")
			}
		}
		result[grant.ID] = copyGrant(grant)
	}
	return result, nil
}

func (s *FileGrantStore) write(grants map[string]DeviceGrant) error {
	values := make([]DeviceGrant, 0, len(grants))
	for _, grant := range grants {
		values = append(values, copyGrant(grant))
	}
	sort.Slice(values, func(i, j int) bool { return values[i].ID < values[j].ID })
	raw, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode access grant state", "Do not report the grant as persisted.")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return storeUnavailable(err)
	}
	temp, err := os.CreateTemp(dir, ".access-grants-*")
	if err != nil {
		return storeUnavailable(err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return storeUnavailable(err)
	}
	if _, err := temp.Write(append(raw, '\n')); err != nil {
		_ = temp.Close()
		return storeUnavailable(err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return storeUnavailable(err)
	}
	if err := temp.Close(); err != nil {
		return storeUnavailable(err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		return storeUnavailable(err)
	}
	handle, err := os.Open(dir)
	if err != nil {
		return storeUnavailable(err)
	}
	defer handle.Close()
	if err := handle.Sync(); err != nil {
		return storeUnavailable(err)
	}
	return nil
}

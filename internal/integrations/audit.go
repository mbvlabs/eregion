// Package integrations owns audited external integrations: Cloudflare DNS
// reconciliation and private-access device grants. Both fail closed when
// authorization or audit is unavailable.
package integrations

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// AuditEntry is one security-relevant event. Detail must be built from validated
// identifiers and fixed text; never include credentials, request bodies, or
// upstream error bodies.
type AuditEntry struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Action  string    `json:"action"`
	Target  string    `json:"target"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
}

// AuditSink is mandatory for mutations and access decisions. An error aborts the
// operation before the external effect, or is returned after an effect whose
// completion could not be audited.
type AuditSink interface {
	Record(context.Context, AuditEntry) error
}

// MemoryAudit is useful for tests and short-lived adapters. It is not durable.
type MemoryAudit struct {
	mu      sync.Mutex
	entries []AuditEntry
}

func NewMemoryAudit() *MemoryAudit { return &MemoryAudit{} }

func (m *MemoryAudit) Record(_ context.Context, entry AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = append(m.entries, entry)
	return nil
}

func (m *MemoryAudit) Entries() []AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]AuditEntry, len(m.entries))
	copy(result, m.entries)
	return result
}

// FileAudit appends one JSON object per line and fsyncs each record. Files are
// created 0600. Rotation is an operator responsibility.
type FileAudit struct {
	mu   sync.Mutex
	path string
}

func NewFileAudit(path string) (*FileAudit, error) {
	if !filepath.IsAbs(path) {
		return nil, contract.NewFault(contract.ValidationFailed, "audit path must be absolute", "Use a root-owned path outside application data.")
	}
	return &FileAudit{path: filepath.Clean(path)}, nil
}

func (f *FileAudit) Record(_ context.Context, entry AuditEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode audit entry", "Do not perform the action.")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(f.path), 0750); err != nil {
		return contract.NewFault(contract.Unavailable, "audit directory is unavailable", "Do not perform the action.")
	}
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return contract.NewFault(contract.Unavailable, "audit log is unavailable", "Do not perform the action.")
	}
	defer file.Close()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return contract.NewFault(contract.Unavailable, "audit log write failed", "Do not perform the action.")
	}
	if err := file.Sync(); err != nil {
		return contract.NewFault(contract.Unavailable, "audit log sync failed", "Do not perform the action.")
	}
	return nil
}

// Authorizer decides whether an actor may perform an integration action. A nil
// or failing implementation denies.
type Authorizer interface {
	Authorize(context.Context, contract.Actor, string, string) error
}

// StaticAuthorizer grants named actions to exact actor subjects. It is an
// explicit allowlist, not a role hierarchy. "*" grants every action to that
// subject and should be reserved for a bootstrap admin.
type StaticAuthorizer struct {
	grants map[string]map[string]bool
}

func NewStaticAuthorizer(grants map[string][]string) (*StaticAuthorizer, error) {
	result := &StaticAuthorizer{grants: map[string]map[string]bool{}}
	for subject, actions := range grants {
		if subject == "" {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid authorization subject", "Use a nonempty authenticated actor subject.")
		}
		if len(actions) == 0 {
			return nil, contract.NewFault(contract.ValidationFailed, "empty authorization grant for "+subject, "Grant at least one action or omit the subject.")
		}
		result.grants[subject] = map[string]bool{}
		for _, action := range actions {
			if action == "" {
				return nil, contract.NewFault(contract.ValidationFailed, "empty authorization action for "+subject, "Use an explicit action name or *.")
			}
			result.grants[subject][action] = true
		}
	}
	return result, nil
}

func (s *StaticAuthorizer) Authorize(_ context.Context, actor contract.Actor, action, _ string) error {
	if s == nil {
		return forbidden()
	}
	if err := actor.Validate(); err != nil {
		return err
	}
	allowed := s.grants[actor.Subject]
	if allowed != nil && (allowed["*"] || allowed[action]) {
		return nil
	}
	return forbidden()
}

func forbidden() error {
	return contract.NewFault(contract.Forbidden, "integration action is not authorized", "Use an actor with an explicit grant for this action; authorization fails closed.")
}

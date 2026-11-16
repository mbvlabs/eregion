package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
)

// Workspace identity is installation metadata, not a tenant boundary. The
// database owns the one configured identity; node configuration and operator
// access files may mirror it but never decide it. An empty migrated table is
// explicitly unconfigured until initialization assigns its identity.
var (
	workspaceNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,62}[a-z0-9])?$`)
	workspaceIDPattern   = regexp.MustCompile(`^ws_[0-9a-f]{24}$`)
	// ErrWorkspaceSchemaMissing signals that the workspace migration has not run.
	ErrWorkspaceSchemaMissing = errors.New("workspace schema is missing")
)

// workspaceAssignmentLock serialises the singleton insert and replay without
// holding a table lock. It is intentionally unrelated to migrationLock.
const workspaceAssignmentLock int64 = 0x65726567776f726b // "eregwork"

// WorkspaceIdentity is the portable, non-secret installation identity. The
// zero value is the explicit unconfigured state; it is never treated as a
// guess at a workspace name or ID.
type WorkspaceIdentity struct {
	Configured bool   `json:"configured"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	// RegistryHost is the immutable public binding to the physical main host
	// that owns the private registry. Nil means the workspace predates the
	// binding and is explicitly unverified.
	RegistryHost *RegistryHostIdentity `json:"registry_host,omitempty"`
}

// ValidateWorkspaceName checks the human-readable slug before any database
// transaction or local state write.
func ValidateWorkspaceName(name string) error {
	if !workspaceNamePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid workspace name "+name,
			"Use 1–64 lowercase letters, digits, or hyphens, starting and ending with a letter or digit (for example thelab).")
	}
	return nil
}

// ValidateWorkspaceID checks the generated portable ID in bundles and access records.
func ValidateWorkspaceID(id string) error {
	if !workspaceIDPattern.MatchString(id) {
		return contract.NewFault(contract.ValidationFailed, "invalid workspace ID "+id,
			"Use the workspace ID from a current protected export; do not edit it by hand.")
	}
	return nil
}

// NewWorkspaceID generates the immutable portable ID at first assignment.
func NewWorkspaceID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", contract.NewFault(contract.Internal, "could not generate a workspace ID", "Retry the apply on the node.")
	}
	return "ws_" + hex.EncodeToString(raw[:]), nil
}

// workspaceReader is the typed read surface shared by the Store and StoreTx
// workspace views. It exposes no query or driver handle.
type workspaceReader interface {
	Get(context.Context) (entities.WorkspaceRecord, bool, error)
	SchemaExists(context.Context) (bool, error)
}

// ReadWorkspace reads the authoritative identity without inserting a row.
// A missing table is distinct from a migrated but empty table.
func ReadWorkspace(ctx context.Context, store *entities.Store) (WorkspaceIdentity, error) {
	if store == nil {
		return WorkspaceIdentity{}, contract.NewFault(contract.Internal, "workspace identity needs typed persistence", "Construct the entities store before reading the workspace.")
	}
	return readWorkspaceSchema(ctx, store.Workspace())
}

func readWorkspaceSchema(ctx context.Context, reader workspaceReader) (WorkspaceIdentity, error) {
	exists, err := reader.SchemaExists(ctx)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	if !exists {
		return WorkspaceIdentity{}, ErrWorkspaceSchemaMissing
	}
	record, found, err := reader.Get(ctx)
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	if !found {
		return WorkspaceIdentity{}, nil
	}
	return workspaceIdentity(record)
}

func workspaceIdentity(record entities.WorkspaceRecord) (WorkspaceIdentity, error) {
	if err := ValidateWorkspaceID(record.ID); err != nil {
		return WorkspaceIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the control database has an invalid workspace ID", "Inspect the workspace row; never rewrite applied migrations.")
	}
	if err := ValidateWorkspaceName(record.Name); err != nil {
		return WorkspaceIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the control database has an invalid workspace name", "Inspect the workspace row; never rewrite applied migrations.")
	}
	if !record.WorkspaceRegistryHost() {
		if record.WorkspaceRegistryHostPartial() {
			return WorkspaceIdentity{}, contract.NewFault(contract.PreconditionFailed,
				"the workspace registry host binding is incomplete",
				"Inspect the workspace row; missing bindings are unverified and are never inferred.")
		}
		return WorkspaceIdentity{Configured: true, ID: record.ID, Name: record.Name}, nil
	}
	binding := RegistryHostIdentity{
		NodeName:           strings.TrimSpace(*record.RegistryHostNode),
		HostKeyType:        strings.TrimSpace(*record.RegistryHostKeyType),
		HostKeyFingerprint: strings.TrimSpace(*record.RegistryHostKeyFingerprint),
		StateDir:           strings.TrimSpace(*record.RegistryHostStateDir),
	}
	if err := binding.Validate(); err != nil {
		return WorkspaceIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the control database has an invalid workspace registry host binding", err.Error())
	}
	return WorkspaceIdentity{Configured: true, ID: record.ID, Name: record.Name, RegistryHost: &binding}, nil
}

// AssignWorkspace implements the one-time compare-and-set assignment:
//
//   - an unconfigured installation gets a newly generated immutable ID;
//   - replaying the same name returns the same identity;
//   - a different name is refused and never overwrites the existing identity.
//
// The advisory transaction lock serialises assignment. INSERT ... ON CONFLICT
// is the compare-and-set that also handles a writer that did not take the lock.
func AssignWorkspace(ctx context.Context, store *entities.Store, name string, registryHost RegistryHostIdentity) (WorkspaceIdentity, error) {
	if store == nil {
		return WorkspaceIdentity{}, contract.NewFault(contract.Internal, "workspace identity needs typed persistence", "Construct the entities store before assigning the workspace.")
	}
	if err := ValidateWorkspaceName(name); err != nil {
		return WorkspaceIdentity{}, err
	}
	if err := registryHost.Validate(); err != nil {
		return WorkspaceIdentity{}, err
	}
	var identity WorkspaceIdentity
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := tx.Workspace().LockAssignment(ctx, workspaceAssignmentLock); err != nil {
			return err
		}
		id, err := NewWorkspaceID()
		if err != nil {
			return err
		}
		if _, err := tx.Workspace().Insert(ctx, entities.WorkspaceInsert{
			ID: id, Name: name,
			RegistryHostNode:           registryHost.NodeName,
			RegistryHostKeyType:        registryHost.HostKeyType,
			RegistryHostKeyFingerprint: registryHost.HostKeyFingerprint,
			RegistryHostStateDir:       registryHost.StateDir,
		}); err != nil {
			return err
		}
		identity, err = readWorkspaceSchema(ctx, tx.Workspace())
		if err != nil {
			return err
		}
		if !identity.Configured {
			return contract.NewFault(contract.Internal,
				"the workspace identity was not recorded", "Inspect the control database and retry the apply.")
		}
		if identity.Name != name {
			return workspaceNameConflict(identity.Name, name)
		}
		if identity.RegistryHost == nil {
			return contract.NewFault(contract.PreconditionFailed,
				"the existing workspace has no verified registry host binding",
				"The workspace remains unverified and is never backfilled. Use a fresh installation; do not edit the workspace row.")
		}
		if !identity.RegistryHost.Equal(registryHost) {
			fault := contract.NewFault(contract.Conflict,
				"the workspace registry host binding is already configured for another physical host",
				"The bound node, public SSH host key and canonical node state directory are immutable. Use the original host or a fresh installation.")
			fault.Details = map[string]any{
				"bound_node":  identity.RegistryHost.NodeName,
				"actual_node": registryHost.NodeName,
			}
			return fault
		}
		return nil
	})
	if err != nil {
		return WorkspaceIdentity{}, err
	}
	return identity, nil
}

func workspaceNameConflict(configured, requested string) error {
	fault := contract.NewFault(contract.Conflict,
		"the workspace identity is already configured as "+configured,
		"The workspace name and ID are immutable. Use the existing name "+configured+", or restore the intended database before assigning another workspace.")
	fault.Details = map[string]any{
		"configured_workspace": configured,
		"requested_workspace":  requested,
	}
	return fault
}

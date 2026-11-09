package vault

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
)

// ProviderCloudflare is the only provider credential scope implemented today.
// The scope is deliberately typed: it is not an application secret namespace
// and cannot be reached through the app/environment secret APIs.
const ProviderCloudflare = "cloudflare"

const (
	MaxProviderZones   = 200
	MaxProviderZoneLen = 253
)

var (
	providerPattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	providerZonePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	providerNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])$`)
)

// ProviderScope identifies one provider credential. Provider and zone ID are
// both required; a zone name is metadata and never an identity.
type ProviderScope struct {
	Provider string `json:"provider"`
	ZoneID   string `json:"zone_id"`
}

// NewProviderScope validates and returns a canonical provider scope.
func NewProviderScope(provider, zoneID string) (ProviderScope, error) {
	scope := ProviderScope{
		Provider: strings.ToLower(strings.TrimSpace(provider)),
		ZoneID:   strings.ToLower(strings.TrimSpace(zoneID)),
	}
	if err := scope.Validate(); err != nil {
		return ProviderScope{}, err
	}
	return scope, nil
}

// Validate checks the typed provider identity without inspecting any secret.
func (p ProviderScope) Validate() error {
	if !providerPattern.MatchString(p.Provider) {
		return contract.NewFault(contract.ValidationFailed, "invalid provider credential scope", "Use a lowercase provider name such as cloudflare.")
	}
	if p.Provider != ProviderCloudflare {
		return contract.NewFault(contract.ValidationFailed, "unsupported provider credential scope", "Only Cloudflare provider credentials are supported.")
	}
	if !providerZonePattern.MatchString(p.ZoneID) {
		return contract.NewFault(contract.ValidationFailed, "invalid provider zone ID", "Use the lowercase 32-character Cloudflare zone identifier.")
	}
	return nil
}

// Key returns a stable, non-secret diagnostic label.
func (p ProviderScope) Key() string { return p.Provider + "/" + p.ZoneID }

func (p ProviderScope) String() string { return p.Key() }

// ValidateProviderZoneName validates the non-secret zone name stored beside a
// provider credential.
func ValidateProviderZoneName(zoneName string) error {
	if len(zoneName) == 0 || len(zoneName) > MaxProviderZoneLen ||
		!providerNamePattern.MatchString(zoneName) || !strings.Contains(zoneName, ".") ||
		strings.Contains(zoneName, "..") {
		return contract.NewFault(contract.ValidationFailed, "invalid provider zone name", "Use a lowercase DNS name such as example.com.")
	}
	return nil
}

// ProviderMetadata is secret-free provider credential state. It can be placed
// in API results, logs and status output.
type ProviderMetadata struct {
	Provider       string         `json:"provider"`
	ZoneID         string         `json:"zone_id"`
	ZoneName       string         `json:"zone_name"`
	LatestRevision uint64         `json:"latest_revision"`
	Revision       RevisionRef    `json:"revision"`
	CreatedBy      contract.Actor `json:"created_by"`
	UpdatedBy      contract.Actor `json:"updated_by"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

// Scope returns the typed identity.
func (m ProviderMetadata) Scope() ProviderScope {
	return ProviderScope{Provider: m.Provider, ZoneID: m.ZoneID}
}

// Validate rejects malformed provider metadata before a caller trusts it.
func (m ProviderMetadata) Validate() error {
	if err := m.Scope().Validate(); err != nil {
		return err
	}
	if err := ValidateProviderZoneName(m.ZoneName); err != nil {
		return err
	}
	if m.LatestRevision == 0 || m.Revision.Revision != m.LatestRevision ||
		!isHexDigest(m.Revision.Hash) || m.Revision.KeyID == "" ||
		m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() ||
		m.CreatedBy.Validate() != nil || m.UpdatedBy.Validate() != nil {
		return contract.NewFault(contract.Unavailable, "API returned invalid provider credential metadata", "Check binary compatibility before retrying a provider mutation.")
	}
	return nil
}

// SetProviderCredential appends one immutable encrypted provider revision under
// the caller's transaction. expectedRevision is 0 only for the first
// configuration; every replacement must name the observed latest revision.
func (s *Store) SetProviderCredential(ctx context.Context, tx pgx.Tx, scope ProviderScope, zoneName string, value []byte, expectedRevision uint64, actor contract.Actor) (ProviderMetadata, error) {
	if tx == nil {
		return ProviderMetadata{}, contract.NewFault(contract.Internal, "provider credential mutation has no transaction", "Integrate provider credential storage through an explicit PostgreSQL transaction.")
	}
	if err := scope.Validate(); err != nil {
		return ProviderMetadata{}, err
	}
	zoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneName), "."))
	if err := ValidateProviderZoneName(zoneName); err != nil {
		return ProviderMetadata{}, err
	}
	if err := ValidateValue(value); err != nil {
		return ProviderMetadata{}, err
	}
	if err := validateActor(actor); err != nil {
		return ProviderMetadata{}, err
	}

	var latest uint64
	var existingName string
	err := tx.QueryRow(ctx, `
		SELECT latest_revision, zone_name
		FROM eregion_provider_credentials
		WHERE provider = $1 AND zone_id = $2
		FOR UPDATE`, scope.Provider, scope.ZoneID).Scan(&latest, &existingName)
	exists := true
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		exists = false
	case err != nil:
		return ProviderMetadata{}, databaseFault(err)
	}
	if exists && existingName != zoneName {
		return ProviderMetadata{}, contract.NewFault(contract.Conflict, "provider zone name cannot change for an existing zone ID", "Keep the configured zone name or remove the binding deliberately before using a different name.")
	}
	revision, create, err := providerNextRevision(scope, exists, latest, expectedRevision)
	if err != nil {
		return ProviderMetadata{}, err
	}
	ciphertext, keyID, err := s.ring.SealProvider(scope, revision, value)
	if err != nil {
		return ProviderMetadata{}, err
	}
	ciphertextHash := CiphertextSHA256(ciphertext)
	revisionHash := ProviderRevisionHash(scope, revision, keyID, ciphertext)

	if create {
		_, err = tx.Exec(ctx, `
			INSERT INTO eregion_provider_credentials
				(provider, zone_id, zone_name, latest_revision,
				 created_by_kind, created_by_subject, updated_by_kind, updated_by_subject)
			VALUES ($1,$2,$3,1,$4,$5,$4,$5)`,
			scope.Provider, scope.ZoneID, zoneName, actor.Kind, actor.Subject)
		if err != nil {
			if isUniqueViolation(err) {
				return ProviderMetadata{}, providerUniqueConflict(scope)
			}
			return ProviderMetadata{}, databaseFault(err)
		}
	} else {
		tag, err := tx.Exec(ctx, `
			UPDATE eregion_provider_credentials
			SET latest_revision = $4, updated_by_kind = $5, updated_by_subject = $6,
			    updated_at = clock_timestamp()
			WHERE provider = $1 AND zone_id = $2 AND latest_revision = $3`,
			scope.Provider, scope.ZoneID, expectedRevision, revision, actor.Kind, actor.Subject)
		if err != nil {
			return ProviderMetadata{}, databaseFault(err)
		}
		if tag.RowsAffected() != 1 {
			return ProviderMetadata{}, providerRevisionConflict(scope, expectedRevision, latest)
		}
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO eregion_provider_credential_revisions
			(provider, zone_id, revision, key_id, ciphertext, ciphertext_sha256,
			 revision_hash, created_by_kind, created_by_subject)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		scope.Provider, scope.ZoneID, revision, keyID, ciphertext,
		ciphertextHash, revisionHash, actor.Kind, actor.Subject)
	if err != nil {
		if isUniqueViolation(err) {
			return ProviderMetadata{}, providerRevisionConflict(scope, expectedRevision, latest)
		}
		return ProviderMetadata{}, databaseFault(err)
	}
	return providerMetadataTx(ctx, tx, scope)
}

// GetProviderCredential reads secret-free provider metadata.
func (s *Store) GetProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, error) {
	if err := scope.Validate(); err != nil {
		return ProviderMetadata{}, err
	}
	return providerMetadataQuery(ctx, s.pool, scope)
}

// ListProviderCredentials reads secret-free metadata for one provider. The
// list is bounded and ordered by zone name so a name reference is unambiguous.
func (s *Store) ListProviderCredentials(ctx context.Context, provider string, limit int) ([]ProviderMetadata, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if !providerPattern.MatchString(provider) || provider != ProviderCloudflare {
		return nil, contract.NewFault(contract.ValidationFailed, "unsupported provider credential scope", "Only Cloudflare provider credentials are supported.")
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > MaxProviderZones {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid provider credential list limit", "Use a limit from 1 to 200.")
	}
	rows, err := s.pool.Query(ctx, providerMetadataSelect+`
		WHERE s.provider = $1
		ORDER BY s.zone_name
		LIMIT $2`, provider, limit)
	if err != nil {
		return nil, databaseFault(err)
	}
	defer rows.Close()
	out := make([]ProviderMetadata, 0, limit)
	for rows.Next() {
		metadata, err := scanProviderMetadata(rows)
		if err != nil {
			return nil, databaseFault(err)
		}
		out = append(out, metadata)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseFault(err)
	}
	return out, nil
}

// ResolveProviderCredential decrypts the current revision for one operation.
// It always reads PostgreSQL; no caller may cache the plaintext across
// operations or process starts.
func (s *Store) ResolveProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, []byte, error) {
	if err := scope.Validate(); err != nil {
		return ProviderMetadata{}, nil, err
	}
	row, err := loadLatestProviderEncrypted(ctx, s.pool, scope)
	if err != nil {
		return ProviderMetadata{}, nil, err
	}
	plaintext, err := decryptProviderRow(s.ring, scope, &row)
	if err != nil {
		return ProviderMetadata{}, nil, err
	}
	return row.Metadata, plaintext, nil
}

// Service wrappers keep the setup and DNS adapters on the existing vault
// boundary without introducing a second credential system.

func (s *Service) GetProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, error) {
	return s.store.GetProviderCredential(ctx, scope)
}

func (s *Service) ListProviderCredentials(ctx context.Context, provider string, limit int) ([]ProviderMetadata, error) {
	return s.store.ListProviderCredentials(ctx, provider, limit)
}

func (s *Service) ResolveProviderCredential(ctx context.Context, scope ProviderScope) (ProviderMetadata, []byte, error) {
	return s.store.ResolveProviderCredential(ctx, scope)
}

const providerMetadataSelect = `
	SELECT s.provider, s.zone_id, s.zone_name, s.latest_revision,
	       s.created_by_kind, s.created_by_subject,
	       s.updated_by_kind, s.updated_by_subject,
	       s.created_at, s.updated_at,
	       r.revision, r.key_id, r.revision_hash, r.created_at
	FROM eregion_provider_credentials s
	JOIN eregion_provider_credential_revisions r
	  ON r.provider = s.provider AND r.zone_id = s.zone_id
	 AND r.revision = s.latest_revision`

const providerEncryptedSelect = `
	SELECT s.provider, s.zone_id, s.zone_name, s.latest_revision,
	       r.revision, r.key_id, r.ciphertext, r.ciphertext_sha256, r.revision_hash, r.created_at
	FROM eregion_provider_credentials s
	JOIN eregion_provider_credential_revisions r
	  ON r.provider = s.provider AND r.zone_id = s.zone_id
	 AND r.revision = s.latest_revision`

func providerMetadataQuery(ctx context.Context, q Querier, scope ProviderScope) (ProviderMetadata, error) {
	row := q.QueryRow(ctx, providerMetadataSelect+`
		WHERE s.provider = $1 AND s.zone_id = $2`, scope.Provider, scope.ZoneID)
	metadata, err := scanProviderMetadata(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProviderMetadata{}, providerNotFound(scope)
	}
	if err != nil {
		return ProviderMetadata{}, databaseFault(err)
	}
	return metadata, nil
}

func providerMetadataTx(ctx context.Context, tx pgx.Tx, scope ProviderScope) (ProviderMetadata, error) {
	return providerMetadataQuery(ctx, tx, scope)
}

func scanProviderMetadata(row scanner) (ProviderMetadata, error) {
	var metadata ProviderMetadata
	var createdAt, updatedAt, revisionCreated time.Time
	err := row.Scan(
		&metadata.Provider, &metadata.ZoneID, &metadata.ZoneName, &metadata.LatestRevision,
		&metadata.CreatedBy.Kind, &metadata.CreatedBy.Subject,
		&metadata.UpdatedBy.Kind, &metadata.UpdatedBy.Subject,
		&createdAt, &updatedAt,
		&metadata.Revision.Revision, &metadata.Revision.KeyID, &metadata.Revision.Hash, &revisionCreated,
	)
	if err != nil {
		return ProviderMetadata{}, err
	}
	metadata.CreatedAt = createdAt.UTC()
	metadata.UpdatedAt = updatedAt.UTC()
	metadata.Revision.CreatedAt = revisionCreated.UTC()
	return metadata, nil
}

type providerEncryptedRow struct {
	Metadata       ProviderMetadata
	KeyID          string
	Ciphertext     []byte
	CiphertextHash string
	RevisionHash   string
}

func loadLatestProviderEncrypted(ctx context.Context, q Querier, scope ProviderScope) (providerEncryptedRow, error) {
	row := q.QueryRow(ctx, providerEncryptedSelect+`
		WHERE s.provider = $1 AND s.zone_id = $2`, scope.Provider, scope.ZoneID)
	var out providerEncryptedRow
	var revisionCreated time.Time
	err := row.Scan(
		&out.Metadata.Provider, &out.Metadata.ZoneID, &out.Metadata.ZoneName, &out.Metadata.LatestRevision,
		&out.Metadata.Revision.Revision, &out.KeyID, &out.Ciphertext, &out.CiphertextHash,
		&out.RevisionHash, &revisionCreated,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return providerEncryptedRow{}, providerNotFound(scope)
	}
	if err != nil {
		return providerEncryptedRow{}, databaseFault(err)
	}
	out.Metadata.Revision.KeyID = out.KeyID
	out.Metadata.Revision.CreatedAt = revisionCreated.UTC()
	metadata, err := providerMetadataQuery(ctx, q, scope)
	if err != nil {
		return providerEncryptedRow{}, err
	}
	if metadata.LatestRevision != out.Metadata.Revision.Revision ||
		metadata.Revision.Hash != out.RevisionHash || metadata.Revision.KeyID != out.KeyID ||
		metadata.ZoneName != out.Metadata.ZoneName {
		return providerEncryptedRow{}, contract.NewFault(contract.Unavailable, "provider credential changed while it was read", "Retry the read; provider revisions are immutable but the latest pointer may advance concurrently.")
	}
	out.Metadata = metadata
	return out, nil
}

func decryptProviderRow(ring *KeyRing, scope ProviderScope, row *providerEncryptedRow) ([]byte, error) {
	if row.CiphertextHash != CiphertextSHA256(row.Ciphertext) {
		return nil, contract.NewFault(contract.PreconditionFailed, "provider credential ciphertext integrity check failed", "Restore the revision from backup; do not trust or re-encrypt modified ciphertext.")
	}
	expectedHash := ProviderRevisionHash(scope, row.Metadata.Revision.Revision, row.KeyID, row.Ciphertext)
	if expectedHash != row.RevisionHash {
		return nil, contract.NewFault(contract.PreconditionFailed, "provider credential revision binding check failed", "Restore the revision from backup; ciphertext cannot be moved between provider scopes or revisions.")
	}
	plaintext, keyID, err := ring.OpenProvider(scope, row.Metadata.Revision.Revision, row.Ciphertext)
	if err != nil {
		if IsCiphertextError(err) {
			return nil, contract.NewFault(contract.PreconditionFailed, "provider credential ciphertext failed authentication", "Restore the revision from backup and verify the master-key file; never log or retry modified ciphertext.")
		}
		return nil, err
	}
	if keyID != row.KeyID {
		clearBytes(plaintext)
		return nil, contract.NewFault(contract.PreconditionFailed, "provider credential key identity check failed", "Restore the revision from backup and verify the master-key file.")
	}
	row.Metadata.Revision.Hash = expectedHash
	return plaintext, nil
}

func providerNotFound(scope ProviderScope) *contract.Fault {
	fault := contract.NewFault(contract.NotFound, "provider credential was not found", "Configure it with eregion domain cloudflare configure, then retry.")
	fault.Details = map[string]any{"provider": scope.Provider, "zone_id": scope.ZoneID}
	return fault
}

func providerRevisionConflict(scope ProviderScope, expected, current uint64) *contract.Fault {
	fault := contract.NewFault(contract.Conflict, "provider credential revision does not match", "Re-read the provider credential status and retry with its latest revision; the existing credential was not changed.")
	fault.Details = map[string]any{
		"provider": scope.Provider, "zone_id": scope.ZoneID,
		"expected_revision": expected, "current_revision": current,
	}
	return fault
}

func providerUniqueConflict(scope ProviderScope) *contract.Fault {
	fault := contract.NewFault(contract.Conflict, "provider zone is already configured", "Read the provider credential status; a zone ID and zone name are both configured once.")
	fault.Details = map[string]any{"provider": scope.Provider, "zone_id": scope.ZoneID}
	return fault
}

// ProviderRevisionHash binds provider ciphertext to its exact scope and
// revision without using plaintext. It is deterministic across processes and
// safe to persist or log.
func ProviderRevisionHash(scope ProviderScope, revision uint64, keyID string, ciphertext []byte) string {
	hash := sha256.New()
	hash.Write([]byte("eregion-provider-credential-revision/v1"))
	writeHashField(hash, scope.Provider)
	writeHashField(hash, scope.ZoneID)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], revision)
	hash.Write(number[:])
	writeHashField(hash, keyID)
	writeHashField(hash, string(ciphertext))
	return hex.EncodeToString(hash.Sum(nil))
}

// providerNextRevision is the CAS decision used under the row lock. expected 0
// creates only when no row exists; otherwise the immutable latest revision is
// the guard.
func providerNextRevision(scope ProviderScope, exists bool, latest, expected uint64) (uint64, bool, error) {
	if !exists {
		if expected != 0 {
			return 0, false, providerRevisionConflict(scope, expected, 0)
		}
		return 1, true, nil
	}
	if expected == 0 || expected != latest {
		return 0, false, providerRevisionConflict(scope, expected, latest)
	}
	if latest == math.MaxUint64 {
		return 0, false, contract.NewFault(contract.Conflict, "provider credential revision space is exhausted", "Report the binary version; revision numbers cannot wrap.")
	}
	return latest + 1, false, nil
}

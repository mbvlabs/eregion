package integrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	ActionAccessGrant  = "access.grant"
	ActionAccessRevoke = "access.revoke"
	ActionAccessUse    = "access.use"

	// DefaultMaxGrantDuration bounds a private-access grant when the caller
	// does not configure a shorter one.
	DefaultMaxGrantDuration = 30 * 24 * time.Hour
)

type GrantSpec struct {
	Subject   string    `json:"subject"`
	DeviceID  string    `json:"device_id"`
	Routes    []string  `json:"routes"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Note      string    `json:"note,omitempty"`
}

type DeviceGrant struct {
	ID                 string     `json:"id"`
	Subject            string     `json:"subject"`
	DeviceID           string     `json:"device_id"`
	Routes             []string   `json:"routes"`
	CreatedBy          string     `json:"created_by"`
	CreatedByOperation string     `json:"created_by_operation"`
	CreatedAt          time.Time  `json:"created_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	RevokedBy          string     `json:"revoked_by,omitempty"`
	RevokedByOperation string     `json:"revoked_by_operation,omitempty"`
	Note               string     `json:"note,omitempty"`
}

type AccessDecision struct {
	Allowed bool   `json:"allowed"`
	GrantID string `json:"grant_id,omitempty"`
	Reason  string `json:"reason"`
}

type AccessConfig struct {
	AdminSubjects []string
	Audit         AuditSink
	// Store persists grants across process restarts. Use FileGrantStore in a
	// control plane and MemoryGrantStore only in tests.
	Store            GrantStore
	Now              func() time.Time
	MaxGrantDuration time.Duration
}

type AccessController struct {
	admins map[string]bool
	audit  AuditSink
	store  GrantStore
	now    func() time.Time
	maxAge time.Duration
}

var (
	deviceIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,127}$`)
	subjectPattern  = regexp.MustCompile(`^[^\x00-\x1f\x7f]{1,256}$`)
	grantIDPattern  = regexp.MustCompile(`^grant_[0-9a-f]{32}$`)
)

func NewAccessController(cfg AccessConfig) (*AccessController, error) {
	if cfg.Audit == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "access decisions require an audit sink", "Configure durable auditing; access fails closed.")
	}
	if cfg.Store == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "access grants require a persistent store", "Use NewFileGrantStore in production; MemoryGrantStore is test-only.")
	}
	if len(cfg.AdminSubjects) == 0 {
		return nil, contract.NewFault(contract.ValidationFailed, "access controller needs at least one admin subject", "Configure an explicit admin allowlist; access fails closed.")
	}
	maxAge := cfg.MaxGrantDuration
	if maxAge == 0 {
		maxAge = DefaultMaxGrantDuration
	}
	if maxAge < time.Hour || maxAge > 90*24*time.Hour {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid maximum access-grant duration", "Use 1 hour–90 days.")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	admins := map[string]bool{}
	for _, subject := range cfg.AdminSubjects {
		if !subjectPattern.MatchString(subject) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid access admin subject", "Use a nonempty authenticated actor subject of at most 256 bytes.")
		}
		admins[subject] = true
	}
	return &AccessController{admins: admins, audit: cfg.Audit, store: cfg.Store, now: now, maxAge: maxAge}, nil
}

// CreateGrant is the non-operation convenience path used by tests and local
// administration. Durable platform actions must use CreateGrantForOperation.
func (c *AccessController) CreateGrant(ctx context.Context, actor contract.Actor, spec GrantSpec) (DeviceGrant, error) {
	opID, err := contract.NewOperationID()
	if err != nil {
		return DeviceGrant{}, contract.NewFault(contract.Internal, "could not allocate an operation identity", "Inspect process entropy.")
	}
	return c.CreateGrantForOperation(ctx, actor, opID, spec)
}

// CreateGrantForOperation derives the grant ID from the durable operation ID, so
// a retry after a lost acknowledgement returns the original grant rather than
// creating a second one.
func (c *AccessController) CreateGrantForOperation(ctx context.Context, actor contract.Actor, opID contract.OperationID, spec GrantSpec) (DeviceGrant, error) {
	if err := actor.Validate(); err != nil {
		return DeviceGrant{}, err
	}
	if err := opID.Validate(); err != nil {
		return DeviceGrant{}, err
	}
	now := c.now().UTC()
	defaultedExpiry := spec.ExpiresAt.IsZero()
	normalized, err := normalizeGrantSpec(spec, now, c.maxAge)
	if err != nil {
		return DeviceGrant{}, err
	}
	grantID, err := grantIDForOperation(opID)
	if err != nil {
		return DeviceGrant{}, err
	}
	grant := DeviceGrant{
		ID: grantID, Subject: normalized.Subject, DeviceID: normalized.DeviceID,
		Routes: normalized.Routes, CreatedBy: actor.Subject, CreatedByOperation: string(opID),
		CreatedAt: now, ExpiresAt: normalized.ExpiresAt, Note: normalized.Note,
	}
	target := grant.Subject + "/" + grant.DeviceID
	if err := c.requireAdmin(ctx, actor, ActionAccessGrant, target); err != nil {
		return DeviceGrant{}, err
	}
	existing, found, err := c.store.Get(ctx, grantID)
	if err != nil {
		return DeviceGrant{}, storeUnavailable(err)
	}
	if found {
		if !sameGrantRequest(existing, grant, defaultedExpiry) {
			return DeviceGrant{}, contract.NewFault(contract.Conflict, "access grant operation ID already binds different input", "Submit a new operation instead of changing a retried grant.")
		}
		if err := c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: ActionAccessGrant, Target: target, Outcome: "replayed", Detail: grantID}); err != nil {
			return existing, err
		}
		return existing, nil
	}
	if err := c.audit.Record(ctx, AuditEntry{At: now, Actor: actor.Subject, Action: ActionAccessGrant, Target: target, Outcome: "intent", Detail: grantID}); err != nil {
		return DeviceGrant{}, err
	}
	if err := c.store.Put(ctx, grant); err != nil {
		return DeviceGrant{}, storeUnavailable(err)
	}
	if err := c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: ActionAccessGrant, Target: target, Outcome: "succeeded", Detail: grantID}); err != nil {
		// The grant is already durable. A retry with the same operation ID will
		// observe it and replay the audit instead of creating another grant.
		return grant, err
	}
	return grant, nil
}

func (c *AccessController) RevokeGrant(ctx context.Context, actor contract.Actor, id string) (DeviceGrant, error) {
	opID, err := contract.NewOperationID()
	if err != nil {
		return DeviceGrant{}, contract.NewFault(contract.Internal, "could not allocate an operation identity", "Inspect process entropy.")
	}
	return c.RevokeGrantForOperation(ctx, actor, opID, id)
}

// RevokeGrantForOperation records the operation ID that revoked the grant, so a
// retried revoke is a no-op rather than a second audit/effect.
func (c *AccessController) RevokeGrantForOperation(ctx context.Context, actor contract.Actor, opID contract.OperationID, id string) (DeviceGrant, error) {
	if err := actor.Validate(); err != nil {
		return DeviceGrant{}, err
	}
	if err := opID.Validate(); err != nil {
		return DeviceGrant{}, err
	}
	if !grantIDPattern.MatchString(id) {
		return DeviceGrant{}, contract.NewFault(contract.ValidationFailed, "invalid access grant ID", "Use the opaque ID returned when the grant was created.")
	}
	grant, found, err := c.store.Get(ctx, id)
	if err != nil {
		return DeviceGrant{}, storeUnavailable(err)
	}
	if !found {
		return DeviceGrant{}, contract.NewFault(contract.NotFound, "access grant not found", "List grants before revoking.")
	}
	if err := c.requireAdmin(ctx, actor, ActionAccessRevoke, grant.Subject+"/"+grant.DeviceID); err != nil {
		return grant, err
	}
	if grant.RevokedAt != nil {
		if grant.RevokedByOperation == string(opID) {
			if err := c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: ActionAccessRevoke, Target: grant.Subject + "/" + grant.DeviceID, Outcome: "replayed", Detail: id}); err != nil {
				return grant, err
			}
		}
		return grant, nil
	}
	now := c.now().UTC()
	if err := c.audit.Record(ctx, AuditEntry{At: now, Actor: actor.Subject, Action: ActionAccessRevoke, Target: grant.Subject + "/" + grant.DeviceID, Outcome: "intent", Detail: id}); err != nil {
		return grant, err
	}
	grant.RevokedAt = &now
	grant.RevokedBy = actor.Subject
	grant.RevokedByOperation = string(opID)
	if err := c.store.Put(ctx, grant); err != nil {
		return grant, storeUnavailable(err)
	}
	if err := c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: ActionAccessRevoke, Target: grant.Subject + "/" + grant.DeviceID, Outcome: "succeeded", Detail: id}); err != nil {
		return grant, err
	}
	return grant, nil
}

// Authorize evaluates one exact subject/device/route request. Missing, expired,
// revoked, or audit-failing decisions are denials. There is no wildcard route.
func (c *AccessController) Authorize(ctx context.Context, subject, deviceID, route string) (AccessDecision, error) {
	decision := AccessDecision{Reason: "no active grant"}
	if !subjectPattern.MatchString(subject) || !deviceIDPattern.MatchString(deviceID) || validateRoute(route) != nil {
		decision.Reason = "invalid access identity or route"
		return c.recordDecision(ctx, subject, deviceID, route, decision)
	}
	grants, err := c.store.List(ctx)
	if err != nil {
		decision.Reason = "grant store unavailable; access denied"
		return c.recordDecision(ctx, subject, deviceID, route, decision)
	}
	now := c.now().UTC()
	var matches []DeviceGrant
	for _, grant := range grants {
		if grant.Subject != subject || grant.DeviceID != deviceID || grant.RevokedAt != nil || !grant.ExpiresAt.After(now) {
			continue
		}
		for _, granted := range grant.Routes {
			if granted == route {
				matches = append(matches, grant)
				break
			}
		}
	}
	if len(matches) == 0 {
		return c.recordDecision(ctx, subject, deviceID, route, decision)
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].CreatedAt.After(matches[j].CreatedAt) })
	decision.Allowed = true
	decision.GrantID = matches[0].ID
	decision.Reason = "active grant"
	return c.recordDecision(ctx, subject, deviceID, route, decision)
}

func (c *AccessController) recordDecision(ctx context.Context, subject, deviceID, route string, decision AccessDecision) (AccessDecision, error) {
	outcome := "denied"
	if decision.Allowed {
		outcome = "allowed"
	}
	err := c.audit.Record(ctx, AuditEntry{
		At: c.now().UTC(), Actor: subject, Action: ActionAccessUse,
		Target: deviceID + "/" + route, Outcome: outcome, Detail: decision.GrantID,
	})
	if err != nil {
		decision.Allowed = false
		decision.GrantID = ""
		decision.Reason = "audit unavailable; access denied"
		return decision, err
	}
	return decision, nil
}

func (c *AccessController) requireAdmin(ctx context.Context, actor contract.Actor, action, target string) error {
	if c.admins[actor.Subject] {
		return nil
	}
	_ = c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "denied", Detail: "not an access admin"})
	return forbidden()
}

// ListGrants returns a stable snapshot. Empty filters return every grant.
func (c *AccessController) ListGrants(ctx context.Context, subject, deviceID string) ([]DeviceGrant, error) {
	grants, err := c.store.List(ctx)
	if err != nil {
		return nil, storeUnavailable(err)
	}
	result := make([]DeviceGrant, 0, len(grants))
	for _, grant := range grants {
		if subject != "" && grant.Subject != subject {
			continue
		}
		if deviceID != "" && grant.DeviceID != deviceID {
			continue
		}
		copyGrant := copyGrant(grant)
		result = append(result, copyGrant)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].ID < result[j].ID
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return result, nil
}

func copyGrant(grant DeviceGrant) DeviceGrant {
	grant.Routes = append([]string(nil), grant.Routes...)
	return grant
}

func normalizeGrantSpec(spec GrantSpec, now time.Time, maxAge time.Duration) (GrantSpec, error) {
	if !subjectPattern.MatchString(spec.Subject) {
		return spec, contract.NewFault(contract.ValidationFailed, "invalid access subject", "Use a nonempty authenticated actor subject of at most 256 bytes.")
	}
	if !deviceIDPattern.MatchString(spec.DeviceID) {
		return spec, contract.NewFault(contract.ValidationFailed, "invalid private device ID", "Use 1–128 lowercase letters, digits, dots, underscores, colons, or hyphens.")
	}
	if len(spec.Routes) == 0 || len(spec.Routes) > 32 {
		return spec, contract.NewFault(contract.ValidationFailed, "invalid access route count", "Grant 1–32 exact routes.")
	}
	grant := DeviceGrant{}
	if err := validateRoutes(&grant, spec.Routes); err != nil {
		return spec, err
	}
	spec.Routes = grant.Routes
	if len(spec.Note) > 256 || strings.ContainsAny(spec.Note, "\r\n\x00") {
		return spec, contract.NewFault(contract.ValidationFailed, "invalid access grant note", "Use at most 256 bytes without control characters.")
	}
	if spec.ExpiresAt.IsZero() {
		spec.ExpiresAt = now.Add(24 * time.Hour)
	}
	spec.ExpiresAt = spec.ExpiresAt.UTC()
	if !spec.ExpiresAt.After(now) {
		return spec, contract.NewFault(contract.ValidationFailed, "access grant expiry must be in the future", "Use a future expiry within the configured maximum.")
	}
	if maxAge > 0 && spec.ExpiresAt.Sub(now) > maxAge {
		return spec, contract.NewFault(contract.ValidationFailed, "access grant expiry is out of range", "Use a future expiry within the configured maximum (default 30 days).")
	}
	return spec, nil
}

func grantIDForOperation(opID contract.OperationID) (string, error) {
	if err := opID.Validate(); err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("eregion/access/grant/" + string(opID)))
	return "grant_" + hex.EncodeToString(sum[:16]), nil
}

func sameGrantRequest(existing, requested DeviceGrant, defaultedExpiry bool) bool {
	if existing.Subject != requested.Subject || existing.DeviceID != requested.DeviceID ||
		existing.CreatedBy != requested.CreatedBy || existing.CreatedByOperation != requested.CreatedByOperation ||
		existing.Note != requested.Note {
		return false
	}
	if !defaultedExpiry && !existing.ExpiresAt.Equal(requested.ExpiresAt) {
		return false
	}
	return sameRoutes(existing.Routes, requested.Routes)
}

func sameRoutes(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func storeUnavailable(err error) error {
	if fault, ok := err.(*contract.Fault); ok {
		return fault
	}
	return contract.NewFault(contract.Unavailable, "access grant store is unavailable", "Do not treat this as an access decision; repair the store and retry the same operation.")
}

func validateRoutes(grant *DeviceGrant, routes []string) error {
	seen := map[string]bool{}
	for _, route := range routes {
		if err := validateRoute(route); err != nil {
			return err
		}
		if seen[route] {
			return contract.NewFault(contract.ValidationFailed, "duplicate access route", "List each exact route once.")
		}
		seen[route] = true
	}
	grant.Routes = append([]string(nil), routes...)
	sort.Strings(grant.Routes)
	return nil
}

func validateRoute(route string) error {
	if len(route) == 0 || len(route) > 256 || route != strings.TrimSpace(route) || strings.ContainsAny(route, "\\\r\n\x00") || strings.Contains(route, "://") || strings.Contains(route, "*") {
		return contract.NewFault(contract.ValidationFailed, "invalid private access route", "Use an exact lowercase host, host/path, or CIDR without a scheme or wildcard.")
	}
	if strings.Contains(route, "/") {
		if _, _, err := net.ParseCIDR(route); err == nil {
			return nil
		}
	}
	if route != strings.ToLower(route) {
		return contract.NewFault(contract.ValidationFailed, "invalid private access route", "Routes are lowercase and matched exactly.")
	}
	return nil
}

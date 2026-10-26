package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
)

const (
	// MaxVaultRequestBytes bounds a credential mutation body. One 32 KiB value
	// is 43692 bytes in base64 plus JSON metadata.
	MaxVaultRequestBytes = 64 * 1024
)

// VaultScope separates metadata/decrypt reads from secret mutations.
type VaultScope string

const (
	VaultReadScope  VaultScope = "read"
	VaultWriteScope VaultScope = "write"
)

// The four secret capabilities are explicit action kinds. A credential grant
// binds one kind to a canonical env/<app>/<environment> target; the secret name
// is not part of the grant, so one grant covers every name in that environment.
const (
	VaultKindSecretSet     = "platform.secret.set"
	VaultKindSecretList    = "platform.secret.list"
	VaultKindSecretArchive = "platform.secret.archive"
	VaultKindSecretExport  = "platform.secret.export"
)

// VaultAccess carries the authenticated actor and the exact action-grant check.
// The grant check runs after the route/body namespace is validated so the target
// can never be supplied by an untrusted body before authentication.
type VaultAccess struct {
	Actor     contract.Actor
	authorize func(kind, target string) error
}

// NewVaultAccess is the external-mount hook. An external authenticator supplies
// the server-resolved actor and an authorize callback that enforces its own
// grants for kind/target. operations:submit/read alone must not be passed as an
// always-allow callback.
func NewVaultAccess(actor contract.Actor, authorize func(kind, target string) error) (VaultAccess, error) {
	if err := actor.Validate(); err != nil {
		return VaultAccess{}, err
	}
	if authorize == nil {
		return VaultAccess{}, contract.NewFault(contract.Internal, "secret authorization hook is missing", "Supply an explicit kind/target authorization callback.")
	}
	return VaultAccess{Actor: actor, authorize: authorize}, nil
}

// Authorize checks one secret capability for the resolved environment target.
func (a VaultAccess) Authorize(kind, target string) error {
	if a.authorize == nil {
		return contract.NewFault(contract.Internal, "secret authorization hook is missing", "Mount the vault handler with an explicit grant check.")
	}
	return a.authorize(kind, target)
}

// VaultAuthorizer authenticates one request and returns access with its grant
// hook. Authentication happens before request parsing; grant authorization
// happens after the namespace is validated.
type VaultAuthorizer func(*http.Request, VaultScope) (VaultAccess, error)

// VaultService is the transaction-owning secret boundary. vault.Service
// implements it; API tests use an in-memory implementation.
type VaultService interface {
	Set(context.Context, vault.Namespace, []byte, uint64, contract.Actor) (vault.Metadata, error)
	Archive(context.Context, vault.Namespace, uint64, contract.Actor) (vault.Metadata, error)
	Get(context.Context, vault.Namespace) (vault.Metadata, error)
	List(context.Context, string, string, int) ([]vault.Metadata, error)
	Export(context.Context, vault.Namespace) (vault.Metadata, []byte, error)
}

// NewVaultHandler returns the credential mutation/read adapter. Mount it after
// (or pass) authentication and before the generic API fallback. It never reads
// an actor from the body, and it refuses plaintext HTTP to a non-loopback peer.
func NewVaultHandler(service VaultService, authorize VaultAuthorizer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if service == nil || authorize == nil {
			writeVaultFailure(w, contract.NewFault(contract.Internal, "secret API is not configured", "Mount the vault handler with a store and authorizer."))
			return
		}
		if !plaintextTransportAllowed(r) {
			writeVaultFailure(w, contract.NewFault(contract.PreconditionFailed, "secret API requires TLS or a literal loopback client", "Use HTTPS; plaintext HTTP is allowed only to a loopback development listener."))
			return
		}
		status, data, err := dispatchVault(r, service, authorize)
		if err != nil {
			writeVaultFailure(w, err)
			return
		}
		body, err := json.Marshal(contract.Success("secrets API request", data, nil))
		if err != nil {
			writeVaultFailure(w, contract.NewFault(contract.Internal, "could not encode the secret API result", "Report the binary version."))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

// AuthVaultAuthorizer adapts the authenticated principal set. Reads require
// operations:read and mutations require operations:submit; either scope alone
// grants nothing. The returned access additionally requires an explicit action
// grant (or an administrative operator/system principal) for the exact
// platform.secret.* kind and env/<app>/<environment> target.
func AuthVaultAuthorizer(auth *Auth) VaultAuthorizer {
	return func(r *http.Request, scope VaultScope) (VaultAccess, error) {
		if auth == nil {
			return VaultAccess{}, contract.NewFault(contract.Internal, "API authentication is not configured", "Load the API credential file before serving secret routes.")
		}
		if len(r.Header.Values("Authorization")) != 1 {
			return VaultAccess{}, contract.NewFault(contract.Unauthenticated, "exactly one authorization header is required", "Supply one Bearer token.")
		}
		principal, err := auth.resolvePrincipal(r.Header.Get("Authorization"))
		if err != nil {
			return VaultAccess{}, err
		}
		required := "operations:read"
		if scope == VaultWriteScope {
			required = "operations:submit"
		}
		if !slices.Contains(principal.Scopes, required) {
			return VaultAccess{}, contract.NewFault(contract.Forbidden, "API credential lacks the required scope", "Use a credential explicitly authorized for this action.")
		}
		return NewVaultAccess(principal.Actor, func(kind, target string) error {
			return authorizePrincipalAction(principal, kind, target)
		})
	}
}

func dispatchVault(r *http.Request, service VaultService, authorize VaultAuthorizer) (int, any, error) {
	path := r.URL.Path
	if path == "/v1/secrets" {
		switch r.Method {
		case http.MethodPost:
			access, err := authorize(r, VaultWriteScope)
			if err != nil {
				return 0, nil, err
			}
			return setSecret(r, service, access)
		case http.MethodGet:
			access, err := authorize(r, VaultReadScope)
			if err != nil {
				return 0, nil, err
			}
			return listSecrets(r, service, access)
		default:
			return 0, nil, vaultNotFound()
		}
	}
	if path == "/v1/secrets/archive" {
		if r.Method != http.MethodPost {
			return 0, nil, vaultNotFound()
		}
		access, err := authorize(r, VaultWriteScope)
		if err != nil {
			return 0, nil, err
		}
		return archiveSecret(r, service, access)
	}
	const prefix = "/v1/secrets/"
	if !strings.HasPrefix(path, prefix) || strings.HasSuffix(path, "/") {
		return 0, nil, vaultNotFound()
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	for i := range parts {
		decoded, err := url.PathUnescape(parts[i])
		if err != nil || decoded == "" || strings.Contains(decoded, "/") {
			return 0, nil, vaultNotFound()
		}
		parts[i] = decoded
	}
	if len(parts) != 3 && len(parts) != 4 {
		return 0, nil, vaultNotFound()
	}
	namespace, err := vault.NewNamespace(parts[0], parts[1], parts[2])
	if err != nil {
		return 0, nil, err
	}
	access, err := authorize(r, VaultReadScope)
	if err != nil {
		return 0, nil, err
	}
	if len(parts) == 3 {
		if r.Method != http.MethodGet {
			return 0, nil, vaultNotFound()
		}
		if err := access.Authorize(VaultKindSecretList, secretTarget(namespace)); err != nil {
			return 0, nil, err
		}
		metadata, err := GetSecretMetadata(r.Context(), service, namespace)
		return http.StatusOK, metadata, err
	}
	if parts[3] != "export" || r.Method != http.MethodGet {
		return 0, nil, vaultNotFound()
	}
	if err := access.Authorize(VaultKindSecretExport, secretTarget(namespace)); err != nil {
		return 0, nil, err
	}
	metadata, value, err := ExportSecretValue(r.Context(), service, namespace)
	if err != nil {
		return 0, nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(value)
	for i := range value {
		value[i] = 0
	}
	return http.StatusOK, vaultExport{Secret: metadata, ValueBase64: encoded}, nil
}

type setSecretRequest struct {
	App              string  `json:"app"`
	Environment      string  `json:"environment"`
	Name             string  `json:"name"`
	ValueBase64      string  `json:"value_base64"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

func setSecret(r *http.Request, service VaultService, access VaultAccess) (int, any, error) {
	var request setSecretRequest
	if err := decodeVaultBody(r, &request); err != nil {
		return 0, nil, err
	}
	if request.ExpectedRevision == nil {
		return 0, nil, contract.NewFault(contract.InputRequired, "expected_revision is required", "Use 0 to create a new secret or the latest revision from metadata to rotate it.")
	}
	namespace, err := vault.NewNamespace(request.App, request.Environment, request.Name)
	if err != nil {
		return 0, nil, err
	}
	if err := access.Authorize(VaultKindSecretSet, secretTarget(namespace)); err != nil {
		return 0, nil, err
	}
	value, err := base64.StdEncoding.Strict().DecodeString(request.ValueBase64)
	if err != nil {
		return 0, nil, contract.NewFault(contract.ValidationFailed, "invalid secret value encoding", "Send value_base64 as standard base64; never place a plaintext secret in an action input.")
	}
	defer func() {
		for i := range value {
			value[i] = 0
		}
	}()
	metadata, err := SetSecretValue(r.Context(), service, access.Actor, namespace, value, *request.ExpectedRevision)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, metadata, nil
}

type archiveSecretRequest struct {
	App              string  `json:"app"`
	Environment      string  `json:"environment"`
	Name             string  `json:"name"`
	ExpectedRevision *uint64 `json:"expected_revision"`
}

func archiveSecret(r *http.Request, service VaultService, access VaultAccess) (int, any, error) {
	var request archiveSecretRequest
	if err := decodeVaultBody(r, &request); err != nil {
		return 0, nil, err
	}
	if request.ExpectedRevision == nil {
		return 0, nil, contract.NewFault(contract.InputRequired, "expected_revision is required", "Read secret metadata and pass its latest revision as the CAS guard.")
	}
	namespace, err := vault.NewNamespace(request.App, request.Environment, request.Name)
	if err != nil {
		return 0, nil, err
	}
	if err := access.Authorize(VaultKindSecretArchive, secretTarget(namespace)); err != nil {
		return 0, nil, err
	}
	metadata, err := ArchiveSecretValue(r.Context(), service, access.Actor, namespace, *request.ExpectedRevision)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, metadata, nil
}

func listSecrets(r *http.Request, service VaultService, access VaultAccess) (int, any, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, nil, contract.NewFault(contract.ValidationFailed, "invalid secret list query", "Use one app, one environment, and an optional decimal limit.")
	}
	limit := 100
	values := map[string]string{}
	for name, entries := range query {
		if (name != "app" && name != "environment" && name != "limit") || len(entries) != 1 {
			return 0, nil, contract.NewFault(contract.ValidationFailed, "invalid secret list query", "Use one app, one environment, and an optional decimal limit.")
		}
		values[name] = entries[0]
	}
	if value, ok := values["limit"]; ok {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > vault.MaxListLimit {
			return 0, nil, contract.NewFault(contract.ValidationFailed, "invalid secret list limit", "Use a limit from 1 to 200.")
		}
	}
	app, environment := values["app"], values["environment"]
	if app == "" || environment == "" {
		return 0, nil, contract.NewFault(contract.InputRequired, "app and environment are required", "List one environment's secret metadata; values are never listed.")
	}
	if err := access.Authorize(VaultKindSecretList, secretEnvTarget(app, environment)); err != nil {
		return 0, nil, err
	}
	metadata, err := ListSecretMetadata(r.Context(), service, app, environment, limit)
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, vaultList{Secrets: metadata}, nil
}

type vaultList struct {
	Secrets []vault.Metadata `json:"secrets"`
}

type vaultExport struct {
	Secret      vault.Metadata `json:"secret"`
	ValueBase64 string         `json:"value_base64"`
}

func decodeVaultBody(r *http.Request, target any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return contract.NewFault(contract.ValidationFailed, "JSON content type required", "Use Content-Type: application/json.")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxVaultRequestBytes+1))
	if err != nil || len(raw) > MaxVaultRequestBytes {
		return contract.NewFault(contract.ValidationFailed, "secret request body is unreadable or too large", "Limit a credential mutation to 64 KiB, including base64 encoding.")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil || decoder.Decode(new(any)) != io.EOF {
		return contract.NewFault(contract.ValidationFailed, "invalid secret request", "Provide only app, environment, name, value_base64, and expected_revision; identity comes from authentication.")
	}
	return nil
}

func writeVaultFailure(w http.ResponseWriter, err error) {
	body, marshalErr := json.Marshal(contract.Failure("secrets API request", err))
	if marshalErr != nil {
		body = []byte(`{"schema":"eregion/v1","ok":false,"command":"secrets API request","error":{"code":"internal_error","message":"an internal error occurred"}}`)
	}
	w.WriteHeader(vaultHTTPStatus(contract.Classify(err).Code))
	_, _ = w.Write(body)
}

func vaultHTTPStatus(code contract.Code) int {
	switch code {
	case contract.InputRequired, contract.Usage, contract.ValidationFailed, contract.InvalidOutputMode:
		return http.StatusBadRequest
	case contract.NotFound:
		return http.StatusNotFound
	case contract.Unauthenticated:
		return http.StatusUnauthorized
	case contract.Forbidden:
		return http.StatusForbidden
	case contract.Conflict:
		return http.StatusConflict
	case contract.PreconditionFailed, contract.ConfirmationRequired:
		return http.StatusPreconditionFailed
	case contract.Unavailable, contract.Cancelled:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func vaultNotFound() *contract.Fault {
	return contract.NewFault(contract.NotFound, "secret API endpoint not found", "Use POST /v1/secrets, POST /v1/secrets/archive, GET /v1/secrets, or GET /v1/secrets/{app}/{environment}/{name}.")
}

func plaintextTransportAllowed(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	host := r.RemoteAddr
	if split, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = split
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func secretTarget(namespace vault.Namespace) string {
	return secretEnvTarget(namespace.App, namespace.Environment)
}

func secretEnvTarget(app, environment string) string {
	return "env/" + app + "/" + environment
}

// The client helpers below use the existing authenticated request boundary. They
// are deliberately separate from operation submission: secret values never
// enter an operation input or River argument.

type setSecretClientRequest struct {
	App              string `json:"app"`
	Environment      string `json:"environment"`
	Name             string `json:"name"`
	ValueBase64      string `json:"value_base64"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

type archiveSecretClientRequest struct {
	App              string `json:"app"`
	Environment      string `json:"environment"`
	Name             string `json:"name"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

// SetSecret encrypts one revision on the server. expectedRevision 0 creates;
// otherwise it is the CAS guard for rotation.
func (c *Client) SetSecret(ctx context.Context, namespace vault.Namespace, value []byte, expectedRevision uint64) (vault.Metadata, error) {
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, err
	}
	if err := vault.ValidateValue(value); err != nil {
		return vault.Metadata{}, err
	}
	request := setSecretClientRequest{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
		ValueBase64: base64.StdEncoding.EncodeToString(value), ExpectedRevision: expectedRevision,
	}
	raw, err := c.request(ctx, http.MethodPost, "/v1/secrets", request, "")
	if err != nil {
		return vault.Metadata{}, err
	}
	return decodeVaultMetadata(raw)
}

// ArchiveSecret archives one namespace under a CAS revision guard.
func (c *Client) ArchiveSecret(ctx context.Context, namespace vault.Namespace, expectedRevision uint64) (vault.Metadata, error) {
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, err
	}
	request := archiveSecretClientRequest{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name, ExpectedRevision: expectedRevision,
	}
	raw, err := c.request(ctx, http.MethodPost, "/v1/secrets/archive", request, "")
	if err != nil {
		return vault.Metadata{}, err
	}
	return decodeVaultMetadata(raw)
}

// GetSecretMetadata reads secret-free metadata.
func (c *Client) GetSecretMetadata(ctx context.Context, namespace vault.Namespace) (vault.Metadata, error) {
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, err
	}
	raw, err := c.request(ctx, http.MethodGet, "/v1/secrets/"+secretPath(namespace), nil, "")
	if err != nil {
		return vault.Metadata{}, err
	}
	return decodeVaultMetadata(raw)
}

// ListSecrets reads one environment's metadata page. It never returns values.
func (c *Client) ListSecrets(ctx context.Context, app, environment string, limit int) ([]vault.Metadata, error) {
	if err := vault.ValidateApp(app); err != nil {
		return nil, err
	}
	if err := vault.ValidateEnvironment(environment); err != nil {
		return nil, err
	}
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > vault.MaxListLimit {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid secret list limit", "Use a limit from 1 to 200.")
	}
	query := url.Values{"app": {app}, "environment": {environment}, "limit": {strconv.Itoa(limit)}}
	raw, err := c.request(ctx, http.MethodGet, "/v1/secrets?"+query.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	var page vaultList
	if err := json.Unmarshal(raw, &page); err != nil || page.Secrets == nil || len(page.Secrets) > limit {
		return nil, invalidVaultData()
	}
	for _, metadata := range page.Secrets {
		if err := metadata.Validate(); err != nil || metadata.App != app || metadata.Environment != environment {
			return nil, invalidVaultData()
		}
	}
	return page.Secrets, nil
}

// ExportSecret returns plaintext for an explicit protected destination. Callers
// must never print the value; clear the returned slice after writing it.
func (c *Client) ExportSecret(ctx context.Context, namespace vault.Namespace) (vault.Metadata, []byte, error) {
	if err := namespace.Validate(); err != nil {
		return vault.Metadata{}, nil, err
	}
	raw, err := c.request(ctx, http.MethodGet, "/v1/secrets/"+secretPath(namespace)+"/export", nil, "")
	if err != nil {
		return vault.Metadata{}, nil, err
	}
	var response vaultExport
	if err := json.Unmarshal(raw, &response); err != nil {
		return vault.Metadata{}, nil, invalidVaultData()
	}
	if err := response.Secret.Validate(); err != nil || response.Secret.Namespace() != namespace {
		return vault.Metadata{}, nil, invalidVaultData()
	}
	value, err := base64.StdEncoding.Strict().DecodeString(response.ValueBase64)
	if err != nil || len(value) == 0 || len(value) > vault.MaxValueBytes {
		zeroVaultBytes(value)
		return vault.Metadata{}, nil, invalidVaultData()
	}
	return response.Secret, value, nil
}

func decodeVaultMetadata(raw json.RawMessage) (vault.Metadata, error) {
	var metadata vault.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata.Validate() != nil {
		return vault.Metadata{}, invalidVaultData()
	}
	return metadata, nil
}

func invalidVaultData() *contract.Fault {
	return contract.NewFault(contract.Unavailable, "API returned invalid secret metadata", "Check binary compatibility; do not assume a failed mutation was rejected.")
}

func secretPath(namespace vault.Namespace) string {
	return strings.Join([]string{url.PathEscape(namespace.App), url.PathEscape(namespace.Environment), url.PathEscape(namespace.Name)}, "/")
}

func zeroVaultBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"net"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
)

type Service struct {
	// entities is the mandatory typed durable persistence boundary. It owns
	// the one borrowed pool and never exposes a pool, transaction or driver.
	entities *entities.Store
	// logger is the runtime's existing logger. The candidate-capture safety
	// tail uses it only for bounded static failure categories; raw Docker,
	// database or provider errors never enter it.
	logger               *slog.Logger
	observeActionAttempt func(kind string, outcome string, duration time.Duration)
	// transport is the node-dispatch boundary. Local today; SSH/WireGuard later.
	transport node.Transport
	// localNode is the identity of the in-process local transport, if any.
	localNode string
	// resolver reaches the node a piece of work is placed on: the enrolled
	// node's own pinned SSH transport, or transport for the node it serves.
	resolver *NodeResolver
	// afterNode is a test-only seam between a node effect and its database
	// acknowledgement. Production leaves it nil.
	afterNode func(contract.OperationID, string) error
	// beforeResourceFinalize is a test-only seam between the last resource
	// step-boundary cancellation check and the operation-row-locked
	// finalization. Production leaves it nil.
	beforeResourceFinalize func()
	// beforeDeploymentFinalize is a test-only seam between the last deployment
	// cancellation check and the success transaction. Production leaves it nil.
	beforeDeploymentFinalize func()
	// beforeSubmissionCommit is a test-only seam at the end of the one platform
	// submission transaction, after the domain rows, River job and queued event
	// exist but before commit. Production leaves it nil.
	beforeSubmissionCommit func() error
	// deploymentCleanupEvidence is a test-only seam for the container-step
	// evidence read used before reconstructing cleanup names. Production leaves
	// it nil and reads eregion_operation_steps.
	deploymentCleanupEvidence func(context.Context, contract.OperationID, string) (bool, error)
	// beforeRetentionMark is a test-only seam between planning a retention run
	// and marking its release rows. Production leaves it nil.
	beforeRetentionMark func(context.Context, contract.OperationID)
	// retentionMark injects a mark failure in tests. Production leaves it nil.
	retentionMark func(context.Context, string) (bool, error)
	// routeSwitch and publicVerify are test-only seams for deployments that
	// run no Caddy on the test host. Production leaves them nil.
	routeSwitch  func(context.Context, platform.Environment, []string) error
	publicVerify func(context.Context, platform.Environment, string) error
	// routeSync is the test-only seam for deletions and route recovery that
	// run no Caddy on the test host. Production leaves it nil and writes the
	// real routes file (or the explicit serving override during recovery).
	routeSync func(context.Context) error
	// secrets resolves environment secret references into transient values. Nil
	// means referenced secrets fail honestly instead of being injected.
	secrets SecretResolver
	// vault seals submission-time webhook secrets in the operation admission
	// transaction. Nil fails that activation closed; ordinary submissions still
	// work without a vault.
	vault *vault.Service
	// sourceRoots is the symlink-resolved directory-build allowlist.
	sourceRoots []string
	// resolveGitRef resolves a git branch or tag to an exact commit before a
	// submission transaction opens. Tests replace it; production leaves it nil
	// and uses platform.ResolveGitRef.
	resolveGitRef func(context.Context, string, string, []byte) (string, error)
	// resolveArtifactFn resolves a private or public image selector to an
	// immutable manifest and expected config digest before the admission
	// transaction opens. Tests replace it with HTTP fixtures or a fake.
	resolveArtifactFn func(context.Context, artifactRequest) (resolvedArtifact, error)
	// imageIdentityOverrides is a test-only map of canonical references to
	// authenticated manifest/config identities. Production leaves it nil and
	// resolves through the registry.
	imageIdentityOverrides map[string][2]string
	// diagnosisImageResolver and diagnosisImageObserver are test-only seams for
	// the local Docker reads diagnosis performs. Production leaves them nil and
	// uses platform.ResolveDiagnosisImageID and platform.ObserveDiagnosisImage.
	diagnosisImageResolver func(context.Context, string) (string, string)
	diagnosisImageObserver func(context.Context, string) (*platform.DiagnosisImage, string)
	// registryHTTPClient builds the production registry client with an injected
	// test CA/connector. Nil uses registrymeta.NewHTTPClient.
	registryHTTPClient func(allowLoopback bool) *http.Client
	// registryRedirectHTTPClient is the test CA/connector client for bounded
	// config-blob redirects. Production leaves it nil (public-only client).
	registryRedirectHTTPClient *http.Client
	// registryLookupIP is the production-shaped DNS seam for redirect tests.
	registryLookupIP func(context.Context, string) ([]net.IPAddr, error)
	// redaction holds the worker's own secrets (control DSN, vault keyring
	// material and the secret values handed to one node step). Node events are
	// redacted with it before they are persisted.
	redaction *secrets.Registry
	// registryEndpoint is the node's actual configured loopback registry
	// endpoint, injected by node composition. It has no default: a missing
	// configuration fails a Git build instead of publishing elsewhere.
	registryEndpoint string
	// registryHostIdentity is the observed public identity of the physical main
	// host. Final metadata deletion and image upload verify it against the
	// immutable workspace binding on every operation; a nil identity fails
	// closed. The host lock path is derived only from the verified binding.
	registryHostIdentity *RegistryHostIdentity
	// humanFactor verifies a transient TOTP code from protect.Store inside the
	// submission transaction. A nil verifier fails every factor-gated action
	// closed; HTTP submissions therefore cannot perform one.
	humanFactor func(string) error
	// domainDNS is the domain-scoped Cloudflare planner/executor. Nil fails
	// managed DNS closed and leaves manual domain bindings unchanged.
	domainDNS *integrations.DomainDNS
}

// SecretResolver resolves opaque environment secret references without
// persisting values. Implementations must return only the requested keys.
type SecretResolver interface {
	Resolve(context.Context, map[string]string) (map[string]string, error)
}

// EnvironmentSecretResolver is the authoritative form: the deployment's app and
// environment bind the namespace, so a reference to another environment is
// refused. A resolver that implements it is always preferred over Resolve.
type EnvironmentSecretResolver interface {
	ResolveEnvironment(
		ctx context.Context,
		app, environment string,
		refs map[string]string,
	) (map[string]string, error)
}

// EnvironmentSecretSnapshotResolver resolves revision secret names (or the
// transitional pinned refs) and returns the exact non-secret revision identities.
type EnvironmentSecretSnapshotResolver interface {
	ResolveEnvironmentSnapshot(
		ctx context.Context,
		app, environment string,
		names []string,
		refs map[string]string,
	) (map[string]string, map[string]vault.RevisionRef, error)
}

// redact removes the worker's own secrets from ordinary output before it is
// persisted. A nil registry is a no-op.
func (s *Service) redact(text string) string {
	if s.redaction == nil {
		return text
	}
	return s.redaction.Redact(text)
}

// registerSecretValues adds the values handed to one node step (environment
// secrets and a git read credential) to the worker's redaction registry.
func (s *Service) registerSecretValues(values map[string]string) {
	if s.redaction == nil || len(values) == 0 {
		return
	}
	list := make([]string, 0, len(values))
	for _, value := range values {
		list = append(list, value)
	}
	s.redaction.Add(list...)
}

func (s *Service) buildSourceRoots() []string {
	if len(s.sourceRoots) > 0 {
		return s.sourceRoots
	}
	return platform.BuildSourceRoots()
}

var _ operations.Service = (*Service)(nil)

const columns = `id, kind, target, actor_kind, actor_subject, state, attempt, last_sequence, error_code, created_at, updated_at`

func scanOperation(row pgx.Row) (contract.Operation, error) {
	var op contract.Operation
	err := row.Scan(
		&op.ID,
		&op.Kind,
		&op.Target,
		&op.Actor.Kind,
		&op.Actor.Subject,
		&op.State,
		&op.Attempt,
		&op.LastSequence,
		&op.ErrorCode,
		&op.CreatedAt,
		&op.UpdatedAt,
	)
	return op, err
}

func (s *Service) Submit(
	ctx context.Context,
	sub operations.Submission,
) (contract.Operation, error) {
	result, err := s.SubmitWithReceipt(ctx, sub)
	return result.Operation, err
}

// SubmitWithReceipt returns the authoritative admission receipt. A replay
// never performs first-submission effects and WebhookSecretActivated is false.
func (s *Service) SubmitWithReceipt(
	ctx context.Context,
	sub operations.Submission,
) (operations.SubmissionResult, error) {
	var zero operations.SubmissionResult
	// This operation may only be admitted by the dedicated trusted boundary;
	// registering its normalizer later must not make generic submit authorize it.
	if sub.Kind == platform.KindDefinitionApply {
		return zero, contract.NewFault(
			contract.ValidationFailed,
			"definition apply is not available through generic submission",
			"Use the trusted operator boundary.",
		)
	}
	if err := sub.Actor.Validate(); err != nil {
		return zero, err
	}
	if err := contract.ValidateIdempotencyKey(sub.IdempotencyKey); err != nil {
		return zero, err
	}
	if platform.IsKind(sub.Kind) {
		result, err := s.submitPlatformWithReceipt(ctx, sub)
		if err == nil && result.Replay {
			err = s.populateActionResult(ctx, &result.Operation)
		}
		return result, err
	}
	if sub.Kind != operations.TestKind || sub.Target != operations.TestTarget ||
		sub.Confirmation != "" {
		return zero, contract.NewFault(
			contract.ValidationFailed,
			"unsupported operation or target",
			"Only diagnostic.test on control-plane is implemented; it needs no confirmation.",
		)
	}
	input, err := operations.NormalizeTestInput(sub.Input)
	if err != nil {
		return zero, err
	}
	canonical, _ := json.Marshal(input)
	hash := sha256.Sum256(canonical)
	inputHash := hex.EncodeToString(hash[:])
	id, err := contract.NewOperationID()
	if err != nil {
		return zero, err
	}
	key := entities.OperationKey{
		Kind: sub.Kind, Target: sub.Target, Actor: sub.Actor, IdempotencyKey: sub.IdempotencyKey,
	}
	var submitted operations.SubmissionResult
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		inserted, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: id, Key: key, InputHash: inputHash, Input: canonical,
		})
		if err != nil {
			return txStorageError(err)
		}
		if !inserted {
			identity, found, err := tx.Operations().Identity(ctx, key)
			if err != nil {
				return txStorageError(err)
			}
			if !found {
				return unavailable()
			}
			if identity.InputHash != inputHash {
				return idempotencyInputConflict()
			}
			op, found, err := tx.Operations().Get(ctx, identity.ID)
			if err != nil {
				return txStorageError(err)
			}
			if !found {
				return unavailable()
			}
			submitted = operations.SubmissionResult{Operation: op, Replay: true}
			return nil
		}
		jobID, err := tx.Jobs().EnqueueDiagnostic(ctx, id)
		if err != nil {
			return txStorageError(err)
		}
		if err := tx.Operations().SetJob(ctx, id, jobID); err != nil {
			return txStorageError(err)
		}
		op, found, err := tx.Operations().Get(ctx, id)
		if err != nil {
			return txStorageError(err)
		}
		if !found {
			return unavailable()
		}
		if err := appendEvent(ctx, tx, &op, contract.Event{
			Kind:    "state",
			State:   contract.Queued,
			Message: "Diagnostic operation accepted durably.",
		}); err != nil {
			return err
		}
		if s.beforeSubmissionCommit != nil {
			if err := s.beforeSubmissionCommit(); err != nil {
				return err
			}
		}
		submitted = operations.SubmissionResult{Operation: op}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return zero, unavailable(err)
		}
		var fault *contract.Fault
		if errors.As(err, &fault) {
			return zero, err
		}
		return zero, unavailable(err)
	}
	if submitted.Replay {
		if err := s.populateActionResult(ctx, &submitted.Operation); err != nil {
			return zero, err
		}
	}
	return submitted, nil
}

func (s *Service) Get(
	ctx context.Context,
	authority operations.Authority,
	id contract.OperationID,
) (contract.Operation, error) {
	actor, all, err := operations.ResolveAuthority(authority)
	if err != nil {
		return contract.Operation{}, err
	}
	if err := id.Validate(); err != nil {
		return contract.Operation{}, err
	}
	var (
		op    contract.Operation
		found bool
	)
	if all {
		op, found, err = s.entities.Operations().Get(ctx, id)
	} else {
		op, found, err = s.entities.Operations().GetForActor(ctx, id, actor)
	}
	if err != nil {
		if ctx.Err() != nil {
			return op, ctx.Err()
		}
		return op, unavailable(err)
	}
	if !found {
		return op, contract.NewFault(
			contract.NotFound,
			"operation not found",
			"Check the ID and authenticated identity.",
		)
	}
	if err := s.populateActionResult(ctx, &op); err != nil {
		return op, err
	}
	return op, nil
}

func (s *Service) Events(
	ctx context.Context,
	authority operations.Authority,
	cursor contract.Cursor,
	limit int,
) (operations.EventPage, error) {
	page := operations.EventPage{Events: []contract.Event{}, Cursor: cursor}
	if limit < 1 || limit > 200 {
		return page, contract.NewFault(
			contract.ValidationFailed,
			"event limit out of range",
			"Use an event limit from 1 to 200.",
		)
	}
	op, err := s.Get(ctx, authority, cursor.OperationID)
	if err != nil {
		return page, err
	}
	if cursor.After > op.LastSequence {
		return page, contract.NewFault(
			contract.PreconditionFailed,
			"event cursor is ahead of the operation",
			"Refresh the operation and resume from a previously observed cursor.",
		)
	}
	// No event pruning. A missing retained event is an explicit gap, not silent loss.
	events, err := s.entities.Operations().Events(ctx, cursor, op.LastSequence, limit)
	if err != nil {
		if ctx.Err() != nil {
			return page, ctx.Err()
		}
		return page, unavailable(err)
	}
	for _, event := range events {
		if event.Sequence != page.Cursor.After+1 {
			return page, contract.NewFault(
				contract.PreconditionFailed,
				"event history has a retention gap",
				"Refresh the snapshot; do not silently skip missing history.",
			)
		}
		page.Events = append(page.Events, event)
		page.Cursor.After = event.Sequence
	}
	if len(page.Events) == 0 && cursor.After < op.LastSequence {
		return page, contract.NewFault(
			contract.PreconditionFailed,
			"event history is unavailable",
			"Refresh the snapshot; retained history is incomplete.",
		)
	}
	return page, nil
}

func (s *Service) RequestCancel(
	ctx context.Context,
	authority operations.Authority,
	id contract.OperationID,
) (contract.Operation, error) {
	if _, err := s.Get(ctx, authority, id); err != nil {
		return contract.Operation{}, err
	}
	return s.mutate(ctx, id, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State.Terminal() || op.State == contract.Cancelling {
			return nil
		}
		return transition(
			ctx,
			tx,
			op,
			contract.Cancelling,
			"",
			"Cancellation requested; waiting for a worker safe boundary.",
		)
	})
}

// mutate holds a row lock only for a short database mutation, never during work.
func (s *Service) mutate(
	ctx context.Context,
	id contract.OperationID,
	change func(*entities.StoreTx, *contract.Operation) error,
) (contract.Operation, error) {
	var op contract.Operation
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, found, err := tx.Operations().Lock(ctx, id)
		if err != nil {
			return txStorageError(err)
		}
		if !found {
			return unavailable()
		}
		op = current
		return change(tx, &op)
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return op, unavailable(err)
		}
		return op, err
	}
	s.observeCommittedActionResult(op)
	return op, nil
}

// mutateOwned is mutate with an attempt fence. A stale or terminal operation is
// returned unchanged and change does not run. River's completion query is not
// attempt-fenced, so every worker domain write must pass through here.
func (s *Service) mutateOwned(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	change func(*entities.StoreTx, *contract.Operation) error,
) (contract.Operation, bool, error) {
	var op contract.Operation
	owned := false
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, found, err := tx.Operations().Lock(ctx, id)
		if err != nil {
			return txStorageError(err)
		}
		if !found {
			return unavailable()
		}
		op = current
		if op.State.Terminal() || op.Attempt != attempt {
			return nil
		}
		if err := change(tx, &op); err != nil {
			return err
		}
		owned = true
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return op, false, unavailable(err)
		}
		return op, false, err
	}
	if owned {
		s.observeCommittedActionResult(op)
	}
	return op, owned, nil
}

// observeCommittedActionResult must be called only after the transaction
// which appended op.LatestResult has committed successfully.
// txStorageError preserves an existing contract fault or context result and
// classifies an untyped persistence failure as unavailable.
func txStorageError(err error) error {
	if err == nil {
		return nil
	}
	var fault *contract.Fault
	if errors.As(err, &fault) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return unavailable(err)
}

func (s *Service) observeCommittedActionResult(op contract.Operation) {
	if op.LatestResult == nil || op.LatestResult.Attempt == 0 || s.observeActionAttempt == nil {
		return
	}
	r := op.LatestResult
	outcome := string(r.Status)
	if r.Status == contract.Failed && !op.State.Terminal() {
		outcome = "retrying"
	}
	s.observeActionAttempt(op.Kind, outcome, r.CompletedAt.Sub(r.StartedAt))
}

// transition moves the operation to a new state. A failed transition carries
// the machine-readable error code that the operation snapshot and the state
// event expose, so a caller never has to read the message text.
func transition(
	ctx context.Context,
	tx *entities.StoreTx,
	op *contract.Operation,
	state contract.State,
	code contract.Code,
	message string,
) error {
	if err := contract.ValidateTransition(op.State, state); err != nil {
		return err
	}
	if err := tx.Operations().SetState(ctx, op.ID, state, code); err != nil {
		return unavailable(err)
	}
	op.State = state
	op.ErrorCode = code
	return appendEvent(
		ctx,
		tx,
		op,
		contract.Event{Kind: "state", State: state, ErrorCode: code, Message: message},
	)
}

func appendEvent(
	ctx context.Context,
	tx *entities.StoreTx,
	op *contract.Operation,
	event contract.Event,
) error {
	event.OperationID, event.Sequence, event.Attempt = op.ID, op.LastSequence+1, op.Attempt
	event.At = time.Now().UTC()
	// All terminalization paths use state events, including domain-specific
	// workers. Preserve a generic result when no richer result was supplied.
	if event.Kind == "state" && event.State.Terminal() && event.Result == nil &&
		op.LatestResult == nil {
		event.Result = &contract.ActionResult{
			OperationID: op.ID,
			Action:      op.Kind,
			Target:      op.Target,
			Attempt:     op.Attempt,
			Status:      event.State,
			Final:       true,
			StartedAt:   op.CreatedAt,
			CompletedAt: event.At,
			Code:        event.ErrorCode,
			Evidence:    "not_collected",
			TimingBasis: "operation_created",
			Stage:       "action",
			History:     contract.Cursor{OperationID: op.ID},
		}
		if event.Result.StartedAt.After(event.At) {
			event.Result.StartedAt = event.At
		}
		if event.State == contract.Failed {
			event.Result.Message = event.Message
			if event.Result.Message == "" {
				event.Result.Message = "Action failed; inspect retained history."
			}
		}
		if op.PendingResult != nil {
			event.Result.Data = op.PendingResult.Data
			event.Result.DataStatus = op.PendingResult.DataStatus
			if op.PendingResult.Message != "" {
				event.Result.Message = op.PendingResult.Message
			}
		}
		tryEnrichActionResultTx(ctx, tx, event.Result)
		tryActionOutputTx(ctx, tx, event.Result)
		tryActionPayloadTx(ctx, tx, op, event.Result, nil)
		op.LatestResult = event.Result
	}
	if event.Kind == "state" && event.State.Terminal() && event.Result != nil {
		op.LatestResult = event.Result
	}
	if event.Result != nil {
		result := event.Result
		if err := result.Validate(); err != nil {
			return err
		}
		if result.OperationID != op.ID || result.Action != op.Kind || result.Target != op.Target ||
			result.Attempt != op.Attempt ||
			result.History.After > op.LastSequence ||
			(result.ExcerptCursor != nil && result.ExcerptCursor.After > op.LastSequence) ||
			(result.DataCursor != nil && result.DataCursor.After > op.LastSequence) ||
			(event.Kind == "state" && (result.Status != event.State || !result.Final)) {
			return contract.NewFault(
				contract.ValidationFailed,
				"action result does not match its operation",
				"Record an outcome for the fenced operation and attempt.",
			)
		}
	}
	if err := event.Validate(); err != nil {
		return err
	}
	return tx.Operations().AppendEvent(ctx, op, event)
}

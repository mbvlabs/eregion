package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/jobargs"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/processmetrics"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// workerClientIDPrefix marks a River client as an eregion worker. Every client
// started on one host shares the "eregion-<host-hash>-" prefix, which lets a
// restarted worker recognise jobs its predecessor was running when it died.
const workerClientIDPrefix = "eregion-"

// workerHostHashLength is the number of SHA-256 hex characters that identify a
// host. A fixed-length hash cannot make host "web" a prefix of host
// "web-prod", and it avoids a lossy sanitisation that would conflate "web_a"
// with "web+a".
const workerHostHashLength = 16

// workerRescueTimeout bounds the pre-start recovery query. A worker that cannot
// read its own queue must not begin working as if no predecessor existed.
const workerRescueTimeout = 15 * time.Second

// workerHostIdentity returns the fixed-length, hash-derived host component of a
// worker client ID. Hostnames are case-insensitive, so trimming and lowercasing
// does not merge distinct hosts; every other byte is hashed as-is.
func workerHostIdentity(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		host = "unknown-host"
	}
	sum := sha256.Sum256([]byte(host))
	return hex.EncodeToString(sum[:])[:workerHostHashLength]
}

// workerHostPrefix is the deterministic prefix shared by every worker client
// started on one host. River records the client ID in a job's attempted_by, so
// a new worker can identify jobs a dead predecessor left running.
func workerHostPrefix(host string) string {
	return workerClientIDPrefix + workerHostIdentity(host) + "-"
}

// newWorkerClientID includes a verifiable process incarnation when available.
// Other platforms and inaccessible procfs retain unique but unrescuable IDs.
func newWorkerClientID(host string, startedAt time.Time) string {
	if identity, ok := currentWorkerIncarnation(); ok {
		random := newRequestID()
		if len(random) > 12 {
			random = random[:12]
		}
		id := workerHostPrefix(host) + identity + "-" + random
		if len(id) <= 100 {
			return id
		}
	}
	return workerHostPrefix(host) + startedAt.UTC().Format("20060102T150405") + "-" + newRequestID()
}

// Only a positively identified dead incarnation is safe to recover. Earlier
// entries are history; legacy and unverifiable owners are left to River.
func shouldRescueJob(attemptedBy []string, hostPrefix, clientID string) bool {
	if len(attemptedBy) == 0 {
		return false
	}
	last := attemptedBy[len(attemptedBy)-1]
	return last != "" && last != clientID && strings.HasPrefix(last, hostPrefix) &&
		workerIncarnationDead(strings.TrimPrefix(last, hostPrefix))
}

// orphanedPlatformJob is one running platform job whose last River attempt came
// from an earlier client on this host.
type orphanedPlatformJob struct {
	ID          int64
	OperationID contract.OperationID
	Attempt     int32
	MaxAttempts int32
	LastClient  string
}

// rescueHostPlatformJobs moves this host's running platform jobs back to
// available, or discards an exhausted job. The platform worker's own timeout is
// 30 minutes, longer than River's two-minute rescue threshold, so without this
// a SIGKILLed predecessor would hold accepted work until its timeout expired.
// Only jobs whose last attempt is proven dead on this host are moved.
func (s *Service) rescueHostPlatformJobs(
	ctx context.Context,
	hostPrefix, clientID string,
) (requeued, discarded int, err error) {
	running, err := s.entities.RiverJobs().ListRunningForQueue(ctx, jobargs.PlatformQueue)
	if err != nil {
		return 0, 0, unavailable(err)
	}
	var orphaned []orphanedPlatformJob
	for _, job := range running {
		attemptedBy := job.AttemptedBy
		if !shouldRescueJob(attemptedBy, hostPrefix, clientID) {
			continue
		}
		orphaned = append(orphaned, orphanedPlatformJob{
			ID: job.ID, OperationID: job.OperationID,
			Attempt: int32(job.Attempt), MaxAttempts: int32(job.MaxAttempts), //nolint:gosec // G115: River attempts are bounded well below int32 range.
			LastClient: attemptedBy[len(attemptedBy)-1],
		})
	}
	for _, job := range orphaned {
		moved, dropped, err := s.recoverOrphanedPlatformJob(ctx, job)
		if err != nil {
			return requeued, discarded, err
		}
		if moved {
			requeued++
		}
		if dropped {
			discarded++
		}
	}
	return requeued, discarded, nil
}

// recoverOrphanedPlatformJob re-queues or discards one observed running job.
// The conditional update rechecks the observed attempt and last client ID, so a
// job that changed owner after the scan is left alone. The job change and its
// operation event commit in one transaction.
func (s *Service) recoverOrphanedPlatformJob(
	ctx context.Context,
	job orphanedPlatformJob,
) (requeued, discarded bool, err error) {
	exhausted := job.Attempt >= job.MaxAttempts
	jobAttempt := uint32(job.Attempt) //nolint:gosec // G115: rescued River attempts are non-negative.
	rescue := entities.RiverRescue{
		ID: job.ID, Queue: jobargs.PlatformQueue, Attempt: jobAttempt, LastClient: job.LastClient,
	}
	var outcome platformJobRecovery
	err = s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		outcome, err = s.recoverOrphanedPlatformJobTx(ctx, tx, job, jobAttempt, rescue, exhausted)
		return err
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return false, false, unavailable(err)
		}
		return false, false, err
	}
	if !outcome.owned {
		return false, false, nil
	}
	s.observeCommittedActionResult(outcome.operation)
	return outcome.requeued, outcome.discarded, nil
}

type platformJobRecovery struct {
	owned     bool
	requeued  bool
	discarded bool
	operation contract.Operation
}

func (s *Service) recoverOrphanedPlatformJobTx(
	ctx context.Context,
	tx *entities.StoreTx,
	job orphanedPlatformJob,
	jobAttempt uint32,
	rescue entities.RiverRescue,
	exhausted bool,
) (platformJobRecovery, error) {
	affected, err := rescuePlatformJobTx(ctx, tx, rescue, exhausted)
	if err != nil || !affected {
		return platformJobRecovery{}, err
	}
	if job.OperationID == "" {
		return platformJobRecovery{}, fmt.Errorf(
			"platform job %d has no operation_id in its River arguments",
			job.ID,
		)
	}
	op, found, err := tx.Operations().Lock(ctx, job.OperationID)
	if err != nil {
		return platformJobRecovery{}, unavailable(err)
	}
	if !found {
		return platformJobRecovery{}, contract.NewFault(
			contract.Unavailable,
			"a running platform job has no operation record",
			"Inspect the operation and River job rows before starting the worker.",
		)
	}
	// The operation row is locked for this transaction, and the job change is
	// not visible until commit. No new River attempt can claim the operation
	// between the state change and this event.
	if exhausted {
		return s.finishExhaustedPlatformJobTx(ctx, tx, job, jobAttempt, op)
	}
	message := fmt.Sprintf(
		"Recovered orphaned platform job %d left running by an earlier worker process on this host; durable step records decide which effects still need to run.",
		job.ID,
	)
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "warning", Stage: "worker", Message: message}); err != nil {
		return platformJobRecovery{}, err
	}
	return platformJobRecovery{owned: true, requeued: true, operation: op}, nil
}

func rescuePlatformJobTx(ctx context.Context, tx *entities.StoreTx, rescue entities.RiverRescue, exhausted bool) (bool, error) {
	if !exhausted {
		affected, err := tx.RiverJobs().RequeueRunning(ctx, rescue)
		if err != nil {
			return false, unavailable(err)
		}
		return affected, nil
	}
	const reason = "Platform job discarded after its final attempt was left running by an earlier worker process on this host."
	affected, err := tx.RiverJobs().DiscardRunning(ctx, rescue, reason)
	if err != nil {
		return false, unavailable(err)
	}
	return affected, nil
}

func (s *Service) finishExhaustedPlatformJobTx(
	ctx context.Context,
	tx *entities.StoreTx,
	job orphanedPlatformJob,
	jobAttempt uint32,
	op contract.Operation,
) (platformJobRecovery, error) {
	handled, err := rescheduleExhaustedPlatformJobTx(ctx, tx, job, jobAttempt, &op)
	if err != nil {
		return platformJobRecovery{}, err
	}
	if handled {
		return platformJobRecovery{owned: true, requeued: true, operation: op}, nil
	}
	if err := s.discardExhaustedPlatformJobTx(ctx, tx, &op, job.ID, jobAttempt, job.Attempt, job.MaxAttempts); err != nil {
		return platformJobRecovery{}, err
	}
	return platformJobRecovery{owned: true, discarded: true, operation: op}, nil
}

func rescheduleExhaustedPlatformJobTx(ctx context.Context, tx *entities.StoreTx, job orphanedPlatformJob, jobAttempt uint32, op *contract.Operation) (bool, error) {
	if (op.Kind == platform.KindDomainAdd || op.Kind == platform.KindDomainRemove) &&
		op.Attempt <= jobAttempt {
		handled, err := prepareDefinitionDNSRecoveryTx(ctx, tx, op)
		if err != nil {
			return false, err
		}
		if handled {
			if _, err := tx.RiverJobs().Reschedule(ctx, job.ID, 30); err != nil {
				return false, unavailable(err)
			}
			return true, nil
		}
	}
	if op.Kind != platform.KindDefinitionApply || op.State.Terminal() || op.Attempt > jobAttempt {
		return false, nil
	}
	if op.State != contract.Cancelling {
		if err := definitionFailureTx(
			ctx,
			tx,
			op,
			contract.Unavailable,
			"Definition worker attempts were exhausted; linked work is being drained.",
		); err != nil {
			return false, err
		}
	}
	if _, err := definitionDrainTx(ctx, tx, op); err != nil {
		return false, err
	}
	if _, err := tx.RiverJobs().Reschedule(ctx, job.ID, 2); err != nil {
		return false, unavailable(err)
	}
	return true, nil
}

func (s *Service) discardExhaustedPlatformJobTx(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, jobID int64, jobAttempt uint32, attempt, maxAttempts int32) error {
	message := fmt.Sprintf(
		"Discarded platform job %d: its final River attempt was left running by an earlier worker process on this host.",
		jobID,
	)
	if err := appendEvent(
		ctx,
		tx,
		op,
		contract.Event{Kind: "warning", Stage: "worker", Message: message},
	); err != nil {
		return err
	}
	if op.Attempt > jobAttempt {
		return nil
	}
	if usesBuildTargetClaim(op.Kind) {
		return appendEvent(ctx, tx, op, contract.Event{
			Kind:    "warning",
			Stage:   "owner",
			Message: "River attempts are exhausted; reconciliation will verify the original build effect before settling.",
		})
	}
	if resourceTargetOperation(op.Kind) {
		return appendEvent(ctx, tx, op, contract.Event{
			Kind:    "warning",
			Stage:   "owner",
			Message: "River attempts are exhausted; the resource reservation remains until reconciliation accounts for its node effects.",
		})
	}
	if deploymentLifecycleKind(op.Kind) {
		// Restore the owner's exact traffic target before the terminal failure.
		// Reconcile sees the discarded job and completes the owner recovery;
		// marking the operation failed here would hide the route from that recovery.
		return appendEvent(ctx, tx, op, contract.Event{
			Kind:    "warning",
			Stage:   "owner",
			Message: "River attempts are exhausted; durable deployment ownership will restore the exact recorded serving route before the terminal failure is recorded.",
		})
	}
	failure := fmt.Sprintf(
		"Platform execution stopped after River attempt %d of %d was left running by an earlier worker process on this host; the job was discarded because no attempts remain.",
		attempt,
		maxAttempts,
	)
	return s.failPlatformOperationTx(ctx, tx, op, contract.Unavailable, failure)
}

// jobTimeout is the deadline one River job gets. It is the effective bound
// for a wait on a detached node task: the transport's own wait bound stays
// below it, so a worker never waits longer than its job.
const jobTimeout = time.Minute

// Job arguments deliberately contain an operation reference only, not input or secrets.
type Runtime struct {
	Service *Service
	Jobs    *entities.JobRuntime
	Metrics *processmetrics.Metrics
	// Nodes resolves the transport of any node for worker dispatch.
	Nodes NodeTransports
	// WebhookDeliveries is the durable admission store the API handler uses.
	// It enqueues the webhook worker's job in the same transaction as the
	// sanitized receipt.
	WebhookDeliveries *PostgresWebhookDeliveries
}

// EnsureLocalNode records the in-process local host as a platform node before
// the API serves inventory. Without this, a fresh installation lists no node
// until the first platform submission. It is idempotent and does nothing when
// the process has no local transport (a pure remote/SSH worker).
func (r *Runtime) EnsureLocalNode(ctx context.Context) error {
	if r == nil || r.Service == nil {
		return nil
	}
	return r.Service.ensureLocalNode(ctx)
}

// Close releases the per-node tunnels. Call it after the workers stopped.
func (r *Runtime) Close() {
	if r != nil && r.Service != nil && r.Service.resolver != nil {
		r.Service.resolver.Close()
	}
}

type WorkerConfig struct {
	Concurrency int
	Metrics     *processmetrics.Metrics
	// TestOnly removes River startup jitter. Never enable it in service commands.
	TestOnly bool
	// TestRouteSync replaces the Caddy route write in tests that need a real
	// worker transition without a Caddy installation. It is ignored unless
	// TestOnly is set; production reads the actual routes file.
	TestRouteSync func(context.Context) error
	// Transport defaults to a local node dispatcher. SSH and userspace WireGuard
	// adapters will implement the same interface.
	Transport node.Transport
	// NodeStateDir is the local dispatcher's shared state directory. Tests inject
	// a temporary directory; services use EREGION_NODE_STATE_DIR or the default.
	NodeStateDir string
	// RegistryEndpoint is the build node's actual configured loopback registry
	// endpoint, supplied by node composition. It is never defaulted here: a
	// build or private artifact selection without it fails rather than
	// publishing or reading somewhere unconfigured.
	RegistryEndpoint string
	// RegistryHostIdentity is the observed public identity of the physical main
	// host. New verifies it against the immutable workspace binding; a nil value
	// leaves direct registry mutation guards closed.
	RegistryHostIdentity *RegistryHostIdentity
	// SecretResolver resolves environment secret references at deployment time.
	// Nil means referenced secrets fail honestly instead of being injected.
	SecretResolver SecretResolver
	// Vault seals webhook secrets inside the operation admission transaction.
	// Nil fails that activation closed; ordinary submissions remain available.
	Vault *vault.Service
	// SourceRoots is the directory-build allowlist. Empty uses
	// EREGION_BUILD_SOURCE_ROOTS (or /srv/eregion/sources).
	SourceRoots []string
	// Nodes configures how enrolled nodes are reached (SSH identity, WireGuard
	// key directory). The zero value serves only the fallback Transport.
	Nodes ResolverConfig
	// LocalNodeName is the identity of the in-process local transport. Empty
	// uses EREGION_NODE_NAME. A worker that serves the local transport needs an
	// explicit name; there is no fallback identity.
	LocalNodeName string
	// Resolver is an already-built per-node transport resolver. The process
	// builds one resolver and shares it with extension dispatch, so there is
	// exactly one cache and one WireGuard key lock per node. Nil builds the
	// resolver from Nodes and Transport.
	Resolver *NodeResolver
	// Redaction is the process redaction registry. Nil builds one and still
	// registers the worker's own environment secrets.
	Redaction *secrets.Registry
	// HumanFactor verifies a transient TOTP code inside a factor-gated
	// submission transaction. Node RPC sets it; HTTP and worker-only processes
	// leave it nil, which fails factor-gated submissions closed.
	HumanFactor func(string) error
	// DomainDNS is the domain-scoped Cloudflare planner/executor. Nil means
	// managed DNS fails closed; manual domain bindings remain unaffected.
	DomainDNS *integrations.DomainDNS
}

func New(store *entities.Store, logger *slog.Logger, config WorkerConfig) (*Runtime, error) {
	if err := validateWorkerStartup(config, store); err != nil {
		return nil, err
	}
	service := newRuntimeService(store, logger, config)
	if err := service.configureRegistryHost(store, config.RegistryHostIdentity); err != nil {
		return nil, err
	}
	service.configureRedaction(config.Redaction)
	if err := service.configureNodeTransport(config); err != nil {
		return nil, err
	}
	service.configureResolver(store, config)
	riverConfig := newWorkerRiverConfig(logger, config, service)
	hostPrefix := configureWorkerQueues(riverConfig, config)
	jobRuntime, err := store.NewJobRuntime(riverConfig)
	if err != nil {
		return nil, err
	}
	if err := rescueWorkerJobs(service, config, riverConfig, hostPrefix, logger); err != nil {
		return nil, err
	}
	return &Runtime{
		Service:           service,
		Jobs:              jobRuntime,
		Metrics:           config.Metrics,
		Nodes:             service.resolver,
		WebhookDeliveries: NewWebhookDeliveryStore(store),
	}, nil
}

func validateWorkerStartup(config WorkerConfig, store *entities.Store) error {
	if config.Concurrency < 0 || config.Concurrency > 16 {
		return contract.NewFault(
			contract.ValidationFailed,
			"invalid worker concurrency",
			"Use 0 for insert-only, or 1–16 workers.",
		)
	}
	if store == nil {
		return contract.NewFault(
			contract.Internal,
			"the control plane needs typed persistence",
			"Construct the one entities store before constructing the control plane.",
		)
	}
	return nil
}

func newRuntimeService(store *entities.Store, logger *slog.Logger, config WorkerConfig) *Service {
	service := &Service{
		entities:         store,
		logger:           logger,
		secrets:          config.SecretResolver,
		vault:            config.Vault,
		sourceRoots:      config.SourceRoots,
		humanFactor:      config.HumanFactor,
		domainDNS:        config.DomainDNS,
		registryEndpoint: strings.TrimSpace(config.RegistryEndpoint),
	}
	if config.TestOnly && config.TestRouteSync != nil {
		service.routeSync = config.TestRouteSync
	}
	if config.Metrics != nil {
		service.observeActionAttempt = config.Metrics.ObserveActionAttempt
	}
	return service
}

func (s *Service) configureRegistryHost(store *entities.Store, observed *RegistryHostIdentity) error {
	if observed == nil {
		return nil
	}
	identity := *observed
	if _, err := VerifyWorkspaceRegistryHost(context.Background(), store.Workspace(), identity); err != nil {
		return err
	}
	s.registryHostIdentity = &identity
	return nil
}

func (s *Service) configureRedaction(provided *secrets.Registry) {
	s.redaction = provided
	if s.redaction == nil {
		s.redaction = &secrets.Registry{}
	}
	registerRuntimeSecrets(s.redaction)
}

func (s *Service) configureNodeTransport(config WorkerConfig) error {
	s.transport = config.Transport
	if s.transport == nil && (config.Concurrency > 0 || config.NodeStateDir != "") {
		return s.startLocalTransport(config)
	}
	if local, ok := s.transport.(*node.Local); ok {
		// Tests and embedders inject the local transport directly.
		s.localNode = local.NodeIdentity()
	}
	return nil
}

func (s *Service) startLocalTransport(config WorkerConfig) error {
	stateDir := config.NodeStateDir
	if stateDir == "" {
		stateDir = node.DefaultStateDir()
	}
	localName := strings.TrimSpace(config.LocalNodeName)
	if localName == "" {
		localName = strings.TrimSpace(os.Getenv("EREGION_NODE_NAME"))
	}
	if localName == "" {
		return contract.NewFault(
			contract.ValidationFailed,
			"the worker needs EREGION_NODE_NAME",
			"Set EREGION_NODE_NAME to this node's registered name before starting the worker.",
		)
	}
	local, err := node.NewLocal(stateDir, localName, &secrets.Registry{})
	if err != nil {
		return contract.NewFault(
			contract.ValidationFailed,
			"could not initialise the local node transport",
			"Check EREGION_NODE_STATE_DIR permissions and that every worker on this host uses the same directory.",
		)
	}
	s.transport = local
	s.localNode = localName
	return nil
}

func (s *Service) configureResolver(store *entities.Store, config WorkerConfig) {
	s.resolver = config.Resolver
	if s.resolver == nil {
		s.resolver = NewNodeResolver(
			store.Nodes(),
			config.Nodes,
			func() node.Transport { return s.transport },
		)
	}
}

func newWorkerRiverConfig(logger *slog.Logger, config WorkerConfig, service *Service) *river.Config {
	workers := river.NewWorkers()
	river.AddWorker(workers, &testWorker{service: service})
	river.AddWorker(workers, &platformWorker{service: service})
	river.AddWorker(workers, &webhookWorker{service: service})
	river.AddWorker(workers, &reconcileWorker{service: service})
	river.AddWorker(workers, &domainRouteWorker{service: service})
	river.AddWorker(workers, &domainObserveWorker{service: service})
	river.AddWorker(workers, &containerCleanupWorker{service: service})
	river.AddWorker(workers, &workloadRepairWorker{service: service})
	river.AddWorker(workers, &retentionWorker{service: service})
	riverConfig := &river.Config{
		Workers: workers, Logger: logger, MaxAttempts: operations.TestMaxAttempts,
		JobTimeout: jobTimeout, RescueStuckJobsAfter: 2 * time.Minute,
		SoftStopTimeout: 5 * time.Second, TestOnly: config.TestOnly, PollOnly: true,
	}
	if config.Metrics != nil {
		riverConfig.Middleware = []rivertype.Middleware{workerMetricsMiddleware(config.Metrics)}
	}
	return riverConfig
}

func workerMetricsMiddleware(metrics *processmetrics.Metrics) rivertype.Middleware {
	return river.WorkerMiddlewareFunc(
		func(ctx context.Context, job *rivertype.JobRow, inner func(context.Context) error) error {
			kind, action := workerJobMetricLabels(job)
			if job.Attempt > 1 {
				metrics.Retries.WithLabelValues(kind, action).Inc()
			}
			started := time.Now()
			outcome := "panic"
			defer func() {
				metrics.Attempts.WithLabelValues(kind, action, outcome).Inc()
				metrics.AttemptDuration.WithLabelValues(kind, action, outcome).
					Observe(time.Since(started).Seconds())
			}()
			err := inner(processmetrics.WithWorker(ctx, metrics))
			outcome = workerJobOutcome(err)
			return err
		},
	)
}

func workerJobMetricLabels(job *rivertype.JobRow) (string, string) {
	kind := job.Kind
	switch kind {
	case "diagnostic_test",
		"platform_action",
		"operation_reconcile",
		"platform_domain_routes",
		"platform_domain_observe",
		"platform_container_cleanup",
		"platform_workload_repair",
		"platform_retention",
		"webhook_admission":
	default:
		kind = "other"
	}
	switch kind {
	case "platform_action":
		var args struct {
			ActionKind string `json:"action_kind"`
		}
		if json.Unmarshal(job.EncodedArgs, &args) == nil &&
			(platform.IsKind(args.ActionKind) || args.ActionKind == platform.KindDefinitionApply) {
			return kind, args.ActionKind
		}
		return kind, "other"
	case "platform_retention":
		if len(job.EncodedArgs) > 0 {
			var args jobargs.Retention
			if json.Unmarshal(job.EncodedArgs, &args) == nil && args.OperationID != "" {
				return kind, platform.KindRetentionPrune
			}
		}
	}
	return kind, "none"
}

func workerJobOutcome(err error) string {
	var snooze *river.JobSnoozeError
	var cancelled *river.JobCancelError
	switch {
	case err == nil:
		return "success"
	case errors.As(err, &snooze):
		return "snoozed"
	case errors.As(err, &cancelled),
		errors.Is(err, context.Canceled),
		errors.Is(err, river.ErrJobCancelledRemotely):
		return "cancelled"
	default:
		return "error"
	}
}

func configureWorkerQueues(riverConfig *river.Config, config WorkerConfig) string {
	if config.Concurrency == 0 {
		return ""
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "unknown-host"
	}
	hostPrefix := workerHostPrefix(host)
	riverConfig.ID = newWorkerClientID(host, time.Now())
	riverConfig.Queues = map[string]river.QueueConfig{
		jobargs.DiagnosticQueue: {MaxWorkers: config.Concurrency},
		jobargs.PlatformQueue:   {MaxWorkers: config.Concurrency},
		jobargs.WebhookQueue:    {MaxWorkers: config.Concurrency},
		"maintenance":           {MaxWorkers: 1},
	}
	riverConfig.PeriodicJobs = workerPeriodicJobs()
	return hostPrefix
}

func workerPeriodicJobs() []*river.PeriodicJob {
	return []*river.PeriodicJob{
		river.NewPeriodicJob(
			river.PeriodicInterval(5*time.Second),
			func() (river.JobArgs, *river.InsertOpts) {
				return jobargs.OperationReconcile{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 3}
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		),
		periodicJob(30*time.Second, jobargs.DomainRoutes{}),
		periodicJob(5*time.Minute, jobargs.DomainObserve{}),
		periodicJob(5*time.Minute, jobargs.ContainerCleanup{}),
		periodicJob(5*time.Minute, jobargs.WorkloadRepair{}),
		periodicJob(retentionDailyCheckInterval, jobargs.Retention{}),
	}
}

func periodicJob(interval time.Duration, args river.JobArgs) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(interval),
		func() (river.JobArgs, *river.InsertOpts) {
			return args, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 1}
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	)
}

func rescueWorkerJobs(service *Service, config WorkerConfig, riverConfig *river.Config, hostPrefix string, logger *slog.Logger) error {
	if config.Concurrency == 0 {
		return nil
	}
	rescueCtx, cancel := context.WithTimeout(context.Background(), workerRescueTimeout)
	requeued, discarded, err := service.rescueHostPlatformJobs(rescueCtx, hostPrefix, riverConfig.ID)
	cancel()
	if err != nil {
		return contract.NewFault(
			contract.Unavailable,
			"could not recover this host's orphaned platform jobs",
			"Check the control database connection and restart the worker; another host's running jobs are left to River's rescuer.",
		)
	}
	if (requeued > 0 || discarded > 0) && logger != nil {
		logger.Info(
			"Recovered orphaned platform jobs before worker start",
			"requeued",
			requeued,
			"discarded",
			discarded,
		)
	}
	return nil
}

// registerRuntimeSecrets registers the worker's own secrets so a child process
// or a node observation that echoes them cannot persist them in operation
// events. The values are read from the protected environment, never from an
// operation input.
func registerRuntimeSecrets(registry *secrets.Registry) {
	dsn := os.Getenv("EREGION_DATABASE_URL")
	registry.Add(os.Getenv("PGPASSWORD"), dsn)
	if parsed, err := ParseDatabaseConfig(dsn); err == nil {
		registry.Add(parsed.ConnConfig.Password)
	}
	if path := os.Getenv("EREGION_VAULT_KEY_FILE"); path != "" {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the service's configured keyring path.
		if err != nil {
			return
		}
		registry.Add(string(raw))
		var file struct {
			Keys []struct {
				Key string `json:"key"`
			} `json:"keys"`
		}
		if json.Unmarshal(raw, &file) == nil {
			for _, entry := range file.Keys {
				registry.Add(entry.Key)
			}
		}
	}
}

type testWorker struct {
	river.WorkerDefaults[jobargs.DiagnosticTest]
	service *Service
}

func (*testWorker) NextRetry(job *river.Job[jobargs.DiagnosticTest]) time.Time {
	return time.Now().Add(time.Duration(job.Attempt) * time.Second)
}

var (
	errOperationTerminal = errors.New("operation is terminal")
	errNotOwned          = errors.New("operation is owned by a newer attempt")
)

func (w *testWorker) Work(ctx context.Context, job *river.Job[jobargs.DiagnosticTest]) error {
	attempt := uint32(job.Attempt) //nolint:gosec // G115: River attempts are non-negative.
	op, raw, claimed, err := w.service.claimAttempt(
		ctx,
		job.Args.OperationID,
		attempt,
		"diagnostic",
	)
	if err != nil {
		return err
	}
	if !claimed {
		return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
	}
	var input operations.TestInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	if op.State == contract.Cancelling {
		return w.settleAttempt(
			ctx,
			op.ID,
			attempt,
			w.reconcileCancellation(ctx, op.ID, attempt, input),
		)
	}
	if job.Attempt <= input.FailUntilAttempt {
		return w.settleAttempt(ctx, op.ID, attempt, w.injectedFailure(ctx, op.ID, attempt, job))
	}
	for index := 1; index <= input.Steps; index++ {
		state, currentAttempt, err := w.service.operationState(ctx, op.ID)
		if err != nil {
			return err
		}
		if currentAttempt != attempt {
			return w.service.waitForNewerAttempt(ctx, op.ID, attempt)
		}
		if state.Terminal() {
			return nil
		}
		if state == contract.Cancelling {
			return w.settleAttempt(
				ctx,
				op.ID,
				attempt,
				w.reconcileCancellation(ctx, op.ID, attempt, input),
			)
		}
		if err := w.runStep(ctx, op.ID, attempt, index, input); err != nil {
			return w.settleAttempt(ctx, op.ID, attempt, err)
		}
	}
	err = w.service.finishSucceeded(ctx, op.ID, attempt)
	return w.settleAttempt(ctx, op.ID, attempt, err)
}

// settleAttempt completes terminal work and makes a lost ownership race wait for
// the newer attempt. River's completion query is not attempt-fenced.
func (w *testWorker) settleAttempt(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	err error,
) error {
	if errors.Is(err, errOperationTerminal) {
		return nil
	}
	if errors.Is(err, errNotOwned) {
		return w.service.waitForNewerAttempt(ctx, id, attempt)
	}
	return err
}

func diagnosticStepID(index int) string { return node.DiagnosticStepID(index) }

func (w *testWorker) injectedFailure(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	job *river.Job[jobargs.DiagnosticTest],
) error {
	op, owned, err := w.service.mutateOwned(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, current *contract.Operation) error {
			if current.State.Terminal() {
				return nil
			}
			if current.State == contract.Cancelling {
				return transition(
					ctx,
					tx,
					current,
					contract.Canceled,
					"",
					"Diagnostic cancellation acknowledged.",
				)
			}
			if job.Attempt >= job.MaxAttempts {
				if err := markIncompleteSteps(
					ctx,
					tx,
					id,
					contract.Unavailable,
					"retry limit reached before the step completed",
				); err != nil {
					return err
				}
				return transition(
					ctx,
					tx,
					current,
					contract.Failed,
					contract.Unavailable,
					"Diagnostic retry limit reached.",
				)
			}
			if err := appendEvent(
				ctx,
				tx,
				current,
				contract.Event{
					Kind:    "warning",
					Message: "Diagnostic requested a retry; work remains durably queued.",
				},
			); err != nil {
				return err
			}
			return w.service.appendActionResultTx(
				ctx,
				tx,
				current,
				contract.Failed,
				contract.Unavailable,
				"Diagnostic requested a retry; work remains durably queued.",
				false,
			)
		},
	)
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	if op.State.Terminal() {
		return nil
	}
	if job.Attempt >= job.MaxAttempts {
		return river.JobCancel(errors.New("diagnostic retry limit reached"))
	}
	return errors.New("diagnostic retry requested")
}

// runStep asks the database, then the node, whether an effect already exists.
// Only a node status of absent/stale/failed leads to a dispatch.
func (w *testWorker) runStep(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	index int,
	input operations.TestInput,
) error {
	if w.service.transport == nil {
		return contract.NewFault(
			contract.Unavailable,
			"node transport is not configured",
			"Run this work in a worker configured with a node transport.",
		)
	}
	stepID := diagnosticStepID(index)
	exec := engine.Execution{
		OperationID: id,
		Attempt:     attempt,
		Step:        stepID,
		RequestID:   newRequestID(),
	}
	payload := diagnosticPayload(index, input)
	// The step is built through the same registry the node uses, so both sides
	// derive the same canonical inputs for the effect.
	step, err := node.DefaultRegistry().Build(exec, payload)
	if err != nil {
		return err
	}
	inputs, err := step.Inputs()
	if err != nil {
		return err
	}
	inputsHash := engine.InputsHash(step.ID(), inputs)

	hasRecord, completed, err := w.checkStepRecord(ctx, id, attempt, stepID, inputsHash)
	if err != nil {
		return err
	}
	if completed {
		return nil
	}
	status, err := w.queryNodeStatus(ctx, id, attempt, stepID, exec)
	if err != nil {
		return err
	}
	if status.InputsHash != "" && status.InputsHash != inputsHash {
		return w.failInputsMismatch(ctx, id, attempt, "node effect has different inputs")
	}
	if status.Status == engine.StatusCompleted {
		return w.completeNodeStep(ctx, id, attempt, stepID, inputsHash, status, hasRecord, input.Steps)
	}
	already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, exec.ExecutionID())
	if err != nil {
		return err
	}
	if already {
		return nil
	}
	response, err := w.callNodeRun(ctx, id, attempt, stepID, exec, payload)
	if err != nil {
		return err
	}
	if response.InputsHash != "" && response.InputsHash != inputsHash {
		return w.failInputsMismatch(ctx, id, attempt, "node effect has different inputs")
	}
	return w.handleNodeRunResponse(ctx, id, attempt, stepID, inputsHash, exec, input, response)
}

func (w *testWorker) checkStepRecord(ctx context.Context, id contract.OperationID, attempt uint32, stepID, inputsHash string) (bool, bool, error) {
	record, hasRecord, err := w.service.stepRecord(ctx, id, stepID)
	if err != nil {
		return false, false, err
	}
	if !hasRecord {
		return false, false, nil
	}
	if record.InputsHash != inputsHash {
		return false, false, w.failInputsMismatch(
			ctx,
			id,
			attempt,
			"step inputs changed for an existing effect",
		)
	}
	return true, record.State == "completed", nil
}

func (w *testWorker) queryNodeStatus(ctx context.Context, id contract.OperationID, attempt uint32, stepID string, exec engine.Execution) (node.Response, error) {
	status, err := w.service.transport.Call(
		ctx,
		node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec},
		nil,
	)
	if err != nil {
		if errors.Is(err, engine.ErrJournalCorrupt) {
			if failErr := w.service.failOperation(
				ctx,
				id,
				attempt,
				contract.PreconditionFailed,
				"Node journal is corrupt; inspect it before retrying.",
			); failErr != nil {
				return node.Response{}, failErr
			}
			return node.Response{}, errOperationTerminal
		}
		if node.IsPermanentTransport(err) {
			return node.Response{}, w.failTransport(ctx, id, attempt, err)
		}
		return node.Response{}, fmt.Errorf("node status %s: %w", stepID, err)
	}
	return status, nil
}

func (w *testWorker) completeNodeStep(ctx context.Context, id contract.OperationID, attempt uint32, stepID, inputsHash string, status node.Response, hasRecord bool, total int) error {
	if !hasRecord {
		if _, err := w.service.beginStep(
			ctx,
			id,
			attempt,
			stepID,
			inputsHash,
			status.ExecutionID,
		); err != nil {
			return err
		}
	}
	if err := w.service.recordRecoveredStep(
		ctx,
		id,
		attempt,
		stepID,
		status,
	); err != nil {
		return err
	}
	return w.service.ackStep(
		ctx,
		id,
		attempt,
		stepID,
		inputsHash,
		status.ExecutionID,
		status.Result,
		total,
	)
}

func (w *testWorker) callNodeRun(ctx context.Context, id contract.OperationID, attempt uint32, stepID string, exec engine.Execution, payload json.RawMessage) (node.Response, error) {
	var nodeEvents []node.Event
	onEvent := func(event node.Event) { nodeEvents = append(nodeEvents, event) }
	response, err := w.service.transport.Call(ctx, node.Request{
		Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec,
		Payload: payload,
	}, onEvent)
	if appendErr := w.service.appendNodeEvents(ctx, id, attempt, nodeEvents); appendErr != nil && err == nil {
		err = appendErr
	}
	if err != nil {
		if node.IsPermanentTransport(err) {
			return node.Response{}, w.failTransport(ctx, id, attempt, err)
		}
		// A lost stream is an unknown outcome, never proof that the node
		// stopped. The next attempt asks the node for its status first.
		return node.Response{}, fmt.Errorf("node run %s: %w", stepID, err)
	}
	if w.service.afterNode != nil {
		if err := w.service.afterNode(id, stepID); err != nil {
			return node.Response{}, err
		}
	}
	return response, nil
}

func (w *testWorker) handleNodeRunResponse(ctx context.Context, id contract.OperationID, attempt uint32, stepID, inputsHash string, exec engine.Execution, input operations.TestInput, response node.Response) error {
	switch response.Status {
	case engine.StatusCompleted:
		return w.service.ackStep(
			ctx,
			id,
			attempt,
			stepID,
			inputsHash,
			response.ExecutionID,
			response.Result,
			input.Steps,
		)
	case engine.StatusRunning:
		// A live node owner holds the effect beyond our lock timeout. Do not
		// start a second Apply; let River retry and reconcile.
		return contract.NewFault(
			contract.Unavailable,
			"node task is still running",
			"The operation remains running; reconnecting or retrying will attach to the existing effect, not start another.",
		)
	case engine.StatusFailed:
		return w.handleFailedNodeStep(ctx, id, attempt, stepID, exec, response)
	case engine.StatusAbsent, engine.StatusDeferred, engine.StatusStale:
		fallthrough
	default:
		return fmt.Errorf("node returned unexpected status %q for %s", response.Status, stepID)
	}
}

func (w *testWorker) handleFailedNodeStep(ctx context.Context, id contract.OperationID, attempt uint32, stepID string, exec engine.Execution, response node.Response) error {
	if err := w.service.retainNodeResponse(ctx, exec, response); err != nil {
		return err
	}
	code := response.ErrorCode
	if code == "" {
		code = string(contract.Internal)
	}
	if failErr := w.service.failStep(
		ctx,
		id,
		attempt,
		stepID,
		contract.Code(code),
		response.Detail,
	); failErr != nil {
		return failErr
	}
	if response.Retryable {
		if err := w.service.recordRetryResult(
			ctx,
			id,
			attempt,
			contract.Code(code),
			w.service.resultText(response.Detail),
		); err != nil {
			return err
		}
		return fmt.Errorf("node step %s failed: %s", stepID, response.Detail)
	}
	if response.InputsMismatch {
		return w.failInputsMismatch(ctx, id, attempt, "node step failed with different inputs")
	}
	if err := w.service.failOperation(
		ctx,
		id,
		attempt,
		contract.Code(code),
		"Node step failed without a retryable cause: "+response.Detail,
	); err != nil {
		return err
	}
	return errOperationTerminal
}

// failTransport ends an operation whose transport refused the request for a
// reason a retry cannot fix, such as a host-key mismatch.
func (w *testWorker) failTransport(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	cause error,
) error {
	message := "The node transport refused the request: " + cause.Error()
	if err := w.service.failOperation(
		ctx,
		id,
		attempt,
		contract.PreconditionFailed,
		message,
	); err != nil {
		return err
	}
	return errOperationTerminal
}

// failInputsMismatch ends the operation once. A recorded effect under other
// inputs can never converge, so returning a retryable error would only loop.
func (w *testWorker) failInputsMismatch(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	message string,
) error {
	fault := contract.NewFault(
		contract.PreconditionFailed,
		message,
		"Do not re-apply an effect recorded under other inputs; inspect the node journal.",
	)
	if err := w.service.failOperation(
		ctx,
		id,
		attempt,
		contract.PreconditionFailed,
		fault.Message,
	); err != nil {
		return err
	}
	return errOperationTerminal
}

func diagnosticPayload(index int, input operations.TestInput) json.RawMessage {
	raw, _ := json.Marshal(
		node.DiagnosticPayload{Index: index, Total: input.Steps, StepDelayMS: input.StepDelayMS},
	)
	return raw
}

func newRequestID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "request"
	}
	return hex.EncodeToString(value)
}

// reconcileCancellation acknowledges completed effects that were not yet
// acknowledged, then decides between success (all effects complete) and
// cancellation. It never starts a new node step.
func (w *testWorker) reconcileCancellation(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	input operations.TestInput,
) error {
	live, err := w.service.reconcileCompletedSteps(ctx, id, attempt, input)
	if err != nil {
		return err
	}
	if live {
		return contract.NewFault(
			contract.Unavailable,
			"node task is still running",
			"Cancellation waits for the in-flight step boundary; retry to reconcile the completed effect.",
		)
	}
	if err := w.service.finishCancelled(ctx, id, attempt, input.Steps); err != nil {
		return err
	}
	return nil
}

type reconcileWorker struct {
	river.WorkerDefaults[jobargs.OperationReconcile]
	service *Service
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[jobargs.OperationReconcile]) error {
	return w.service.Reconcile(ctx)
}

type domainRouteWorker struct {
	river.WorkerDefaults[jobargs.DomainRoutes]
	service *Service
}

func (w *domainRouteWorker) Work(ctx context.Context, _ *river.Job[jobargs.DomainRoutes]) error {
	// Periodic reconciliation retries on its own; returning the error would
	// only make River attach a retry to the same schedule.
	logDomainMaintenance(ctx, "route reconciliation", w.service.reconcileDomainRoutes(ctx))
	return nil
}

type domainObserveWorker struct {
	river.WorkerDefaults[jobargs.DomainObserve]
	service *Service
}

func (w *domainObserveWorker) Work(ctx context.Context, _ *river.Job[jobargs.DomainObserve]) error {
	logDomainMaintenance(ctx, "observation", w.service.observeDomains(ctx))
	return nil
}

type containerCleanupWorker struct {
	river.WorkerDefaults[jobargs.ContainerCleanup]
	service *Service
}

func (w *containerCleanupWorker) Work(
	ctx context.Context,
	_ *river.Job[jobargs.ContainerCleanup],
) error {
	logMaintenance(ctx, "container cleanup", w.service.retryDueContainerCleanups(ctx))
	return nil
}

type workloadRepairWorker struct {
	river.WorkerDefaults[jobargs.WorkloadRepair]
	service *Service
}

func (w *workloadRepairWorker) Work(ctx context.Context, _ *river.Job[jobargs.WorkloadRepair]) error {
	logMaintenance(ctx, "workload repair", w.service.repairServingWorkloads(ctx))
	return nil
}

// Reconcile covers exhausted attempts lost to a process crash, not just errors
// returned by Work. Domain terminal state cannot depend on an in-process callback.
func (s *Service) Reconcile(ctx context.Context) error {
	var failures []error
	if err := s.reconcileSettledTargets(ctx); err != nil {
		failures = append(failures, fmt.Errorf("reconcile settled targets: %w", err))
	}
	if err := s.reconcileTerminalBuildCapacity(ctx); err != nil {
		failures = append(failures, fmt.Errorf("reconcile terminal build capacity: %w", err))
	}
	if err := s.reconcileTerminalDefinitionRuns(ctx); err != nil {
		failures = append(failures, fmt.Errorf("reconcile terminal definition runs: %w", err))
	}
	if err := s.reconcileWebhookAdmissions(ctx); err != nil {
		failures = append(failures, fmt.Errorf("reconcile webhook admissions: %w", err))
	}
	if err := s.reconcileTerminalResourceStates(ctx); err != nil {
		failures = append(failures, fmt.Errorf("reconcile terminal resource states: %w", err))
	}
	if err := s.reconcileDeploymentOwners(ctx); err != nil {
		failures = append(failures, fmt.Errorf("reconcile deployment owners: %w", err))
	}
	rows, err := s.entities.RiverJobs().ListReconcileCandidateOperations(ctx, 100)
	if err != nil {
		return unavailable(err)
	}
	ids := rows
	for _, id := range ids {
		if err := s.reconcileOperation(ctx, id); err != nil {
			if errors.Is(err, errNotOwned) || errors.Is(err, errOperationTerminal) {
				continue
			}
			failures = append(failures, fmt.Errorf("reconcile %s: %w", id, err))
		}
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func (s *Service) reconcileOperation(ctx context.Context, id contract.OperationID) error {
	op, found, err := s.entities.Operations().Get(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return contract.NewFault(contract.NotFound, "operation not found", "The operation no longer exists; nothing to reconcile.")
	}
	if op.State.Terminal() {
		return nil
	}
	if op.Kind == platform.KindDefinitionApply {
		return s.requeueDefinitionDrain(ctx, id)
	}
	if s.transport == nil {
		return contract.NewFault(
			contract.Unavailable,
			"node transport is not configured",
			"Reconciliation requires a worker node transport.",
		)
	}
	if strings.HasPrefix(op.Kind, "platform.") {
		return s.reconcilePlatformJob(ctx, op)
	}
	var input operations.TestInput
	rawInput, found, err := s.entities.Operations().Input(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return contract.NewFault(contract.NotFound, "operation input not found", "The operation no longer exists; nothing to reconcile.")
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return err
	}
	live, stop, err := s.reconcileOperationSteps(ctx, id, op, input)
	if err != nil {
		return err
	}
	if stop {
		return nil
	}
	return s.finishReconciledOperation(ctx, id, op, input, live)
}

func (s *Service) reconcilePlatformJob(ctx context.Context, op contract.Operation) error {
	switch {
	case usesBuildTargetClaim(op.Kind):
		return s.reconcileBuildTarget(ctx, op)
	case resourceTargetOperation(op.Kind):
		return s.reconcileResourceTarget(ctx, op)
	}
	return s.reconcilePlatformOperation(ctx, op)
}

func (s *Service) reconcileOperationSteps(ctx context.Context, id contract.OperationID, op contract.Operation, input operations.TestInput) (live, stop bool, err error) {
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return false, false, err
	}
	for _, record := range records {
		if record.State == "completed" {
			continue
		}
		stepLive, stop, err := s.reconcileStepRecord(ctx, id, op, record, input)
		if errors.Is(err, errNotOwned) {
			return false, true, nil
		}
		if err != nil || stop {
			return false, stop, err
		}
		live = live || stepLive
	}
	return live, false, nil
}

func (s *Service) reconcileStepRecord(ctx context.Context, id contract.OperationID, op contract.Operation, record StepRecord, input operations.TestInput) (live, stop bool, err error) {
	exec := engine.Execution{
		OperationID: id,
		Attempt:     op.Attempt,
		Step:        record.Step,
		RequestID:   newRequestID(),
	}
	status, err := s.transport.Call(
		ctx,
		node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec},
		nil,
	)
	if err != nil {
		return false, false, s.reconcileStepStatusError(ctx, id, op.Attempt, err)
	}
	if status.InputsHash != "" && status.InputsHash != record.InputsHash {
		err := s.failReconciliationInputs(ctx, id, op.Attempt)
		if errors.Is(err, errNotOwned) {
			return false, true, nil
		}
		if err != nil {
			return false, false, err
		}
		return false, true, nil
	}
	if status.Status != engine.StatusCompleted {
		return status.Status == engine.StatusRunning, false, nil
	}
	err = s.ackStep(
		ctx,
		id,
		op.Attempt,
		record.Step,
		record.InputsHash,
		status.ExecutionID,
		status.Result,
		input.Steps,
	)
	if errors.Is(err, errNotOwned) {
		return false, true, nil
	}
	return false, false, err
}

func (s *Service) reconcileStepStatusError(ctx context.Context, id contract.OperationID, attempt uint32, statusErr error) error {
	switch {
	case errors.Is(statusErr, engine.ErrJournalCorrupt):
		err := s.failOperation(ctx, id, attempt, contract.PreconditionFailed, "Node journal is corrupt; inspect it before retrying.")
		if errors.Is(err, errNotOwned) {
			return errNotOwned
		}
		return err
	case node.IsPermanentTransport(statusErr):
		err := s.failOperation(ctx, id, attempt, contract.PreconditionFailed, "The node transport refused the request: "+statusErr.Error())
		if errors.Is(err, errNotOwned) {
			return errNotOwned
		}
		return err
	default:
		return unavailable(statusErr)
	}
}

func (s *Service) failReconciliationInputs(ctx context.Context, id contract.OperationID, attempt uint32) error {
	// The write is attempt-fenced like every other worker write: a newer
	// attempt owns the operation and must not be overwritten.
	_, owned, err := s.mutateOwned(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, current *contract.Operation) error {
			if current.State.Terminal() {
				return nil
			}
			return transition(
				ctx,
				tx,
				current,
				contract.Failed,
				contract.PreconditionFailed,
				"Node effect inputs changed during reconciliation; inspect the node journal.",
			)
		},
	)
	if !owned || errors.Is(err, errNotOwned) {
		return errNotOwned
	}
	return err
}

func (s *Service) finishReconciledOperation(ctx context.Context, id contract.OperationID, op contract.Operation, input operations.TestInput, live bool) error {
	completed, err := s.entities.Steps().CountCompleted(ctx, id)
	if err != nil {
		return unavailable(err)
	}
	if completed == input.Steps {
		err := s.finishSucceeded(ctx, id, op.Attempt)
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	if live {
		return s.markReconciliationLive(ctx, id, op.Attempt)
	}
	if op.State == contract.Cancelling {
		err := s.finishCancelled(ctx, id, op.Attempt, input.Steps)
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	return s.failReconciliationIncomplete(ctx, id, op.Attempt)
}

func (s *Service) markReconciliationLive(ctx context.Context, id contract.OperationID, attempt uint32) error {
	_, owned, err := s.mutateOwned(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, current *contract.Operation) error {
			if current.State.Terminal() {
				return nil
			}
			if current.State == contract.Queued {
				if err := transition(
					ctx,
					tx,
					current,
					contract.Running,
					"",
					"Reconciled a live node task after its job ended.",
				); err != nil {
					return err
				}
			}
			return appendEvent(
				ctx,
				tx,
				current,
				contract.Event{
					Kind:    "warning",
					Message: "Execution ended without a domain result, but the node task is still running; cancellation and failure wait for the in-flight step boundary.",
				},
			)
		},
	)
	if !owned || errors.Is(err, errNotOwned) {
		return nil
	}
	return err
}

func (s *Service) failReconciliationIncomplete(ctx context.Context, id contract.OperationID, attempt uint32) error {
	_, owned, err := s.mutateOwned(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, current *contract.Operation) error {
			if current.State.Terminal() {
				return nil
			}
			if err := markIncompleteSteps(
				ctx,
				tx,
				id,
				contract.Unavailable,
				"execution ended before the step was acknowledged",
			); err != nil {
				return err
			}
			return transition(
				ctx,
				tx,
				current,
				contract.Failed,
				contract.Unavailable,
				"Execution ended without an acknowledged domain result; inspect operation history before retrying.",
			)
		},
	)
	if !owned || errors.Is(err, errNotOwned) {
		return nil
	}
	return err
}

// waitForNewerAttempt does not write domain state while a newer River attempt
// owns the operation. River's completion query is not attempt-fenced, so
// returning early could complete the shared job row underneath newer work.
func (s *Service) waitForNewerAttempt(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, current, err := s.operationState(ctx, id)
		if err != nil {
			return err
		}
		if state.Terminal() || current <= attempt {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

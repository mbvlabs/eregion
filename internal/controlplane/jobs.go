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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/processmetrics"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
)

const diagnosticQueue = "diagnostic"
const platformQueue = "platform"

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

// newWorkerClientID returns this start's unique River client ID. The timestamp
// keeps IDs readable; the random suffix keeps restarts and same-host processes
// distinct. The fixed prefix and suffix stay below River's 100-character limit.
func newWorkerClientID(host string, startedAt time.Time) string {
	return workerHostPrefix(host) + startedAt.UTC().Format("20060102T150405") + "-" + newRequestID()
}

// shouldRescueJob reports whether a running job's last River attempt was made
// by an earlier client on this host. Earlier entries are history, not the
// current owner; a job last attempted by another host or by the new client is
// left alone.
func shouldRescueJob(attemptedBy []string, hostPrefix, clientID string) bool {
	if len(attemptedBy) == 0 {
		return false
	}
	last := attemptedBy[len(attemptedBy)-1]
	return last != "" && last != clientID && strings.HasPrefix(last, hostPrefix)
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
// Only jobs whose last attempt came from this host are moved; another host may
// still be alive.
func (s *Service) rescueHostPlatformJobs(ctx context.Context, hostPrefix, clientID string) (requeued, discarded int, err error) {
	rows, err := s.pool.Query(ctx, `
        SELECT j.id, j.attempted_by, coalesce(j.args->>'operation_id',''), j.attempt, j.max_attempts
        FROM river_job j
        WHERE j.queue=$1 AND j.state='running'`, platformQueue)
	if err != nil {
		return 0, 0, unavailable()
	}
	var orphaned []orphanedPlatformJob
	for rows.Next() {
		var job orphanedPlatformJob
		var attemptedBy []string
		var operationID string
		if err := rows.Scan(&job.ID, &attemptedBy, &operationID, &job.Attempt, &job.MaxAttempts); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if !shouldRescueJob(attemptedBy, hostPrefix, clientID) {
			continue
		}
		job.OperationID = contract.OperationID(operationID)
		job.LastClient = attemptedBy[len(attemptedBy)-1]
		orphaned = append(orphaned, job)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, err
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
func (s *Service) recoverOrphanedPlatformJob(ctx context.Context, job orphanedPlatformJob) (requeued, discarded bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, unavailable()
	}
	defer rollback(tx)

	exhausted := job.Attempt >= job.MaxAttempts
	var affected int64
	if exhausted {
		const reason = "Platform job discarded after its final attempt was left running by an earlier worker process on this host."
		tag, err := tx.Exec(ctx, `UPDATE river_job
            SET state='discarded', finalized_at=clock_timestamp(),
                errors=array_append(coalesce(errors,ARRAY[]::jsonb[]),jsonb_build_object(
                    'at',clock_timestamp(),'attempt',attempt,'error',$5::text,'trace',''))
            WHERE id=$1 AND queue=$2 AND state='running' AND attempt=$3
              AND cardinality(attempted_by)>0 AND attempted_by[cardinality(attempted_by)]=$4`,
			job.ID, platformQueue, job.Attempt, job.LastClient, reason)
		if err != nil {
			return false, false, unavailable()
		}
		affected = tag.RowsAffected()
	} else {
		tag, err := tx.Exec(ctx, `UPDATE river_job
            SET state='available', scheduled_at=clock_timestamp(), finalized_at=NULL
            WHERE id=$1 AND queue=$2 AND state='running' AND attempt=$3
              AND cardinality(attempted_by)>0 AND attempted_by[cardinality(attempted_by)]=$4`,
			job.ID, platformQueue, job.Attempt, job.LastClient)
		if err != nil {
			return false, false, unavailable()
		}
		affected = tag.RowsAffected()
	}
	if affected == 0 {
		return false, false, nil
	}
	if job.OperationID == "" {
		return false, false, fmt.Errorf("platform job %d has no operation_id in its River arguments", job.ID)
	}
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", job.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, contract.NewFault(contract.Unavailable, "a running platform job has no operation record", "Inspect the operation and River job rows before starting the worker.")
	}
	if err != nil {
		return false, false, unavailable()
	}
	// The operation row is locked for this transaction, and the job change is
	// not visible until commit. No new River attempt can claim the operation
	// between the state change and this event.
	if exhausted {
		if (op.Kind == platform.KindDomainAdd || op.Kind == platform.KindDomainRemove) && op.Attempt <= uint32(job.Attempt) {
			handled, err := prepareDefinitionDNSRecoveryTx(ctx, tx, &op)
			if err != nil {
				return false, false, err
			}
			if handled {
				if _, err := tx.Exec(ctx, `UPDATE river_job SET state='scheduled',finalized_at=NULL,max_attempts=GREATEST(max_attempts,attempt+1),scheduled_at=clock_timestamp()+interval '30 seconds' WHERE id=$1`, job.ID); err != nil {
					return false, false, unavailable(err)
				}
				if err := tx.Commit(ctx); err != nil {
					return false, false, unavailable(err)
				}
				return true, false, nil
			}
		}
		if op.Kind == platform.KindDefinitionApply && !op.State.Terminal() && op.Attempt <= uint32(job.Attempt) {
			if op.State != contract.Cancelling {
				if err := definitionFailureTx(ctx, tx, &op, contract.Unavailable, "Definition worker attempts were exhausted; linked work is being drained."); err != nil {
					return false, false, err
				}
			}
			if _, err := definitionDrainTx(ctx, tx, &op); err != nil {
				return false, false, err
			}
			if _, err := tx.Exec(ctx, `UPDATE river_job SET state='scheduled',finalized_at=NULL,max_attempts=GREATEST(max_attempts,attempt+1),scheduled_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, job.ID); err != nil {
				return false, false, unavailable(err)
			}
			if err := tx.Commit(ctx); err != nil {
				return false, false, unavailable(err)
			}
			return true, false, nil
		}
		message := fmt.Sprintf("Discarded platform job %d: its final River attempt was left running by an earlier worker process on this host.", job.ID)
		if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "warning", Stage: "worker", Message: message}); err != nil {
			return false, false, err
		}
		if op.Attempt <= uint32(job.Attempt) {
			if deploymentLifecycleKind(op.Kind) {
				// Restore the owner's exact traffic target before the terminal
				// failure. Reconcile sees the discarded job and completes the
				// owner recovery; marking the operation failed here would hide
				// the route from that recovery.
				if err := appendEvent(ctx, tx, &op, contract.Event{
					Kind: "warning", Stage: "owner",
					Message: "River attempts are exhausted; durable deployment ownership will restore the exact recorded serving route before the terminal failure is recorded.",
				}); err != nil {
					return false, false, err
				}
			} else {
				failure := fmt.Sprintf("Platform execution stopped after River attempt %d of %d was left running by an earlier worker process on this host; the job was discarded because no attempts remain.", job.Attempt, job.MaxAttempts)
				if err := s.failPlatformOperationTx(ctx, tx, &op, contract.Unavailable, failure); err != nil {
					return false, false, err
				}
			}
		}
	} else {
		message := fmt.Sprintf("Recovered orphaned platform job %d left running by an earlier worker process on this host; durable step records decide which effects still need to run.", job.ID)
		if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "warning", Stage: "worker", Message: message}); err != nil {
			return false, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, false, unavailable()
	}
	s.observeCommittedActionResult(op)
	return !exhausted, exhausted, nil
}

// jobTimeout is the deadline one River job gets. It is the effective bound
// for a wait on a detached node task: the transport's own wait bound stays
// below it, so a worker never waits longer than its job.
const jobTimeout = time.Minute

// Job arguments deliberately contain an operation reference only, not input or secrets.
type testArgs struct {
	OperationID contract.OperationID `json:"operation_id"`
}

func (testArgs) Kind() string { return "diagnostic_test" }

// platformArgs deliberately contains only durable identity. Input stays in
// PostgreSQL and may be read by the worker; no secret material enters River.
type platformArgs struct {
	OperationID contract.OperationID `json:"operation_id"`
	ActionKind  string               `json:"action_kind"`
}

func (platformArgs) Kind() string { return "platform_action" }

type reconcileArgs struct{}

func (reconcileArgs) Kind() string { return "operation_reconcile" }

type domainRouteArgs struct{}

func (domainRouteArgs) Kind() string { return "platform_domain_routes" }

type domainObserveArgs struct{}

func (domainObserveArgs) Kind() string { return "platform_domain_observe" }

type containerCleanupArgs struct{}

func (containerCleanupArgs) Kind() string { return "platform_container_cleanup" }

type workloadRepairArgs struct{}

func (workloadRepairArgs) Kind() string { return "platform_workload_repair" }

// retentionArgs is one durable disk-retention attempt. The periodic daily job
// leaves OperationID empty and submits the day's operation; the operation's own
// job carries its ID on the maintenance queue.
type retentionArgs struct {
	OperationID contract.OperationID `json:"operation_id,omitempty"`
}

func (retentionArgs) Kind() string { return "platform_retention" }

type Runtime struct {
	Service *Service
	Jobs    *river.Client[pgx.Tx]
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

func New(pool *pgxpool.Pool, logger *slog.Logger, config WorkerConfig) (*Runtime, error) {
	if config.Concurrency < 0 || config.Concurrency > 16 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid worker concurrency", "Use 0 for insert-only, or 1–16 workers.")
	}
	service := &Service{pool: pool, secrets: config.SecretResolver, vault: config.Vault, sourceRoots: config.SourceRoots, humanFactor: config.HumanFactor, domainDNS: config.DomainDNS, registryEndpoint: strings.TrimSpace(config.RegistryEndpoint)}
	if config.Metrics != nil {
		service.observeActionAttempt = config.Metrics.ObserveActionAttempt
	}
	if config.RegistryHostIdentity != nil {
		identity := *config.RegistryHostIdentity
		if _, err := VerifyWorkspaceRegistryHost(context.Background(), pool, identity); err != nil {
			return nil, err
		}
		service.registryHostIdentity = &identity
	}
	service.redaction = config.Redaction
	if service.redaction == nil {
		service.redaction = &secrets.Registry{}
	}
	registerRuntimeSecrets(service.redaction)
	service.transport = config.Transport
	if service.transport == nil && (config.Concurrency > 0 || config.NodeStateDir != "") {
		stateDir := config.NodeStateDir
		if stateDir == "" {
			stateDir = node.DefaultStateDir()
		}
		localName := strings.TrimSpace(config.LocalNodeName)
		if localName == "" {
			localName = strings.TrimSpace(os.Getenv("EREGION_NODE_NAME"))
		}
		if localName == "" {
			return nil, contract.NewFault(contract.ValidationFailed, "the worker needs EREGION_NODE_NAME", "Set EREGION_NODE_NAME to this node's registered name before starting the worker.")
		}
		local, err := node.NewLocal(stateDir, localName, &secrets.Registry{})
		if err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "could not initialise the local node transport", "Check EREGION_NODE_STATE_DIR permissions and that every worker on this host uses the same directory.")
		}
		service.transport = local
		service.localNode = localName
	} else if local, ok := service.transport.(*node.Local); ok {
		// Tests and embedders inject the local transport directly.
		service.localNode = local.NodeIdentity()
	}
	service.resolver = config.Resolver
	if service.resolver == nil {
		service.resolver = NewNodeResolver(pool, config.Nodes, func() node.Transport { return service.transport })
	}
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
		SoftStopTimeout: 5 * time.Second, TestOnly: config.TestOnly,
	}
	if config.Metrics != nil {
		riverConfig.Middleware = []rivertype.Middleware{river.WorkerMiddlewareFunc(func(ctx context.Context, job *rivertype.JobRow, inner func(context.Context) error) error {
			kind := job.Kind
			switch kind {
			case "diagnostic_test", "platform_action", "operation_reconcile", "platform_domain_routes", "platform_domain_observe", "platform_container_cleanup", "platform_workload_repair", "platform_retention", "webhook_admission":
			default:
				kind = "other"
			}
			action := "none"
			if kind == "platform_action" {
				var args struct {
					ActionKind string `json:"action_kind"`
				}
				if json.Unmarshal(job.EncodedArgs, &args) == nil && (platform.IsKind(args.ActionKind) || args.ActionKind == platform.KindDefinitionApply) {
					action = args.ActionKind
				} else {
					action = "other"
				}
			} else if kind == "platform_retention" && len(job.EncodedArgs) > 0 {
				var args retentionArgs
				if json.Unmarshal(job.EncodedArgs, &args) == nil && args.OperationID != "" {
					action = platform.KindRetentionPrune
				}
			}
			if job.Attempt > 1 {
				config.Metrics.Retries.WithLabelValues(kind, action).Inc()
			}
			started := time.Now()
			outcome := "panic"
			defer func() {
				config.Metrics.Attempts.WithLabelValues(kind, action, outcome).Inc()
				config.Metrics.AttemptDuration.WithLabelValues(kind, action, outcome).Observe(time.Since(started).Seconds())
			}()
			err := inner(processmetrics.WithWorker(ctx, config.Metrics))
			var snooze *river.JobSnoozeError
			var cancelled *river.JobCancelError
			switch {
			case err == nil:
				outcome = "success"
			case errors.As(err, &snooze):
				outcome = "snoozed"
			case errors.As(err, &cancelled), errors.Is(err, context.Canceled), errors.Is(err, river.ErrJobCancelledRemotely):
				outcome = "cancelled"
			default:
				outcome = "error"
			}
			return err
		})}
	}
	var hostPrefix string
	if config.Concurrency > 0 {
		host, err := os.Hostname()
		if err != nil || strings.TrimSpace(host) == "" {
			host = "unknown-host"
		}
		hostPrefix = workerHostPrefix(host)
		riverConfig.ID = newWorkerClientID(host, time.Now())
		riverConfig.Queues = map[string]river.QueueConfig{
			diagnosticQueue: {MaxWorkers: config.Concurrency},
			platformQueue:   {MaxWorkers: config.Concurrency},
			webhookQueue:    {MaxWorkers: config.Concurrency},
			"maintenance":   {MaxWorkers: 1},
		}
		riverConfig.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(5*time.Second), func() (river.JobArgs, *river.InsertOpts) {
				return reconcileArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 3}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			// Reconcile the routes file on worker start and after any failed
			// reload. The advisory lock keeps this independent of a domain
			// change that is already writing the same file.
			river.NewPeriodicJob(river.PeriodicInterval(30*time.Second), func() (river.JobArgs, *river.InsertOpts) {
				return domainRouteArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 1}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			// Observe DNS, certificate and public response separately. A
			// transient probe failure is not a failed binding; the next run
			// overwrites the observation.
			river.NewPeriodicJob(river.PeriodicInterval(5*time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return domainObserveArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 1}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			// Retry containers a verified switch or a failed candidate could
			// not remove. The row owns the backoff; the job only retries due
			// work, so it never races the deploy that recorded it.
			river.NewPeriodicJob(river.PeriodicInterval(5*time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return containerCleanupArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 1}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			// Restore a serving container that disappeared (reboot, OOM, a
			// manual rm). The queued restart is idempotent per serving
			// deployment and missing-name set.
			river.NewPeriodicJob(river.PeriodicInterval(5*time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return workloadRepairArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 1}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			// Check once an hour whether the UTC day still needs a retention
			// run. The submitter allows at most one successful run per day,
			// never submits while one is queued/running, and retries a failed
			// or cancelled day after a one-hour backoff with a new numbered
			// idempotency key. RunOnStart catches up after a restart.
			river.NewPeriodicJob(river.PeriodicInterval(retentionDailyCheckInterval), func() (river.JobArgs, *river.InsertOpts) {
				return retentionArgs{}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: 1}
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		}
	}
	client, err := river.NewClient(riverpgxv5.New(pool), riverConfig)
	if err != nil {
		return nil, err
	}
	if config.Concurrency > 0 {
		rescueCtx, cancel := context.WithTimeout(context.Background(), workerRescueTimeout)
		requeued, discarded, rescueErr := service.rescueHostPlatformJobs(rescueCtx, hostPrefix, client.ID())
		cancel()
		if rescueErr != nil {
			return nil, contract.NewFault(contract.Unavailable, "could not recover this host's orphaned platform jobs", "Check the control database connection and restart the worker; another host's running jobs are left to River's rescuer.")
		}
		if (requeued > 0 || discarded > 0) && logger != nil {
			logger.Info("Recovered orphaned platform jobs before worker start", "requeued", requeued, "discarded", discarded)
		}
	}
	service.enqueue = func(ctx context.Context, tx pgx.Tx, id contract.OperationID) (int64, error) {
		inserted, err := client.InsertTx(ctx, tx, testArgs{OperationID: id}, &river.InsertOpts{Queue: diagnosticQueue, MaxAttempts: operations.TestMaxAttempts})
		if err != nil {
			return 0, err
		}
		return inserted.Job.ID, nil
	}
	service.enqueuePlatform = func(ctx context.Context, tx pgx.Tx, op contract.Operation) (int64, error) {
		if op.Kind == platform.KindRetentionPrune {
			inserted, err := client.InsertTx(ctx, tx, retentionArgs{OperationID: op.ID}, &river.InsertOpts{Queue: "maintenance", MaxAttempts: platformMaxAttempts})
			if err != nil {
				return 0, err
			}
			return inserted.Job.ID, nil
		}
		inserted, err := client.InsertTx(ctx, tx, platformArgs{OperationID: op.ID, ActionKind: op.Kind}, &river.InsertOpts{Queue: platformQueue, MaxAttempts: platformMaxAttempts})
		if err != nil {
			return 0, err
		}
		return inserted.Job.ID, nil
	}
	service.enqueueWebhook = func(ctx context.Context, tx pgx.Tx, environmentID, deliveryIDHash string) (int64, error) {
		inserted, err := client.InsertTx(ctx, tx, webhookArgs{EnvironmentID: environmentID, DeliveryIDHash: deliveryIDHash}, &river.InsertOpts{Queue: webhookQueue, MaxAttempts: platformMaxAttempts})
		if err != nil {
			return 0, err
		}
		return inserted.Job.ID, nil
	}
	webhookDeliveries := NewWebhookDeliveryStore(pool)
	webhookDeliveries.enqueue = service.enqueueWebhook
	return &Runtime{Service: service, Jobs: client, Metrics: config.Metrics, Nodes: service.resolver, WebhookDeliveries: webhookDeliveries}, nil
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
	river.WorkerDefaults[testArgs]
	service *Service
}

func (*testWorker) NextRetry(job *river.Job[testArgs]) time.Time {
	return time.Now().Add(time.Duration(job.Attempt) * time.Second)
}

var (
	errOperationTerminal = errors.New("operation is terminal")
	errNotOwned          = errors.New("operation is owned by a newer attempt")
)

func (w *testWorker) Work(ctx context.Context, job *river.Job[testArgs]) error {
	attempt := uint32(job.Attempt)
	op, raw, claimed, err := w.service.claimAttempt(ctx, job.Args.OperationID, attempt, "diagnostic")
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
		return w.settleAttempt(ctx, op.ID, attempt, w.reconcileCancellation(ctx, op.ID, attempt, input))
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
			return w.settleAttempt(ctx, op.ID, attempt, w.reconcileCancellation(ctx, op.ID, attempt, input))
		}
		if err := w.runStep(ctx, op.ID, attempt, index, input); err != nil {
			return w.settleAttempt(ctx, op.ID, attempt, err)
		}
	}
	_, err = w.service.finishSucceeded(ctx, op.ID, attempt)
	return w.settleAttempt(ctx, op.ID, attempt, err)
}

// settleAttempt completes terminal work and makes a lost ownership race wait for
// the newer attempt. River's completion query is not attempt-fenced.
func (w *testWorker) settleAttempt(ctx context.Context, id contract.OperationID, attempt uint32, err error) error {
	if errors.Is(err, errOperationTerminal) {
		return nil
	}
	if errors.Is(err, errNotOwned) {
		return w.service.waitForNewerAttempt(ctx, id, attempt)
	}
	return err
}

func diagnosticStepID(index int) string { return node.DiagnosticStepID(index) }

func (w *testWorker) injectedFailure(ctx context.Context, id contract.OperationID, attempt uint32, job *river.Job[testArgs]) error {
	op, owned, err := w.service.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if current.State == contract.Cancelling {
			return transition(ctx, tx, current, contract.Canceled, "", "Diagnostic cancellation acknowledged.")
		}
		if job.Attempt >= job.MaxAttempts {
			if err := markIncompleteSteps(ctx, tx, id, contract.Unavailable, "retry limit reached before the step completed"); err != nil {
				return err
			}
			return transition(ctx, tx, current, contract.Failed, contract.Unavailable, "Diagnostic retry limit reached.")
		}
		if err := appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Message: "Diagnostic requested a retry; work remains durably queued."}); err != nil {
			return err
		}
		return w.service.appendActionResultTx(ctx, tx, current, contract.Failed, contract.Unavailable, "Diagnostic requested a retry; work remains durably queued.", false)
	})
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
func (w *testWorker) runStep(ctx context.Context, id contract.OperationID, attempt uint32, index int, input operations.TestInput) error {
	if w.service.transport == nil {
		return contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	stepID := diagnosticStepID(index)
	exec := engine.Execution{OperationID: id, Attempt: attempt, Step: stepID, RequestID: newRequestID()}
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

	record, hasRecord, err := w.service.stepRecord(ctx, id, stepID)
	if err != nil {
		return err
	}
	if hasRecord {
		if record.InputsHash != inputsHash {
			return w.failInputsMismatch(ctx, id, attempt, "step inputs changed for an existing effect")
		}
		if record.State == "completed" {
			return nil
		}
	}

	status, err := w.service.transport.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil {
		if errors.Is(err, engine.ErrJournalCorrupt) {
			if failErr := w.service.failOperation(ctx, id, attempt, contract.PreconditionFailed, "Node journal is corrupt; inspect it before retrying."); failErr != nil {
				return failErr
			}
			return errOperationTerminal
		}
		if node.IsPermanentTransport(err) {
			return w.failTransport(ctx, id, attempt, err)
		}
		return fmt.Errorf("node status %s: %w", stepID, err)
	}
	if status.InputsHash != "" && status.InputsHash != inputsHash {
		return w.failInputsMismatch(ctx, id, attempt, "node effect has different inputs")
	}
	if status.Status == engine.StatusCompleted {
		if !hasRecord {
			if _, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID); err != nil {
				return err
			}
		}
		if err := w.service.recordRecoveredStep(ctx, id, attempt, stepID, inputsHash, status); err != nil {
			return err
		}
		_, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, status.ExecutionID, status.Result, input.Steps)
		return err
	}

	already, err := w.service.beginStep(ctx, id, attempt, stepID, inputsHash, exec.ExecutionID())
	if err != nil {
		return err
	}
	if already {
		return nil
	}

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
			return w.failTransport(ctx, id, attempt, err)
		}
		// A lost stream is an unknown outcome, never proof that the node
		// stopped. The next attempt asks the node for its status first.
		return fmt.Errorf("node run %s: %w", stepID, err)
	}
	if w.service.afterNode != nil {
		if err := w.service.afterNode(id, stepID); err != nil {
			return err
		}
	}
	if response.InputsHash != "" && response.InputsHash != inputsHash {
		return w.failInputsMismatch(ctx, id, attempt, "node effect has different inputs")
	}
	switch response.Status {
	case engine.StatusCompleted:
		_, err := w.service.ackStep(ctx, id, attempt, stepID, inputsHash, response.ExecutionID, response.Result, input.Steps)
		return err
	case engine.StatusRunning:
		// A live node owner holds the effect beyond our lock timeout. Do not
		// start a second Apply; let River retry and reconcile.
		return contract.NewFault(contract.Unavailable, "node task is still running", "The operation remains running; reconnecting or retrying will attach to the existing effect, not start another.")
	case engine.StatusFailed:
		if err := w.service.retainNodeResponse(ctx, exec, response); err != nil {
			return err
		}
		code := response.ErrorCode
		if code == "" {
			code = string(contract.Internal)
		}
		if failErr := w.service.failStep(ctx, id, attempt, stepID, contract.Code(code), response.Detail); failErr != nil {
			return failErr
		}
		if response.Retryable {
			if err := w.service.recordRetryResult(ctx, id, attempt, contract.Code(code), w.service.resultText(response.Detail)); err != nil {
				return err
			}
			return fmt.Errorf("node step %s failed: %s", stepID, response.Detail)
		}
		if response.InputsMismatch {
			return w.failInputsMismatch(ctx, id, attempt, "node step failed with different inputs")
		}
		if err := w.service.failOperation(ctx, id, attempt, contract.Code(code), "Node step failed without a retryable cause: "+response.Detail); err != nil {
			return err
		}
		return errOperationTerminal
	default:
		return fmt.Errorf("node returned unexpected status %q for %s", response.Status, stepID)
	}
}

// failTransport ends an operation whose transport refused the request for a
// reason a retry cannot fix, such as a host-key mismatch.
func (w *testWorker) failTransport(ctx context.Context, id contract.OperationID, attempt uint32, cause error) error {
	message := "The node transport refused the request: " + cause.Error()
	if err := w.service.failOperation(ctx, id, attempt, contract.PreconditionFailed, message); err != nil {
		return err
	}
	return errOperationTerminal
}

// failInputsMismatch ends the operation once. A recorded effect under other
// inputs can never converge, so returning a retryable error would only loop.
func (w *testWorker) failInputsMismatch(ctx context.Context, id contract.OperationID, attempt uint32, message string) error {
	fault := contract.NewFault(contract.PreconditionFailed, message, "Do not re-apply an effect recorded under other inputs; inspect the node journal.")
	if err := w.service.failOperation(ctx, id, attempt, contract.PreconditionFailed, fault.Message); err != nil {
		return err
	}
	return errOperationTerminal
}

func diagnosticPayload(index int, input operations.TestInput) json.RawMessage {
	raw, _ := json.Marshal(node.DiagnosticPayload{Index: index, Total: input.Steps, StepDelayMS: input.StepDelayMS})
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
func (w *testWorker) reconcileCancellation(ctx context.Context, id contract.OperationID, attempt uint32, input operations.TestInput) error {
	live, err := w.service.reconcileCompletedSteps(ctx, id, attempt, input)
	if err != nil {
		return err
	}
	if live {
		return contract.NewFault(contract.Unavailable, "node task is still running", "Cancellation waits for the in-flight step boundary; retry to reconcile the completed effect.")
	}
	if _, err := w.service.finishCancelled(ctx, id, attempt, input.Steps); err != nil {
		return err
	}
	return nil
}

type reconcileWorker struct {
	river.WorkerDefaults[reconcileArgs]
	service *Service
}

func (w *reconcileWorker) Work(ctx context.Context, _ *river.Job[reconcileArgs]) error {
	return w.service.Reconcile(ctx)
}

type domainRouteWorker struct {
	river.WorkerDefaults[domainRouteArgs]
	service *Service
}

func (w *domainRouteWorker) Work(ctx context.Context, _ *river.Job[domainRouteArgs]) error {
	// Periodic reconciliation retries on its own; returning the error would
	// only make River attach a retry to the same schedule.
	logDomainMaintenance("route reconciliation", w.service.reconcileDomainRoutes(ctx))
	return nil
}

type domainObserveWorker struct {
	river.WorkerDefaults[domainObserveArgs]
	service *Service
}

func (w *domainObserveWorker) Work(ctx context.Context, _ *river.Job[domainObserveArgs]) error {
	logDomainMaintenance("observation", w.service.observeDomains(ctx))
	return nil
}

type containerCleanupWorker struct {
	river.WorkerDefaults[containerCleanupArgs]
	service *Service
}

func (w *containerCleanupWorker) Work(ctx context.Context, _ *river.Job[containerCleanupArgs]) error {
	logMaintenance("container cleanup", w.service.retryDueContainerCleanups(ctx))
	return nil
}

type workloadRepairWorker struct {
	river.WorkerDefaults[workloadRepairArgs]
	service *Service
}

func (w *workloadRepairWorker) Work(ctx context.Context, _ *river.Job[workloadRepairArgs]) error {
	logMaintenance("workload repair", w.service.repairServingWorkloads(ctx))
	return nil
}

// Reconcile covers exhausted attempts lost to a process crash, not just errors
// returned by Work. Domain terminal state cannot depend on an in-process callback.
func (s *Service) Reconcile(ctx context.Context) error {
	var failures []error
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
	rows, err := s.pool.Query(ctx, `SELECT o.id FROM eregion_operations o LEFT JOIN river_job j ON j.id=o.job_id
        WHERE o.state IN ('queued','running','cancelling') AND (j.id IS NULL OR j.state IN ('discarded','cancelled','completed')) LIMIT 100`)
	if err != nil {
		return unavailable()
	}
	var ids []contract.OperationID
	for rows.Next() {
		var id contract.OperationID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
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
	op, err := scanOperation(s.pool.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return unavailable()
	}
	if op.State.Terminal() {
		return nil
	}
	if op.Kind == platform.KindDefinitionApply {
		return s.requeueDefinitionDrain(ctx, id)
	}
	if s.transport == nil {
		return contract.NewFault(contract.Unavailable, "node transport is not configured", "Reconciliation requires a worker node transport.")
	}
	if strings.HasPrefix(op.Kind, "platform.") {
		return s.reconcilePlatformOperation(ctx, op)
	}
	var input operations.TestInput
	var rawInput []byte
	if err := s.pool.QueryRow(ctx, "SELECT input FROM eregion_operations WHERE id=$1", id).Scan(&rawInput); err != nil {
		return unavailable()
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return err
	}
	live := false
	records, err := s.stepRecords(ctx, id)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.State == "completed" {
			continue
		}
		exec := engine.Execution{OperationID: id, Attempt: op.Attempt, Step: record.Step, RequestID: newRequestID()}
		status, statusErr := s.transport.Call(ctx, node.Request{Method: node.MethodStatus, RequestID: exec.RequestID, Execution: exec}, nil)
		if statusErr != nil {
			if errors.Is(statusErr, engine.ErrJournalCorrupt) {
				err := s.failOperation(ctx, id, op.Attempt, contract.PreconditionFailed, "Node journal is corrupt; inspect it before retrying.")
				if errors.Is(err, errNotOwned) {
					return nil
				}
				return err
			}
			if node.IsPermanentTransport(statusErr) {
				err := s.failOperation(ctx, id, op.Attempt, contract.PreconditionFailed, "The node transport refused the request: "+statusErr.Error())
				if errors.Is(err, errNotOwned) {
					return nil
				}
				return err
			}
			return unavailable(statusErr)
		}
		if status.InputsHash != "" && status.InputsHash != record.InputsHash {
			// The write is attempt-fenced like every other worker write: a
			// newer attempt owns the operation and must not be overwritten.
			_, owned, err := s.mutateOwned(ctx, id, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
				if current.State.Terminal() {
					return nil
				}
				return transition(ctx, tx, current, contract.Failed, contract.PreconditionFailed, "Node effect inputs changed during reconciliation; inspect the node journal.")
			})
			if !owned || errors.Is(err, errNotOwned) {
				return nil
			}
			return err
		}
		if status.Status == engine.StatusCompleted {
			if _, err := s.ackStep(ctx, id, op.Attempt, record.Step, record.InputsHash, status.ExecutionID, status.Result, input.Steps); err != nil {
				if errors.Is(err, errNotOwned) {
					return nil
				}
				return err
			}
			continue
		}
		if status.Status == engine.StatusRunning {
			live = true
		}
	}
	var completed int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_steps WHERE operation_id=$1 AND state='completed'`, id).Scan(&completed); err != nil {
		return unavailable()
	}
	if completed == input.Steps {
		_, err := s.finishSucceeded(ctx, id, op.Attempt)
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	if live {
		_, owned, err := s.mutateOwned(ctx, id, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
			if current.State.Terminal() {
				return nil
			}
			if current.State == contract.Queued {
				if err := transition(ctx, tx, current, contract.Running, "", "Reconciled a live node task after its job ended."); err != nil {
					return err
				}
			}
			return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Message: "Execution ended without a domain result, but the node task is still running; cancellation and failure wait for the in-flight step boundary."})
		})
		if !owned || errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	if op.State == contract.Cancelling {
		_, err := s.finishCancelled(ctx, id, op.Attempt, input.Steps)
		if errors.Is(err, errNotOwned) {
			return nil
		}
		return err
	}
	_, owned, err := s.mutateOwned(ctx, id, op.Attempt, func(tx pgx.Tx, current *contract.Operation) error {
		if current.State.Terminal() {
			return nil
		}
		if err := markIncompleteSteps(ctx, tx, id, contract.Unavailable, "execution ended before the step was acknowledged"); err != nil {
			return err
		}
		return transition(ctx, tx, current, contract.Failed, contract.Unavailable, "Execution ended without an acknowledged domain result; inspect operation history before retrying.")
	})
	if !owned || errors.Is(err, errNotOwned) {
		return nil
	}
	return err
}

// waitForNewerAttempt does not write domain state while a newer River attempt
// owns the operation. River's completion query is not attempt-fenced, so
// returning early could complete the shared job row underneath newer work.
func (s *Service) waitForNewerAttempt(ctx context.Context, id contract.OperationID, attempt uint32) error {
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

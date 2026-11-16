// Package jobargs owns the River job argument types and queue names shared by
// the control plane and the typed persistence runtime. Arguments carry durable
// identity only: input stays in PostgreSQL, and no secret material enters a
// River row. The package depends on nothing but the public contract so both
// sides can import it without a cycle.
package jobargs

import "github.com/mbvlabs/eregion/internal/contract"

// Queue names are one River runtime contract. The worker registration and the
// enqueue helpers must use the same values.
const (
	DiagnosticQueue  = "diagnostic"
	PlatformQueue    = "platform"
	WebhookQueue     = "webhook"
	MaintenanceQueue = "maintenance"
)

// PlatformMaxAttempts bounds one platform, webhook or maintenance operation
// job. The tried attempts are recorded on the operation, not inferred from a
// process callback.
const PlatformMaxAttempts = 8

// DiagnosticTest is one diagnostic.test execution. The argument deliberately
// contains an operation reference only.
type DiagnosticTest struct {
	OperationID contract.OperationID `json:"operation_id"`
}

func (DiagnosticTest) Kind() string { return "diagnostic_test" }

// PlatformAction is one platform domain action. It carries only durable
// identity; the canonical input and secrets stay in PostgreSQL.
type PlatformAction struct {
	OperationID contract.OperationID `json:"operation_id"`
	ActionKind  string               `json:"action_kind"`
}

func (PlatformAction) Kind() string { return "platform_action" }

// OperationReconcile repairs work whose River job vanished or ended without a
// domain result.
type OperationReconcile struct{}

func (OperationReconcile) Kind() string { return "operation_reconcile" }

// DomainRoutes reconciles the routes file.
type DomainRoutes struct{}

func (DomainRoutes) Kind() string { return "platform_domain_routes" }

// DomainObserve observes DNS, certificates and public responses.
type DomainObserve struct{}

func (DomainObserve) Kind() string { return "platform_domain_observe" }

// ContainerCleanup retries containers a verified switch or failed candidate
// could not remove.
type ContainerCleanup struct{}

func (ContainerCleanup) Kind() string { return "platform_container_cleanup" }

// WorkloadRepair restores a serving container that disappeared.
type WorkloadRepair struct{}

func (WorkloadRepair) Kind() string { return "platform_workload_repair" }

// Retention is one durable disk-retention attempt. The periodic daily job
// leaves OperationID empty and submits the day's operation; the operation's own
// job carries its ID on the maintenance queue.
type Retention struct {
	OperationID contract.OperationID `json:"operation_id,omitempty"`
}

func (Retention) Kind() string { return "platform_retention" }

// WebhookAdmission carries the environment identity and delivery hash. The
// body, signature, secret, sender and raw delivery ID never enter River
// arguments or logs.
type WebhookAdmission struct {
	EnvironmentID  string `json:"environment_id"`
	DeliveryIDHash string `json:"delivery_id_hash"`
}

func (WebhookAdmission) Kind() string { return "webhook_admission" }

package resources

import "time"

// Schema versions the resource/backup contract returned by this package. It is
// independent of the CLI envelope schema.
const Schema = "eregion/resources/v1"

// Engine is a supported application resource engine. PostgreSQL is the only
// implemented engine; other research engines need an explicit scope decision.
type Engine string

const EnginePostgreSQL Engine = "postgresql"

// Protection marks whether a resource is an ordinary application resource or a
// database that belongs to the control plane. A control-plane database must use
// its separate recovery procedure, never ordinary resource destruction.
type Protection string

const (
	ProtectionApplication  Protection = "application"
	ProtectionControlPlane Protection = "control-plane"
)

// Action is one resource operation.
type Action string

const (
	ActionCreate  Action = "create"
	ActionHealth  Action = "health"
	ActionDestroy Action = "destroy"
	ActionBackup  Action = "backup"
	ActionVerify  Action = "verify"
	ActionRestore Action = "restore"
	// ActionRecover rolls back an interrupted restore recorded in the durable
	// restore journal. It does not start a new restore or need a backup archive.
	ActionRecover Action = "recover"
	// ActionCredentials writes an application credential bundle to one explicit
	// protected destination file. It never returns plaintext in its result.
	ActionCredentials Action = "credentials"
	// ActionSchedule records a backup policy declaration. It does not start a
	// scheduler; the orchestrator owns River periodic registration.
	ActionSchedule Action = "schedule"
)

func (a Action) Valid() bool {
	switch a {
	case ActionCreate, ActionHealth, ActionDestroy, ActionBackup, ActionVerify, ActionRestore, ActionRecover, ActionCredentials, ActionSchedule:
		return true
	}
	return false
}

func (a Action) Mutating() bool {
	switch a {
	case ActionCreate, ActionDestroy, ActionBackup, ActionRestore, ActionRecover, ActionCredentials, ActionSchedule:
		return true
	}
	return false
}

// State is an honest observation of the managed container, not a desired state.
type State string

const (
	StateAbsent   State = "absent"
	StateStarting State = "starting"
	StateReady    State = "ready"
	StateDegraded State = "degraded"
	StateStopped  State = "stopped"
	StateUnknown  State = "unknown"
)

// HealthStatus is the result of the explicit health probe.
type HealthStatus string

const (
	HealthHealthy   HealthStatus = "healthy"
	HealthStarting  HealthStatus = "starting"
	HealthUnhealthy HealthStatus = "unhealthy"
	HealthStopped   HealthStatus = "stopped"
	HealthAbsent    HealthStatus = "absent"
	HealthUnknown   HealthStatus = "unknown"
)

// Spec is the complete requested application-resource identity. Host is an
// explicit application host placement; it is never inferred or defaulted.
type Spec struct {
	// Name is the stable, caller-owned resource identity. It is also the Docker
	// ownership label and part of the deterministic container/volume names.
	Name string `json:"name"`
	// Engine must be postgresql.
	Engine Engine `json:"engine"`
	// Version is a PostgreSQL major version such as "17". The image mapping is
	// service configuration, not caller-provided free-form image text.
	Version string `json:"version"`
	// Host is the application host that runs the container and named volume. It
	// must equal the configured Service host.
	Host string `json:"host"`
	// Address is the host address for the published port. It defaults to
	// 127.0.0.1 and must be an explicit IP address, never 0.0.0.0.
	Address string `json:"address,omitempty"`
	// Port is the host port. Zero lets Docker choose an ephemeral port.
	Port int `json:"port,omitempty"`
	// Database and Username are PostgreSQL identifiers created by the official
	// image on first initialisation.
	Database string `json:"database"`
	Username string `json:"username"`
	// Volume is the named Docker volume. Empty derives a deterministic private
	// name. Supplying a volume never adopts an unlabelled or foreign volume.
	Volume string `json:"volume,omitempty"`
	// Protection is application by default. control-plane is refused for
	// ordinary lifecycle and backup actions.
	Protection Protection `json:"protection,omitempty"`
}

// Endpoint is the application-facing connection description. It contains no
// credential value.
type Endpoint struct {
	Node     string `json:"node"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	SSLMode  string `json:"sslmode"`
}

// CredentialReference names a protected file; it never carries the password.
type CredentialReference struct {
	Username     string `json:"username"`
	Database     string `json:"database"`
	PasswordFile string `json:"password_file"`
	Mode         string `json:"mode"`
}

// CredentialExport describes one explicitly requested protected-file export. It
// names the destination and never contains the credential value.
type CredentialExport struct {
	Destination string `json:"destination"`
	Mode        string `json:"mode"`
	Username    string `json:"username"`
	Database    string `json:"database"`
}

// Health is one explicit observation. It is not a promise of continuous
// availability.
type Health struct {
	Status    HealthStatus `json:"status"`
	Endpoint  *Endpoint    `json:"endpoint,omitempty"`
	Detail    string       `json:"detail,omitempty"`
	CheckedAt time.Time    `json:"checked_at"`
}

// BackupArtifact describes one verified-able pg_dump archive. Verified is only
// true after this process performed the digest and pg_restore --list checks.
type BackupArtifact struct {
	Archive    string     `json:"archive"`
	Manifest   string     `json:"manifest"`
	Resource   string     `json:"resource"`
	Host       string     `json:"host"`
	Database   string     `json:"database"`
	EffectID   string     `json:"effect_id"`
	SizeBytes  int64      `json:"size_bytes"`
	SHA256     string     `json:"sha256"`
	CreatedAt  time.Time  `json:"created_at"`
	Reason     string     `json:"reason"`
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

// Recovery reports whether a failed restore was rolled back to the previous
// database.
type Recovery struct {
	Required  bool   `json:"required"`
	Attempted bool   `json:"attempted"`
	Succeeded bool   `json:"succeeded"`
	Detail    string `json:"detail,omitempty"`
}

// Result is the durable-action result. It is safe to place in an operation
// event: it contains a password file reference, never the password itself.
type Result struct {
	Schema   string `json:"schema"`
	Action   Action `json:"action"`
	Resource string `json:"resource"`
	Host     string `json:"host"`
	Engine   Engine `json:"engine"`
	// OperationID and EffectID are the durable identity supplied by the
	// orchestrator and the stable effect key used for replay. Neither is a
	// credential.
	OperationID string `json:"operation_id,omitempty"`
	EffectID    string `json:"effect_id,omitempty"`
	// Replayed is true when a completed effect result was returned without
	// reapplying the action.
	Replayed     bool                 `json:"replayed,omitempty"`
	State        State                `json:"state"`
	Changed      bool                 `json:"changed"`
	Endpoint     *Endpoint            `json:"endpoint,omitempty"`
	Credential   *CredentialReference `json:"credential,omitempty"`
	Health       *Health              `json:"health,omitempty"`
	Backup       *BackupArtifact      `json:"backup,omitempty"`
	RestoredFrom *BackupArtifact      `json:"restored_from,omitempty"`
	SafetyBackup *BackupArtifact      `json:"safety_backup,omitempty"`
	Recovery     *Recovery            `json:"recovery,omitempty"`
	Export       *CredentialExport    `json:"export,omitempty"`
	Scheduled    *ScheduledBackup     `json:"scheduled,omitempty"`
	Warnings     []string             `json:"warnings,omitempty"`
	ObservedAt   time.Time            `json:"observed_at"`
}

// Request is the complete service input. It is safe to serialize into a River
// job argument because it contains no credential value. Credentials are
// generated and stored by the service from the resource identity.
type Request struct {
	Action Action `json:"action"`
	Spec   Spec   `json:"spec"`
	// OperationID is the orchestrator's durable operation identity. EffectID is
	// an optional explicit idempotency key; when empty it defaults to
	// OperationID. Mutating actions require one of them. Neither is a secret.
	OperationID string          `json:"operation_id,omitempty"`
	EffectID    string          `json:"effect_id,omitempty"`
	Backup      *BackupRequest  `json:"backup,omitempty"`
	Verify      *VerifyRequest  `json:"verify,omitempty"`
	Destroy     *DestroyRequest `json:"destroy,omitempty"`
	Restore     *RestoreRequest `json:"restore,omitempty"`
	Export      *ExportRequest  `json:"export,omitempty"`
	Policy      *Policy         `json:"policy,omitempty"`
}

// BackupRequest controls one archive. Destination comes from Service
// configuration, not from a job argument.
type BackupRequest struct {
	// Reason is manual (default), scheduled, or safety. It is recorded in the
	// manifest and is not an authorization.
	Reason string `json:"reason,omitempty"`
	// Policy is an optional declaration for an orchestrator's periodic job. It
	// is validated but does not schedule work inside this package.
	Policy *Policy `json:"policy,omitempty"`
}

// VerifyRequest names an archive, absolute or relative to the configured backup
// directory.
type VerifyRequest struct {
	Archive string `json:"archive"`
}

// DestroyRequest requires the exact resource name. Removing the named volume
// (the database data) additionally requires RemoveVolume=true.
type DestroyRequest struct {
	Confirmation string `json:"confirmation"`
	RemoveVolume bool   `json:"remove_volume"`
}

// RestoreRequest requires the exact resource name and a verified backup
// archive. A safety backup is always taken before the target is replaced.
type RestoreRequest struct {
	Archive      string  `json:"archive"`
	Confirmation string  `json:"confirmation"`
	Policy       *Policy `json:"policy,omitempty"`
}

// ExportRequest names one new protected destination file. The credential
// contents are written there; they are never part of the request or result.
type ExportRequest struct {
	Destination string `json:"destination"`
}

// PlanStep is one ordered execution step. A plan performs no mutation.
type PlanStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Destructive bool   `json:"destructive"`
}

// Plan is a non-mutating preview. It is not an authorization token; Execute
// revalidates the request and the confirmation.
type Plan struct {
	Schema               string     `json:"schema"`
	Action               Action     `json:"action"`
	Resource             string     `json:"resource"`
	Host                 string     `json:"host"`
	Engine               Engine     `json:"engine"`
	CurrentState         State      `json:"current_state"`
	Steps                []PlanStep `json:"steps"`
	Destructive          bool       `json:"destructive"`
	ConfirmationRequired bool       `json:"confirmation_required"`
	ConfirmationPhrase   string     `json:"confirmation_phrase,omitempty"`
	BackupDestination    string     `json:"backup_destination,omitempty"`
	SafetyBackup         bool       `json:"safety_backup"`
	Warnings             []string   `json:"warnings,omitempty"`
}

// Preview is the static dry-run view attached to an action input. The
// control-plane API returns it without inserting an operation or River job.
// Current container state is read by resources status instead; a preview is not
// an authorization token and Execute revalidates everything.
type Preview struct {
	Schema               string     `json:"schema"`
	Action               Action     `json:"action"`
	Resource             string     `json:"resource"`
	Host                 string     `json:"host"`
	Engine               Engine     `json:"engine"`
	Steps                []PlanStep `json:"steps"`
	Destructive          bool       `json:"destructive"`
	ConfirmationRequired bool       `json:"confirmation_required"`
	ConfirmationPhrase   string     `json:"confirmation_phrase,omitempty"`
	SafetyBackup         bool       `json:"safety_backup"`
	Warnings             []string   `json:"warnings,omitempty"`
}

// staticPreview describes what an action would do without contacting Docker.
func staticPreview(action Action, spec Spec, removeVolume bool) Preview {
	preview := Preview{
		Schema: Schema, Action: action, Resource: spec.Name, Host: spec.Host, Engine: spec.Engine,
	}
	switch action {
	case ActionCreate:
		preview.Steps = []PlanStep{
			{ID: "validate", Description: "Validate the resource identity, version, explicit application host, endpoint, and credential references."},
			{ID: "volume", Description: "Create or verify the labelled named volume."},
			{ID: "container", Description: "Create or start the PostgreSQL container with the protected password file mounted read-only."},
			{ID: "health", Description: "Wait for pg_isready on the application host and publish the observed endpoint."},
		}
	case ActionHealth:
		preview.Steps = []PlanStep{{ID: "health", Description: "Inspect the owned container and run pg_isready. No mutation."}}
	case ActionDestroy:
		preview.Destructive, preview.ConfirmationRequired, preview.ConfirmationPhrase = true, true, spec.Name
		preview.Steps = []PlanStep{
			{ID: "confirm", Description: "Require the exact resource name and refuse the control-plane database.", Destructive: true},
			{ID: "container", Description: "Remove only the container whose ownership labels match this resource.", Destructive: true},
		}
		if removeVolume {
			preview.Steps = append(preview.Steps, PlanStep{ID: "volume", Description: "Remove the labelled named volume and protected credential files (database data is deleted).", Destructive: true})
		} else {
			preview.Steps = append(preview.Steps, PlanStep{ID: "volume", Description: "Keep the named volume and credentials; data remains recoverable."})
		}
	case ActionBackup:
		preview.Steps = []PlanStep{
			{ID: "ready", Description: "Verify the owned container is accepting connections."},
			{ID: "dump", Description: "Run pg_dump --format=custom into a temporary file in the configured backup directory."},
			{ID: "checksum", Description: "Compute SHA-256, publish the archive atomically, and write its manifest."},
		}
	case ActionVerify:
		preview.Steps = []PlanStep{
			{ID: "checksum", Description: "Recompute and compare the archive SHA-256 with its manifest."},
			{ID: "list", Description: "Run pg_restore --list against the archive and require a nonzero table of contents."},
		}
	case ActionRestore:
		preview.Destructive, preview.ConfirmationRequired, preview.ConfirmationPhrase = true, true, spec.Name
		preview.SafetyBackup = true
		preview.Steps = []PlanStep{
			{ID: "confirm", Description: "Require the exact resource name and refuse the control-plane database.", Destructive: true},
			{ID: "verify", Description: "Verify the selected archive, its manifest, resource identity, checksum, and pg_restore listing."},
			{ID: "safety", Description: "Take and verify a safety backup before any target change."},
			{ID: "temporary", Description: "Restore into a temporary database next to the target."},
			{ID: "cutover", Description: "Terminate target connections, rename the old database aside, and rename the restored database into place.", Destructive: true},
			{ID: "health", Description: "Probe the restored database; on failure roll the previous database back.", Destructive: true},
		}
	case ActionRecover:
		preview.Steps = []PlanStep{{ID: "recover", Description: "Recover an interrupted restore from the durable journal, restoring the previous database when needed."}}
	case ActionCredentials:
		preview.Steps = []PlanStep{
			{ID: "credential", Description: "Read the protected credential for the owned resource."},
			{ID: "export", Description: "Write one new 0600 credential bundle to the explicit destination file; plaintext is never printed or returned."},
			{ID: "report", Description: "Report only the destination path and non-secret endpoint metadata."},
		}
		preview.Warnings = append(preview.Warnings, "the destination path is on the application host; the file contains a credential")
	case ActionSchedule:
		preview.Steps = []PlanStep{
			{ID: "validate", Description: "Validate the interval or cron expression, timezone, and retention bound."},
			{ID: "record", Description: "Write the secret-free backup policy declaration to the protected state directory."},
			{ID: "report", Description: "Return the River periodic declaration; registration is owned by the orchestrator."},
		}
		preview.Warnings = append(preview.Warnings, "this records policy only; it does not execute a backup or start a scheduler")
	}
	return preview
}

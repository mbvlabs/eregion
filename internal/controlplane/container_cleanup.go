package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
)

// containerCleanupRow is one leaked container waiting for a maintenance retry.
type containerCleanupRow struct {
	ContainerName string
	EnvironmentID platform.EnvironmentID
	App           string
	Environment   string
	Node          string
	DeploymentID  platform.DeploymentID
	Reason        string
	Attempts      int
	CreatedAt     time.Time
}

var errCleanupDeferred = errors.New("container cleanup is waiting for deployment ownership or serving state to settle")

const containerCleanupPageSize = 50

// containerCleanupDeferInterval moves a guarded row out of the current due page
// without counting a removal attempt. Without this, the oldest protected rows
// would occupy every page forever and starve later cleanup rows.
const containerCleanupDeferInterval = 5 * time.Minute

// containerCleanupBackoff grows from one minute to a one-hour ceiling.
func containerCleanupBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	backoff := time.Minute
	for index := 1; index < attempts; index++ {
		backoff *= 2
		if backoff >= time.Hour {
			return time.Hour
		}
	}
	return backoff
}

// cleanupCauseLimit bounds the removal reason one cleanup warning carries. A
// Docker answer is useful for diagnosis, not a transcript.
const cleanupCauseLimit = 240

// cleanupErrorText is the most specific reason a cleanup failure carries. A
// classified fault keeps its public message; an unclassified step error is the
// node's own text (Docker's stderr or the transport's reason).
func cleanupErrorText(err error) string {
	if err == nil {
		return ""
	}
	if fault := contract.Classify(err); fault.Code != contract.Internal {
		return fault.Message
	}
	return err.Error()
}

// boundedCleanupCause flattens untrusted failure text to one visible line,
// cutting anything longer than cleanupCauseLimit runes and marking the cut with
// an ellipsis. Docker's answer is not trusted terminal input, so controls are
// escaped and line breaks collapse to spaces. Callers must redact it with the
// worker registry first: the registry matches exact values, so a secret that
// crosses the truncation boundary or contains normalizable whitespace is only
// removable from the raw text.
func boundedCleanupCause(text string) string {
	text = strings.Join(strings.Fields(output.SafeText(text)), " ")
	runes := []rune(text)
	if len(runes) > cleanupCauseLimit {
		return string(runes[:cleanupCauseLimit]) + "…"
	}
	return text
}

// cleanupCauseText is one failed removal as the durable cleanup row records it.
// Redaction runs on the raw text before normalization and truncation; doing it
// afterwards could leave a fragment of a secret that crossed the boundary or
// contained a collapsed newline.
func (s *Service) cleanupCauseText(err error) string {
	return boundedCleanupCause(s.redact(cleanupErrorText(err)))
}

// recordContainerCleanup upserts one leaked container and schedules its next
// attempt. Success is recorded by deleting the row.
func (s *Service) recordContainerCleanup(ctx context.Context, containerName string, environmentID platform.EnvironmentID, nodeName string, deploymentID platform.DeploymentID, reason string, cause error) error {
	detail := ""
	if cause != nil {
		detail = s.cleanupCauseText(cause)
	}
	var attempts int
	if err := s.pool.QueryRow(ctx, `INSERT INTO eregion_platform_container_cleanup
        (container_name,environment_id,node,deployment_id,reason,attempts,last_error,next_attempt_at,created_at)
        VALUES($1,$2,$3,$4,$5,1,$6,clock_timestamp(),clock_timestamp())
        ON CONFLICT (container_name) DO UPDATE
        SET environment_id=EXCLUDED.environment_id,node=EXCLUDED.node,
            deployment_id=EXCLUDED.deployment_id,reason=EXCLUDED.reason,
            attempts=eregion_platform_container_cleanup.attempts+1,
            last_error=EXCLUDED.last_error,next_attempt_at=clock_timestamp()
        RETURNING attempts`,
		containerName, environmentID, nodeName, nullableDeploymentID(deploymentID), reason, detail).Scan(&attempts); err != nil {
		return unavailable()
	}
	next := fmt.Sprintf("%d seconds", int(containerCleanupBackoff(attempts).Seconds()))
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_container_cleanup
        SET next_attempt_at=clock_timestamp()+$2::interval WHERE container_name=$1`, containerName, next); err != nil {
		return unavailable()
	}
	return nil
}

// ensureContainerCleanup records a cleanup candidate without counting a new
// removal failure. Terminal/ambiguous owner recovery and protected-cleanup
// deferrals can repeat this operation without inflating attempts or backoff.
func (s *Service) ensureContainerCleanup(ctx context.Context, containerName string, environmentID platform.EnvironmentID, nodeName string, deploymentID platform.DeploymentID, reason string, cause error) error {
	detail := ""
	if cause != nil {
		detail = s.cleanupCauseText(cause)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO eregion_platform_container_cleanup
        (container_name,environment_id,node,deployment_id,reason,attempts,last_error,next_attempt_at,created_at)
        VALUES($1,$2,$3,$4,$5,1,$6,clock_timestamp(),clock_timestamp())
        ON CONFLICT (container_name) DO UPDATE
        SET environment_id=EXCLUDED.environment_id,node=EXCLUDED.node,
            deployment_id=EXCLUDED.deployment_id,reason=EXCLUDED.reason,
            last_error=EXCLUDED.last_error`, containerName, environmentID, nodeName,
		nullableDeploymentID(deploymentID), reason, detail); err != nil {
		return unavailable(err)
	}
	return nil
}

// retryDueContainerCleanups removes due leaked containers. A success deletes
// the row; any failure is recorded again with the next backoff.
func (s *Service) retryDueContainerCleanups(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT c.container_name,c.environment_id,a.name,e.name,c.node,
            coalesce(c.deployment_id,''),c.reason,c.attempts,c.created_at
        FROM eregion_platform_container_cleanup c
        JOIN eregion_platform_environments e ON e.id=c.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE c.next_attempt_at <= clock_timestamp()
        ORDER BY c.next_attempt_at,c.container_name LIMIT $1`, containerCleanupPageSize)
	if err != nil {
		return unavailable()
	}
	var due []containerCleanupRow
	for rows.Next() {
		var row containerCleanupRow
		var deploymentID string
		if err := rows.Scan(&row.ContainerName, &row.EnvironmentID, &row.App, &row.Environment, &row.Node, &deploymentID,
			&row.Reason, &row.Attempts, &row.CreatedAt); err != nil {
			rows.Close()
			return unavailable()
		}
		row.DeploymentID = platform.DeploymentID(deploymentID)
		due = append(due, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	var failures []error
	for _, row := range due {
		if err := s.retryContainerCleanup(ctx, row); err != nil {
			if errors.Is(err, errCleanupDeferred) {
				if _, deferErr := s.pool.Exec(ctx, `UPDATE eregion_platform_container_cleanup
                    SET next_attempt_at=clock_timestamp()+$2::interval
                    WHERE container_name=$1`, row.ContainerName,
					fmt.Sprintf("%d seconds", int(containerCleanupDeferInterval.Seconds()))); deferErr != nil {
					failures = append(failures, unavailable(deferErr))
				}
				continue
			}
			if recordErr := s.recordContainerCleanup(ctx, row.ContainerName, row.EnvironmentID, row.Node, row.DeploymentID, row.Reason, err); recordErr != nil {
				failures = append(failures, recordErr)
			}
			failures = append(failures, fmt.Errorf("cleanup %s: %w", row.ContainerName, err))
			continue
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM eregion_platform_container_cleanup WHERE container_name=$1`, row.ContainerName); err != nil {
			failures = append(failures, unavailable())
		}
	}
	return errors.Join(failures...)
}

func (s *Service) retryContainerCleanup(ctx context.Context, row containerCleanupRow) error {
	release, err := s.lockTargets(ctx, []string{"env/" + row.App + "/" + row.Environment})
	if err != nil {
		return err
	}
	defer release()
	env, err := s.environment(ctx, row.App, row.Environment)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	protected, err := s.deploymentCleanupProtected(ctx, env, row.DeploymentID)
	if err != nil {
		return err
	}
	if protected {
		return errCleanupDeferred
	}
	return s.removeContainerCleanupRow(ctx, row)
}

// removeContainerCleanupRow performs one maintenance-style removal without
// taking the environment lock. It is used by the post-success best-effort pass
// while the worker already holds the lock.
func (s *Service) removeContainerCleanupRow(ctx context.Context, row containerCleanupRow) error {
	if s.resolver == nil {
		return contract.NewFault(contract.Unavailable, "the worker has no node resolver", "Run cleanup in a worker configured with node transports.")
	}
	tr, err := s.resolver.TransportFor(ctx, row.Node)
	if err != nil {
		return err
	}
	step := cleanupStepID(row.ContainerName)
	payload, err := json.Marshal(platform.ArgvPayload{
		Step: step, Argv: []string{"docker", "rm", "-f", row.ContainerName},
		MissingOK: true, TimeoutSeconds: 30,
	})
	if err != nil {
		return err
	}
	exec := engine.Execution{
		OperationID: cleanupOperationID(row), Attempt: uint32(row.Attempts),
		Step: step, RequestID: newRequestID(),
	}
	response, err := tr.Call(ctx, node.Request{
		Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec, Payload: payload,
	}, nil)
	if err != nil {
		return err
	}
	switch response.Status {
	case engine.StatusCompleted:
		return nil
	case engine.StatusFailed:
		return s.containerCleanupFailure(contract.Code(response.ErrorCode), response.Detail)
	default:
		return contract.NewFault(contract.Unavailable, "container cleanup did not finish", "The maintenance job will retry with backoff.")
	}
}

// removeDeploymentCleanupRowsNow is a best-effort immediate pass over the
// durable cleanup rows left by one terminal deployment. It runs after owner
// release, so the owner's candidate protection no longer applies. Any failure
// leaves the row for the normal maintenance retry.
func (s *Service) removeDeploymentCleanupRowsNow(ctx context.Context, env platform.Environment, deploymentID platform.DeploymentID) {
	protected, err := s.deploymentCleanupProtected(ctx, env, deploymentID)
	if err != nil || protected {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT c.container_name,c.environment_id,a.name,e.name,c.node,
            coalesce(c.deployment_id,''),c.reason,c.attempts,c.created_at
        FROM eregion_platform_container_cleanup c
        JOIN eregion_platform_environments e ON e.id=c.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE c.environment_id=$1 AND c.deployment_id=$2
        ORDER BY c.container_name`, env.ID, deploymentID)
	if err != nil {
		return
	}
	var due []containerCleanupRow
	for rows.Next() {
		var row containerCleanupRow
		var storedDeployment string
		if err := rows.Scan(&row.ContainerName, &row.EnvironmentID, &row.App, &row.Environment, &row.Node,
			&storedDeployment, &row.Reason, &row.Attempts, &row.CreatedAt); err != nil {
			rows.Close()
			return
		}
		row.DeploymentID = platform.DeploymentID(storedDeployment)
		due = append(due, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, row := range due {
		if err := s.removeContainerCleanupRow(ctx, row); err != nil {
			_ = s.recordContainerCleanup(ctx, row.ContainerName, row.EnvironmentID, row.Node, row.DeploymentID, row.Reason, err)
			continue
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM eregion_platform_container_cleanup WHERE container_name=$1`, row.ContainerName); err != nil {
			_ = s.recordContainerCleanup(ctx, row.ContainerName, row.EnvironmentID, row.Node, row.DeploymentID, row.Reason, err)
		}
	}
}

// removePreviousDeploymentCleanupNow is a best-effort immediate pass over the
// durable previous-cleanup rows after the success transaction committed. Any
// failure leaves the row in place for the normal maintenance retry.
func (s *Service) removePreviousDeploymentCleanupNow(ctx context.Context, env platform.Environment, deploymentID platform.DeploymentID) {
	protected, err := s.deploymentCleanupProtected(ctx, env, deploymentID)
	if err != nil || protected {
		return
	}
	rows, err := s.pool.Query(ctx, `SELECT c.container_name,c.environment_id,a.name,e.name,c.node,
            coalesce(c.deployment_id,''),c.reason,c.attempts,c.created_at
        FROM eregion_platform_container_cleanup c
        JOIN eregion_platform_environments e ON e.id=c.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE c.environment_id=$1 AND c.deployment_id=$2 AND c.reason='previous'
        ORDER BY c.container_name`, env.ID, deploymentID)
	if err != nil {
		return
	}
	var due []containerCleanupRow
	for rows.Next() {
		var row containerCleanupRow
		var storedDeployment string
		if err := rows.Scan(&row.ContainerName, &row.EnvironmentID, &row.App, &row.Environment, &row.Node,
			&storedDeployment, &row.Reason, &row.Attempts, &row.CreatedAt); err != nil {
			rows.Close()
			return
		}
		row.DeploymentID = platform.DeploymentID(storedDeployment)
		due = append(due, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return
	}
	for _, row := range due {
		if err := s.removeContainerCleanupRow(ctx, row); err != nil {
			_ = s.recordContainerCleanup(ctx, row.ContainerName, row.EnvironmentID, row.Node, row.DeploymentID, row.Reason, err)
			continue
		}
		if _, err := s.pool.Exec(ctx, `DELETE FROM eregion_platform_container_cleanup WHERE container_name=$1`, row.ContainerName); err != nil {
			_ = s.recordContainerCleanup(ctx, row.ContainerName, row.EnvironmentID, row.Node, row.DeploymentID, row.Reason, err)
		}
	}
}

// containerCleanupFailure is the maintenance worker's recorded retry failure.
// The message carries the node's own removal reason; the durable row stores
// the fault message, so a hint alone would lose it. Redaction runs on the raw
// detail before normalization and truncation.
func (s *Service) containerCleanupFailure(code contract.Code, detail string) error {
	if !contract.KnownCode(code) {
		code = contract.Internal
	}
	message := "container cleanup failed"
	if cause := boundedCleanupCause(s.redact(detail)); cause != "" {
		message += ": " + cause
	}
	return contract.NewFault(code, message, "The maintenance job retries with backoff; inspect the node's Docker state.")
}

// cleanupOperationID gives one cleanup row a stable node effect identity. The
// row's creation time separates a new leak of the same deterministic container
// name from an earlier row that already succeeded.
func cleanupOperationID(row containerCleanupRow) contract.OperationID {
	sum := sha256.Sum256([]byte("cleanup\x00" + row.ContainerName + "\x00" + row.CreatedAt.UTC().Format(time.RFC3339Nano)))
	return contract.OperationID("op_" + hex.EncodeToString(sum[:16]))
}

func logMaintenance(what string, err error) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	slog.WarnContext(context.Background(), what+" maintenance failed", "error", err)
}

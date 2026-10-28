package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// containerCleanupRow is one leaked container waiting for a maintenance retry.
type containerCleanupRow struct {
	ContainerName string
	EnvironmentID platform.EnvironmentID
	Node          string
	DeploymentID  platform.DeploymentID
	Reason        string
	Attempts      int
	CreatedAt     time.Time
}

const containerCleanupPageSize = 50

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

// recordContainerCleanup upserts one leaked container and schedules its next
// attempt. Success is recorded by deleting the row.
func (s *Service) recordContainerCleanup(ctx context.Context, containerName string, environmentID platform.EnvironmentID, nodeName string, deploymentID platform.DeploymentID, reason string, cause error) error {
	detail := ""
	if cause != nil {
		detail = boundedRouteError(contract.Classify(cause).Message)
	}
	var attempts int
	if err := s.pool.QueryRow(ctx, `INSERT INTO eregion_platform_container_cleanup
        (container_name,environment_id,node,deployment_id,reason,attempts,last_error,next_attempt_at)
        VALUES($1,$2,$3,$4,$5,1,$6,clock_timestamp())
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

// retryDueContainerCleanups removes due leaked containers. A success deletes
// the row; any failure is recorded again with the next backoff.
func (s *Service) retryDueContainerCleanups(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT container_name,environment_id,node,coalesce(deployment_id,''),reason,attempts,created_at
        FROM eregion_platform_container_cleanup
        WHERE next_attempt_at <= clock_timestamp()
        ORDER BY next_attempt_at,container_name LIMIT $1`, containerCleanupPageSize)
	if err != nil {
		return unavailable()
	}
	var due []containerCleanupRow
	for rows.Next() {
		var row containerCleanupRow
		var deploymentID string
		if err := rows.Scan(&row.ContainerName, &row.EnvironmentID, &row.Node, &deploymentID,
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
		code := contract.Code(response.ErrorCode)
		if code == "" {
			code = contract.Internal
		}
		return contract.NewFault(code, "container cleanup failed", response.Detail)
	default:
		return contract.NewFault(contract.Unavailable, "container cleanup did not finish", "The maintenance job will retry with backoff.")
	}
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

package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	labelManaged  = "io.eregion.managed"
	labelResource = "io.eregion.resource"
	labelHost     = "io.eregion.host"
	labelEngine   = "io.eregion.engine"
)

func ownershipLabels(spec Spec) map[string]string {
	return map[string]string{
		labelManaged: "true", labelResource: spec.Name, labelHost: spec.Host, labelEngine: string(spec.Engine),
	}
}

func (s *Service) labelsOwned(labels map[string]string, spec Spec) bool {
	for key, value := range ownershipLabels(spec) {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// containerLabels returns the labels of a container, or exists=false when Docker
// positively reports that it does not exist. Other Docker failures stay errors.
func (s *Service) containerLabels(ctx context.Context, spec Spec) (map[string]string, bool, error) {
	result, err := s.docker(ctx, "inspect container", "inspect", "--type", "container", "--format", "{{json .Config.Labels}}", containerName(spec))
	if err != nil {
		return nil, false, err
	}
	if result.ExitCode != 0 {
		if dockerNotFound(result) {
			return nil, false, nil
		}
		return nil, false, stepFailure("inspect container", result)
	}
	labels := map[string]string{}
	if len(strings.TrimSpace(string(result.Stdout))) > 0 {
		if err := json.Unmarshal(result.Stdout, &labels); err != nil {
			return nil, false, precondition("container labels are not readable", "Inspect the container with Docker; this service will not guess its owner.")
		}
	}
	return labels, true, nil
}

func (s *Service) volumeLabels(ctx context.Context, spec Spec) (map[string]string, bool, error) {
	result, err := s.docker(ctx, "inspect volume", "volume", "inspect", "--format", "{{json .Labels}}", spec.Volume)
	if err != nil {
		return nil, false, err
	}
	if result.ExitCode != 0 {
		if dockerNotFound(result) {
			return nil, false, nil
		}
		return nil, false, stepFailure("inspect volume", result)
	}
	labels := map[string]string{}
	if len(strings.TrimSpace(string(result.Stdout))) > 0 {
		if err := json.Unmarshal(result.Stdout, &labels); err != nil {
			return nil, false, precondition("volume labels are not readable", "Inspect the volume with Docker; this service will not guess its owner.")
		}
	}
	return labels, true, nil
}

func dockerNotFound(result CommandResult) bool {
	text := strings.ToLower(string(result.Stderr) + " " + string(result.Stdout))
	return strings.Contains(text, "no such")
}

func (s *Service) containerRunning(ctx context.Context, spec Spec) (bool, error) {
	result, err := s.docker(ctx, "inspect container state", "inspect", "--type", "container", "--format", "{{.State.Running}}", containerName(spec))
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, stepFailure("inspect container state", result)
	}
	return strings.TrimSpace(string(result.Stdout)) == "true", nil
}

func (s *Service) volumeCreate(ctx context.Context, spec Spec) error {
	args := []string{"volume", "create"}
	for key, value := range ownershipLabels(spec) {
		args = append(args, "--label", key+"="+value)
	}
	args = append(args, spec.Volume)
	result, err := s.docker(ctx, "create volume", args...)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		// A concurrent create is acceptable when the existing volume carries our
		// exact ownership labels. A foreign volume is never adopted.
		labels, exists, inspectErr := s.volumeLabels(ctx, spec)
		if inspectErr == nil && exists && s.labelsOwned(labels, spec) {
			return nil
		}
		return stepFailure("create volume", result)
	}
	return nil
}

func (s *Service) create(ctx context.Context, spec Spec) (Result, error) {
	result := baseResult(ActionCreate, spec, StateUnknown)
	labels, containerExists, err := s.containerLabels(ctx, spec)
	if err != nil {
		return result, err
	}
	if containerExists && !s.labelsOwned(labels, spec) {
		return result, conflict("container name belongs to a different resource", "Inspect the Docker container and use its real owner; this service will not adopt or mutate a foreign container.")
	}
	volumeLabels, volumeExists, err := s.volumeLabels(ctx, spec)
	if err != nil {
		return result, err
	}
	if volumeExists && !s.labelsOwned(volumeLabels, spec) {
		return result, conflict("volume name belongs to a different resource", "Inspect the Docker volume and use its real owner; this service will not adopt or mutate a foreign volume.")
	}

	if containerExists || volumeExists {
		if _, err := s.loadCredential(spec); err != nil {
			return result, err
		}
	} else {
		if _, err := s.ensureCredential(spec); err != nil {
			return result, err
		}
	}
	if err := s.ensureStoredSpec(spec); err != nil {
		return result, err
	}

	if !volumeExists {
		if err := s.volumeCreate(ctx, spec); err != nil {
			return result, err
		}
	}

	if !containerExists {
		runArgs := []string{
			"run", "--detach", "--name", containerName(spec),
			"--restart", "unless-stopped",
			"--volume", spec.Volume + ":/var/lib/postgresql/data",
			"--volume", s.passwordPath(spec.Name) + ":/run/eregion/postgres-password:ro",
			"--env", "POSTGRES_USER=" + spec.Username,
			"--env", "POSTGRES_DB=" + spec.Database,
			"--env", "POSTGRES_PASSWORD_FILE=/run/eregion/postgres-password",
		}
		for key, value := range ownershipLabels(spec) {
			runArgs = append(runArgs, "--label", key+"="+value)
		}
		if spec.Port == 0 {
			runArgs = append(runArgs, "--publish", spec.Address+"::5432")
		} else {
			runArgs = append(runArgs, "--publish", fmt.Sprintf("%s:%d:5432", spec.Address, spec.Port))
		}
		runArgs = append(runArgs, s.images[spec.Version])
		created, err := s.docker(ctx, "create container", runArgs...)
		if err != nil {
			result.State = StateDegraded
			return result, err
		}
		if created.ExitCode != 0 {
			result.State = StateDegraded
			return result, stepFailure("create container", created)
		}
		result.Changed = true
	} else {
		running, err := s.containerRunning(ctx, spec)
		if err != nil {
			return result, err
		}
		if !running {
			started, err := s.docker(ctx, "start container", "start", containerName(spec))
			if err != nil {
				result.State = StateStopped
				return result, err
			}
			if started.ExitCode != 0 {
				result.State = StateStopped
				return result, stepFailure("start container", started)
			}
			result.Changed = true
		}
	}

	ready, detail := s.waitReady(ctx, spec)
	endpoint, err := s.endpoint(ctx, spec)
	if err != nil {
		return result, err
	}
	result.Endpoint = endpoint
	result.Credential = s.credentialReference(spec)
	result.Health = &Health{Status: HealthStarting, Endpoint: endpoint, Detail: detail, CheckedAt: s.now()}
	if !ready {
		result.State = StateDegraded
		if detail == "" {
			detail = "pg_isready did not succeed before the start deadline"
		}
		result.Health.Status = HealthUnhealthy
		return result, unavailable("database did not become ready", "Inspect the container logs and the protected credential mount; the container and volume were left in place for recovery.")
	}
	result.State = StateReady
	result.Health.Status = HealthHealthy
	result.Health.Detail = "pg_isready accepted connections"
	return result, nil
}

func (s *Service) waitReady(ctx context.Context, spec Spec) (bool, string) {
	deadline := s.now().Add(s.config.StartTimeout)
	var detail string
	for {
		running, err := s.containerRunning(ctx, spec)
		if err != nil {
			return false, "container state could not be inspected"
		}
		if !running {
			return false, "container is not running"
		}
		probe, err := s.containerExec(ctx, "pg_isready", spec, "pg_isready", "--host=127.0.0.1", "--username="+spec.Username, "--dbname="+spec.Database, "--timeout=3")
		if err == nil && probe.ExitCode == 0 {
			return true, "pg_isready accepted connections"
		}
		if err == nil {
			detail = sanitizeDetail(probe.Stderr)
			if detail == "" {
				detail = sanitizeDetail(probe.Stdout)
			}
		} else if contract.Classify(err).Code == contract.Cancelled {
			return false, "startup was interrupted"
		}
		if !s.now().Before(deadline) {
			return false, detail
		}
		timer := time.NewTimer(s.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, "startup was interrupted"
		case <-timer.C:
		}
	}
}

func (s *Service) health(ctx context.Context, spec Spec) (Result, error) {
	result := baseResult(ActionHealth, spec, StateUnknown)
	labels, exists, err := s.containerLabels(ctx, spec)
	if err != nil {
		return result, err
	}
	if !exists {
		result.State = StateAbsent
		result.Health = &Health{Status: HealthAbsent, Detail: "container does not exist", CheckedAt: s.now()}
		return result, nil
	}
	if !s.labelsOwned(labels, spec) {
		return result, conflict("container name belongs to a different resource", "Inspect the Docker container and use its real owner; this service will not probe or mutate a foreign container.")
	}
	running, err := s.containerRunning(ctx, spec)
	if err != nil {
		return result, err
	}
	endpoint, err := s.endpoint(ctx, spec)
	if err != nil {
		return result, err
	}
	result.Endpoint = endpoint
	result.Credential = s.credentialReference(spec)
	if !running {
		result.State = StateStopped
		result.Health = &Health{Status: HealthStopped, Endpoint: endpoint, Detail: "container is not running", CheckedAt: s.now()}
		return result, nil
	}
	probe, err := s.containerExec(ctx, "pg_isready", spec, "pg_isready", "--host=127.0.0.1", "--username="+spec.Username, "--dbname="+spec.Database, "--timeout=3")
	if err != nil {
		return result, err
	}
	health := &Health{Status: HealthUnhealthy, Endpoint: endpoint, CheckedAt: s.now()}
	if probe.ExitCode == 0 {
		health.Status = HealthHealthy
		health.Detail = "pg_isready accepted connections"
		result.State = StateReady
	} else {
		health.Detail = sanitizeDetail(probe.Stderr)
		if health.Detail == "" {
			health.Detail = sanitizeDetail(probe.Stdout)
		}
		if health.Detail == "" {
			health.Detail = "pg_isready rejected connections"
		}
		result.State = StateDegraded
	}
	result.Health = health
	return result, nil
}

func (s *Service) destroy(ctx context.Context, spec Spec, request *DestroyRequest) (Result, error) {
	result := baseResult(ActionDestroy, spec, StateUnknown)
	labels, containerExists, err := s.containerLabels(ctx, spec)
	if err != nil {
		return result, err
	}
	if containerExists && !s.labelsOwned(labels, spec) {
		return result, conflict("container name belongs to a different resource", "Inspect the Docker container and use its real owner; this service will not delete a foreign container.")
	}
	volumeLabels, volumeExists, err := s.volumeLabels(ctx, spec)
	if err != nil {
		return result, err
	}
	if volumeExists && !s.labelsOwned(volumeLabels, spec) {
		return result, conflict("volume name belongs to a different resource", "Inspect the Docker volume and use its real owner; this service will not delete a foreign volume.")
	}

	if containerExists {
		removed, err := s.docker(ctx, "remove container", "rm", "--force", containerName(spec))
		if err != nil {
			result.State = StateDegraded
			return result, err
		}
		if removed.ExitCode != 0 {
			result.State = StateDegraded
			return result, stepFailure("remove container", removed)
		}
		result.Changed = true
	}

	if request.RemoveVolume && volumeExists {
		removed, err := s.docker(ctx, "remove volume", "volume", "rm", spec.Volume)
		if err != nil {
			result.State = StateAbsent
			return result, err
		}
		if removed.ExitCode != 0 {
			result.State = StateAbsent
			fault := stepFailure("remove volume", removed)
			fault.Details["container_removed"] = containerExists
			return result, fault
		}
		result.Changed = true
		volumeExists = false
	}
	if request.RemoveVolume {
		if err := s.removeCredential(spec); err != nil {
			result.Warnings = append(result.Warnings, "container and volume were removed but protected credential files could not be removed")
		}
	} else if volumeExists {
		result.Warnings = append(result.Warnings, "named volume and protected credentials were retained; data is not deleted")
	}
	result.State = StateAbsent
	return result, nil
}

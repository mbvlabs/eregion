package resources

import (
	"context"
	"fmt"
	"net"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

var (
	resourceNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,39}$`)
	hostPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	databasePattern     = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,31}$`)
	versionPattern      = regexp.MustCompile(`^[0-9]{2}$`)
	volumePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
)

func invalidResource(message, hint string) *contract.Fault {
	return contract.NewFault(contract.ValidationFailed, message, hint)
}

func precondition(message, hint string) *contract.Fault {
	return contract.NewFault(contract.PreconditionFailed, message, hint)
}

func unavailable(message, hint string) *contract.Fault {
	return contract.NewFault(contract.Unavailable, message, hint)
}

func conflict(message, hint string) *contract.Fault {
	return contract.NewFault(contract.Conflict, message, hint)
}

func notFound(message, hint string) *contract.Fault {
	return contract.NewFault(contract.NotFound, message, hint)
}

// ValidateSpec checks the caller-supplied shape without applying service
// defaults. The Service additionally enforces its host and image allow-list.
func ValidateSpec(spec Spec) error {
	if spec.Engine != EnginePostgreSQL {
		return invalidResource("unsupported resource engine", "PostgreSQL is the only implemented application resource engine.")
	}
	if !resourceNamePattern.MatchString(spec.Name) {
		return invalidResource("invalid resource name", "Use 2-40 lowercase letters, digits, underscores, or hyphens, starting with a letter.")
	}
	if strings.TrimSpace(spec.Host) == "" {
		return invalidResource("explicit application host is required", "Set the resource host to the configured application host; placement is never inferred.")
	}
	if !hostPattern.MatchString(spec.Host) {
		return invalidResource("invalid application host", "Use a bounded host identifier of letters, digits, dots, underscores, or hyphens.")
	}
	if !versionPattern.MatchString(spec.Version) {
		return invalidResource("invalid PostgreSQL version", "Use a supported two-digit major version such as 17.")
	}
	if spec.Address != "" {
		ip := net.ParseIP(spec.Address)
		if ip == nil {
			return invalidResource("invalid endpoint address", "Use a literal IP address for the published database port.")
		}
		if ip.IsUnspecified() {
			return invalidResource("refusing an unspecified endpoint address", "Bind to a loopback or private address; do not publish the database on 0.0.0.0 or ::.")
		}
	}
	if spec.Port < 0 || spec.Port > 65535 {
		return invalidResource("invalid endpoint port", "Use 0 for an ephemeral port or a port from 1 to 65535.")
	}
	if !databasePattern.MatchString(spec.Username) {
		return invalidResource("invalid database username", "Use 1-32 lowercase letters, digits, or underscores, starting with a letter or underscore.")
	}
	if !databasePattern.MatchString(spec.Database) {
		return invalidResource("invalid database name", "Use 1-32 lowercase letters, digits, or underscores, starting with a letter or underscore.")
	}
	if spec.Volume != "" && (!volumePattern.MatchString(spec.Volume) || strings.Contains(spec.Volume, "..")) {
		return invalidResource("invalid volume name", "Use a bounded Docker volume name of letters, digits, dots, underscores, or hyphens.")
	}
	switch spec.Protection {
	case "", ProtectionApplication, ProtectionControlPlane:
	default:
		return invalidResource("invalid resource protection", "Use application or control-plane.")
	}
	return nil
}

// normalizeSpec applies only documented defaults and validates the result
// against this service's host and configured image set. It never infers a host.
func (s *Service) normalizeSpec(spec Spec) (Spec, error) {
	if err := ValidateSpec(spec); err != nil {
		return spec, err
	}
	if spec.Protection == "" {
		spec.Protection = ProtectionApplication
	}
	if spec.Host != s.config.Host {
		return spec, invalidResource("resource host does not match this service host", "Plan and execute on the service configured for the resource's application host.")
	}
	if _, ok := s.images[spec.Version]; !ok {
		return spec, invalidResource("unsupported PostgreSQL version", "Use a version explicitly configured for this service.")
	}
	if spec.Address == "" {
		spec.Address = "127.0.0.1"
	}
	if ip := net.ParseIP(spec.Address); ip == nil || ip.IsUnspecified() {
		return spec, invalidResource("invalid endpoint address", "Use a literal, specified IP address for the published database port.")
	}
	if spec.Volume == "" {
		spec.Volume = "eregion-pg-" + spec.Name + "-data"
	}
	return spec, nil
}

func containerName(spec Spec) string { return "eregion-pg-" + spec.Name }

func validateRequest(req Request) error {
	if !req.Action.Valid() {
		return invalidResource("unsupported resource action", "Use create, health, destroy, backup, verify, restore, recover, credentials, or schedule.")
	}
	switch req.Action {
	case ActionVerify:
		if req.Verify == nil || strings.TrimSpace(req.Verify.Archive) == "" {
			return invalidResource("backup archive is required", "Name an archive in the configured backup directory.")
		}
	case ActionDestroy:
		if req.Destroy == nil {
			return invalidResource("destroy options are required", "Provide the exact confirmation and whether the named volume may be removed.")
		}
	case ActionRestore:
		if req.Restore == nil || strings.TrimSpace(req.Restore.Archive) == "" || strings.TrimSpace(req.Restore.Confirmation) == "" {
			return invalidResource("restore requires an archive and confirmation", "Provide a backup archive and the exact resource name as confirmation.")
		}
	case ActionCredentials:
		if req.Export == nil {
			return invalidResource("credential export destination is required", "Name one new absolute destination file; credentials are never printed.")
		}
	case ActionSchedule:
		if req.Policy == nil {
			return invalidResource("backup policy is required", "Provide a schedule and at least one retention bound.")
		}
		if err := req.Policy.Validate(); err != nil {
			return err
		}
	case ActionBackup:
		if req.Backup != nil && req.Backup.Policy != nil {
			if err := req.Backup.Policy.Validate(); err != nil {
				return err
			}
		}
	}
	if req.Restore != nil && req.Restore.Policy != nil {
		if err := req.Restore.Policy.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func confirmed(confirmation, name string) bool {
	return confirmation == name
}

func confirmationFault(name string) error {
	return contract.NewFault(contract.ConfirmationRequired, "exact resource-name confirmation is required", fmt.Sprintf("Repeat the operation with confirmation %q.", name))
}

// guardControlPlane is the code boundary that keeps the external control-plane
// database outside ordinary application-resource lifecycle and backup actions.
func (s *Service) guardControlPlane(spec Spec, action Action) error {
	protected := spec.Protection == ProtectionControlPlane ||
		(s.config.ControlPlaneResource != "" && spec.Name == s.config.ControlPlaneResource) ||
		(s.config.ControlPlaneDatabase != "" && spec.Database == s.config.ControlPlaneDatabase)
	if !protected {
		return nil
	}
	switch action {
	case ActionHealth, ActionVerify:
		// Read-only observation does not destroy or replace the database.
		return nil
	}
	return precondition("control-plane database is protected from application-resource operations",
		"Use the separate control-plane database recovery procedure; ordinary resource lifecycle and backup actions cannot touch it.")
}

func sanitizeDetail(raw []byte) string {
	text := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(raw))
	text = strings.TrimSpace(text)
	if len(text) > 400 {
		text = text[:400] + "…"
	}
	return text
}

func stepFailure(step string, result CommandResult) *contract.Fault {
	detail := sanitizeDetail(result.Stderr)
	if detail == "" {
		detail = sanitizeDetail(result.Stdout)
	}
	fault := unavailable(step+" failed", "Inspect the database container and its Docker/PostgreSQL state, then retry.")
	fault.Details = map[string]any{"step": step, "exit_code": result.ExitCode}
	if detail != "" {
		fault.Details["detail"] = detail
	}
	return fault
}

func contextFailure(step string, err error) error {
	if err == nil {
		return unavailable(step+" did not complete", "Inspect Docker and the database container before retrying.")
	}
	if err == context.Canceled {
		return contract.NewFault(contract.Cancelled, step+" was interrupted", "Inspect the resource before retrying.")
	}
	fault := unavailable(step+" did not complete", "Check that the Docker daemon and database container are reachable, then retry.")
	fault.Cause = err
	return fault
}

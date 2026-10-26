package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

func secretMeta(summary, example string) CommandMeta {
	meta := readMeta(summary,
		[]string{summary + ". Runs on the node through the active context; the operator is the SSH-authenticated admin, never the payload."},
		[]string{example}, "eregion secret list --app <app> --environment <env> --json")
	meta.Remote = true
	meta.Environment = []string{"EREGION_CONTEXT_FILE"}
	meta.ErrorCodes = []contract.Code{
		contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.Conflict,
		contract.PreconditionFailed, contract.NodeVersionMismatch, contract.Unavailable, contract.Internal, contract.Cancelled,
	}
	return meta
}

// secretSpecs is the addon surface merged by the CLI owner. It deliberately uses
// only the authenticated API client; the CLI never has a database or master key.
func (a *app) secretSpecs() []spec {
	setMeta := secretMeta("Create or rotate one encrypted secret from a protected file", "eregion secret set --app api --environment production --name DATABASE_URL --file /run/secrets/database-url --expected-revision 0 --json")
	setMeta.Mutating = true
	setMeta.InputFlags = []string{"file"}
	setMeta.DryRunReason = "Secret plaintext must never be sent to a preview endpoint; read metadata and use the revision CAS guard instead."

	archiveMeta := secretMeta("Archive one secret namespace under a revision CAS guard", "eregion secret archive --app api --environment production --name DATABASE_URL --expected-revision 3 --json")
	archiveMeta.Mutating, archiveMeta.Destructive = true, true
	archiveMeta.Confirmation = "plan-bound"
	archiveMeta.DryRunReason = "Archival is a CAS-guarded state transition whose current revision is read from metadata."

	exportMeta := secretMeta("Write one secret value to an explicitly protected destination file", "eregion secret export --app api --environment production --name DATABASE_URL --dest /run/secrets/database-url --json")
	exportMeta.Mutating = true
	exportMeta.InputFlags = []string{"dest"}
	exportMeta.DryRunReason = "Export writes only to the explicitly named destination; values are never printed, so there is no preview output."

	parentMeta := secretMeta("Manage revisioned encrypted secrets", "eregion secret list --app api --environment production --json")

	return []spec{{
		use:  "secret",
		meta: parentMeta,
		flags: func(cmd *cobra.Command) {
		},
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "set", meta: setMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("app", "", "Application name")
				cmd.Flags().String("environment", "", "Environment name")
				cmd.Flags().String("name", "", "Secret name (environment-variable form)")
				cmd.Flags().String("file", "", "Protected 0600 file containing the plaintext value")
				cmd.Flags().Uint64("expected-revision", 0, "CAS guard: 0 creates, otherwise the current latest revision")
				cmd.Flags().String("idempotency-key", "", "Stable key for attaching the name to the environment; one is generated when empty")
			}, run: a.setSecret},
			{use: "list", meta: secretMeta("List secret metadata for one environment", "eregion secret list --app api --environment production --json"), flags: func(cmd *cobra.Command) {
				cmd.Flags().String("app", "", "Application name")
				cmd.Flags().String("environment", "", "Environment name")
				cmd.Flags().Int("limit", 100, "Maximum metadata rows (1–200)")
			}, run: a.listSecrets},
			{use: "archive", meta: archiveMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("app", "", "Application name")
				cmd.Flags().String("environment", "", "Environment name")
				cmd.Flags().String("name", "", "Secret name")
				cmd.Flags().Uint64("expected-revision", 0, "Current latest revision (required CAS guard)")
				cmd.Flags().String("confirm", "", "Exact archive:<app>/<environment>/<name>:<revision> confirmation")
			}, run: a.archiveSecret},
			{use: "export", meta: exportMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("app", "", "Application name")
				cmd.Flags().String("environment", "", "Environment name")
				cmd.Flags().String("name", "", "Secret name")
				cmd.Flags().String("dest", "", "Explicit new destination file; created 0600 and never overwritten")
			}, run: a.exportSecret},
		},
	}}
}

func secretNamespace(cmd *cobra.Command) (vault.Namespace, error) {
	app, _ := cmd.Flags().GetString("app")
	environment, _ := cmd.Flags().GetString("environment")
	name, _ := cmd.Flags().GetString("name")
	if app == "" || environment == "" || name == "" {
		return vault.Namespace{}, contract.NewFault(contract.InputRequired, "--app, --environment, and --name are required", "Use the canonical app/environment/name secret namespace.")
	}
	return vault.NewNamespace(app, environment, name)
}

func (a *app) setSecret(cmd *cobra.Command, _ []string) (result, error) {
	if err := validateIdempotencyFlag(cmd); err != nil {
		return result{}, err
	}
	namespace, err := secretNamespace(cmd)
	if err != nil {
		return result{}, err
	}
	path, _ := cmd.Flags().GetString("file")
	if path == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--file is required", "Write the plaintext to a 0600 regular file outside shell history, then reference that absolute path.")
	}
	expected, _ := cmd.Flags().GetUint64("expected-revision")
	value, err := readProtectedSecretFile(path)
	if err != nil {
		return result{}, err
	}
	defer clearSecretBytes(value)
	a.secrets.Add(string(value))
	env, _, err := a.readEnvironment(cmd, namespace.App, namespace.Environment)
	if err != nil {
		return result{}, err
	}
	attached := env.Revision != nil && slices.Contains(env.Revision.SecretNames, namespace.Name)
	target := ""
	attachInput := platform.EnvironmentSecretAttachInput{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	}
	if !attached {
		envTarget, err := platform.NewEnvironmentTarget(namespace.App, namespace.Environment)
		if err != nil {
			return result{}, err
		}
		target = envTarget.String()
		// Validate the attach action before the vault write, so a rejected
		// submission cannot leave an unreferenced secret revision behind.
		if _, err := a.previewAction(cmd, platform.KindEnvironmentSecretAttach, target, attachInput); err != nil {
			return result{}, err
		}
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	metadata, err := client.SetSecret(cmd.Context(), namespace, value, expected)
	if err != nil {
		return result{}, err
	}
	data := map[string]any{"secret": metadata, "attached": true, "revision_updated": false}
	plain := formatSecretMetadata(metadata) + "\n"
	if attached {
		plain += namespace.Name + " is already in the environment revision.\n"
		return result{data: data, plain: plain}, nil
	}
	op, err := a.submitAccepted(cmd, platform.KindEnvironmentSecretAttach, target, attachInput)
	if err != nil {
		fault := contract.Classify(err)
		return result{}, contract.NewFault(fault.Code,
			"the secret value was stored, but attaching "+namespace.Name+" failed: "+fault.Message,
			"Check secret metadata, then re-run eregion secret set with the current --expected-revision.")
	}
	data["revision_updated"] = true
	data["operation_id"] = op.ID
	plain += "Attached " + namespace.Name + " to " + namespace.App + "/" + namespace.Environment + " in a new revision.\noperation: " + string(op.ID) + "\n"
	return result{data: data, plain: plain}, nil
}

func (a *app) listSecrets(cmd *cobra.Command, _ []string) (result, error) {
	appName, _ := cmd.Flags().GetString("app")
	environment, _ := cmd.Flags().GetString("environment")
	if appName == "" || environment == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--app and --environment are required", "List one environment's secret metadata.")
	}
	if err := vault.ValidateApp(appName); err != nil {
		return result{}, err
	}
	if err := vault.ValidateEnvironment(environment); err != nil {
		return result{}, err
	}
	limit, _ := cmd.Flags().GetInt("limit")
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	metadata, err := client.ListSecrets(cmd.Context(), appName, environment, limit)
	if err != nil {
		return result{}, err
	}
	plain := fmt.Sprintf("Secrets for %s/%s (%d shown)\n", appName, environment, len(metadata))
	for _, item := range metadata {
		plain += "  " + formatSecretMetadata(item) + "\n"
	}
	return result{data: map[string]any{"secrets": metadata}, plain: plain}, nil
}

func (a *app) archiveSecret(cmd *cobra.Command, _ []string) (result, error) {
	namespace, err := secretNamespace(cmd)
	if err != nil {
		return result{}, err
	}
	expected, _ := cmd.Flags().GetUint64("expected-revision")
	if expected == 0 {
		return result{}, contract.NewFault(contract.InputRequired, "--expected-revision is required", "Read secret metadata and pass its latest revision as the CAS guard.")
	}
	confirmation, _ := cmd.Flags().GetString("confirm")
	if confirmation != fmt.Sprintf("archive:%s:%d", namespace.Key(), expected) {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "secret archival requires exact namespace and revision confirmation", fmt.Sprintf("Pass --confirm archive:%s:%d after reviewing secret metadata.", namespace.Key(), expected))
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	metadata, err := client.ArchiveSecret(cmd.Context(), namespace, expected)
	if err != nil {
		return result{}, err
	}
	return result{
		data:  metadata,
		plain: formatSecretMetadata(metadata) + "\n",
	}, nil
}

func (a *app) exportSecret(cmd *cobra.Command, _ []string) (result, error) {
	namespace, err := secretNamespace(cmd)
	if err != nil {
		return result{}, err
	}
	dest, _ := cmd.Flags().GetString("dest")
	if dest == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--dest is required", "Name an explicit new file; values are never printed to stdout.")
	}
	if err := validateExportDestination(dest); err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	metadata, value, err := client.ExportSecret(cmd.Context(), namespace)
	if err != nil {
		return result{}, err
	}
	defer clearSecretBytes(value)
	a.secrets.Add(string(value))
	if err := writeProtectedSecretFile(dest, value); err != nil {
		return result{}, err
	}
	return result{
		data:  map[string]any{"secret": metadata, "destination": dest},
		plain: fmt.Sprintf("Exported %s to %s (mode 0600)\n%s\n", namespace.Key(), dest, formatSecretMetadata(metadata)),
	}, nil
}

func formatSecretMetadata(metadata vault.Metadata) string {
	state := string(metadata.State)
	if metadata.ArchivedAt != nil {
		state = string(vault.StateArchived)
	}
	return fmt.Sprintf("%s/%s/%s revision=%d state=%s key=%s hash=%s",
		metadata.App, metadata.Environment, metadata.Name, metadata.LatestRevision,
		state, metadata.Revision.KeyID, metadata.Revision.Hash)
}

func readProtectedSecretFile(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return nil, contract.NewFault(contract.ValidationFailed, "--file must be an absolute protected path", "Use a regular 0600 file outside shell history.")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret input file is unavailable or not a regular file", "Use a real regular file; symlinks and devices are rejected.")
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret input file permissions are unsafe", "Set mode 0600 (or stricter) before reading the value.")
	}
	if info.Size() <= 0 || info.Size() > vault.MaxValueBytes {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret input file size is outside the supported bound", "Use a nonempty value no larger than 32768 bytes.")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret input file could not be opened", "Check ownership and permissions.")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "secret input file changed while it was opened", "Replace the file atomically and retry.")
	}
	value, err := io.ReadAll(io.LimitReader(file, vault.MaxValueBytes+1))
	if err != nil || len(value) > vault.MaxValueBytes {
		clearSecretBytes(value)
		return nil, contract.NewFault(contract.PreconditionFailed, "secret input file could not be read within its bound", "Check the file size and permissions.")
	}
	if err := vault.ValidateValue(value); err != nil {
		clearSecretBytes(value)
		return nil, err
	}
	return value, nil
}

func validateExportDestination(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return contract.NewFault(contract.ValidationFailed, "--dest must be an absolute protected path", "Name a new file in a protected directory; values are never printed.")
	}
	if _, err := os.Lstat(path); err == nil {
		return contract.NewFault(contract.PreconditionFailed, "export destination already exists", "Choose a new path; secrets are never overwritten implicitly.")
	} else if !os.IsNotExist(err) {
		return contract.NewFault(contract.PreconditionFailed, "export destination is unavailable", "Check the parent directory and permissions.")
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil || !info.IsDir() {
		return contract.NewFault(contract.PreconditionFailed, "export destination directory is unavailable", "Create the protected directory first.")
	}
	return nil
}

func writeProtectedSecretFile(path string, value []byte) error {
	if err := validateExportDestination(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return contract.NewFault(contract.PreconditionFailed, "export destination could not be created", "Choose a new path in a protected directory; existing files are never overwritten.")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		return contract.NewFault(contract.Unavailable, "secret export could not be written", "Check free space and directory permissions.")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return contract.NewFault(contract.Unavailable, "secret export could not be synced", "Check the filesystem.")
	}
	if err := file.Close(); err != nil {
		return contract.NewFault(contract.Unavailable, "secret export could not be closed", "Check the filesystem.")
	}
	cleanup = false
	return nil
}

func clearSecretBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

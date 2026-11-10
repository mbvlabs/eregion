package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/spf13/cobra"
)

const metricsCredentialFile = "metrics-credential.json"

func metricsCredentialPath(dir string) (string, error) {
	return filepath.Join(dir, metricsCredentialFile), nil
}

func loadMetricsCredential(dir string) (*telemetry.Credential, error) {
	path, err := metricsCredentialPath(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "the local telemetry credential is not an owner-only regular file", "Remove or repair "+path+" before configuring scrapers.")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: eregion's own owner-only credential file.
	if err != nil {
		return nil, err
	}
	var credential telemetry.Credential
	if err := json.Unmarshal(raw, &credential); err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the local telemetry credential does not parse", "Run eregion metrics rotate to replace it.")
	}
	if err := credential.Validate(); err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the local telemetry credential is invalid", err.Error())
	}
	return &credential, nil
}

func saveMetricsCredential(dir string, credential telemetry.Credential) error {
	if err := credential.Validate(); err != nil {
		return err
	}
	path, err := metricsCredentialPath(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(path, append(raw, '\n'), 0o600)
}

func removeMetricsCredential(dir string) error {
	path, err := metricsCredentialPath(dir)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// metricsRotationClient is the node-mode boundary needed for rotation.
type metricsRotationClient interface {
	MetricsRotate(context.Context, string, string, bool) (telemetry.Rotation, error)
}

type metricsEndpointClient interface {
	MetricsEndpoint(context.Context) (telemetry.EndpointInfo, error)
}

// rotateMetricsCredential generates or reuses credential, saves it locally
// before the node request (a node holding a hash of a password this machine
// lost would lock every scraper out), and restores the previous local value if
// the request fails, as vaire does. Only the bcrypt hash is sent.
func rotateMetricsCredential(ctx context.Context, client metricsRotationClient, credentialDir, nodeName, confirm string, credential *telemetry.Credential, registerSecret func(string)) (telemetry.Rotation, error) {
	token := nodeName + "/metrics"
	if strings.TrimSpace(confirm) != token {
		fault := contract.NewFault(contract.ConfirmationRequired,
			"rotating the telemetry credential locks every scraper out until it gets the new password",
			"Pass --confirm "+token+".")
		fault.Details = map[string]any{"confirm_token": token}
		return telemetry.Rotation{}, fault
	}
	if credential == nil {
		fresh, err := telemetry.NewCredential()
		if err != nil {
			return telemetry.Rotation{}, err
		}
		credential = &fresh
	}
	hash, err := telemetry.HashPassword(credential.Password)
	if err != nil {
		return telemetry.Rotation{}, err
	}
	previous, err := loadMetricsCredential(credentialDir)
	if err != nil {
		return telemetry.Rotation{}, err
	}
	if err := saveMetricsCredential(credentialDir, *credential); err != nil {
		return telemetry.Rotation{}, err
	}
	if registerSecret != nil {
		registerSecret(credential.Password)
	}
	rotation, err := client.MetricsRotate(ctx, hash, token, false)
	if err != nil {
		if previous != nil {
			_ = saveMetricsCredential(credentialDir, *previous)
		} else {
			_ = removeMetricsCredential(credentialDir)
		}
		return telemetry.Rotation{}, err
	}
	return rotation, nil
}

type metricsEndpointView struct {
	telemetry.EndpointInfo
	LocalCredential bool `json:"local_credential"`
}

func (a *app) metricsEndpoint(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	backend, ok := client.(metricsEndpointClient)
	if !ok {
		return result{}, contract.NewFault(contract.Internal, "this client cannot read the telemetry endpoint", "Use a saved workspace over SSH inside WireGuard.")
	}
	current, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		return result{}, err
	}
	info, err := backend.MetricsEndpoint(cmd.Context())
	if err != nil {
		return result{}, err
	}
	credentialDir, err := workspaceCredentialDir(cmd, current)
	if err != nil {
		return result{}, err
	}
	local, err := loadMetricsCredential(credentialDir)
	if err != nil {
		return result{}, err
	}
	view := metricsEndpointView{EndpointInfo: info, LocalCredential: local != nil}
	return result{data: view, plain: metricsEndpointText(view)}, nil
}

func metricsEndpointText(view metricsEndpointView) string {
	info := view.EndpointInfo
	var out strings.Builder
	out.WriteString(ui.Header("Telemetry endpoint "+info.Node, 72))
	field := func(name, value string) {
		fmt.Fprintf(&out, "\n  %s %s", ui.Dim(fmt.Sprintf("%-12s", name)), value)
	}
	field("listen", info.Listen)
	field("metrics", info.MetricsURL)
	field("discovery", info.DiscoveryURL)
	field("access", "WireGuard only · basic authentication · no public listener")
	if info.Credential {
		field("credential", "configured on the node")
	} else {
		field("credential", "not configured · run eregion metrics rotate")
	}
	if view.LocalCredential {
		field("local", "present (owner-only)")
	} else {
		field("local", "missing · import the protected handoff or run eregion metrics rotate")
	}
	field("upstream", info.Upstream)
	out.WriteString("\n\n")
	out.WriteString(ui.Note("Prometheus or Grafana Alloy must be a WireGuard peer. There is no central telemetry server, metric store or history on the node."))
	return out.String()
}

func (a *app) metricsConfig(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	backend, ok := client.(metricsEndpointClient)
	if !ok {
		return result{}, contract.NewFault(contract.Internal, "this client cannot read the telemetry endpoint", "Use a saved workspace over SSH inside WireGuard.")
	}
	current, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		return result{}, err
	}
	info, err := backend.MetricsEndpoint(cmd.Context())
	if err != nil {
		return result{}, err
	}
	credentialDir, err := workspaceCredentialDir(cmd, current)
	if err != nil {
		return result{}, err
	}
	credential, err := loadMetricsCredential(credentialDir)
	if err != nil {
		return result{}, err
	}
	if credential != nil {
		a.secrets.Add(credential.Password)
	}
	format, _ := cmd.Flags().GetString("format")
	outPath, _ := cmd.Flags().GetString("out")
	switch format {
	case "all", "prometheus", "alloy", "targets":
	default:
		return result{}, contract.NewFault(contract.Usage, "unknown telemetry config format "+format, "Use all, prometheus, alloy or targets.")
	}
	if outPath != "" && format == "all" {
		return result{}, contract.NewFault(contract.Usage, "--out needs one --format value", "Choose prometheus, alloy or targets; all three cannot share one file.")
	}
	if outPath != "" && format != "targets" && credential == nil {
		return result{}, contract.NewFault(contract.PreconditionFailed,
			"this machine has no telemetry credential to write",
			"Import the protected handoff with eregion login, or run eregion metrics rotate.")
	}
	address, err := nodeAddressFromListen(info.Listen)
	if err != nil {
		return result{}, err
	}
	password := telemetry.PasswordPlaceholder
	if outPath != "" && credential != nil {
		password = credential.Password
	}
	targets, err := telemetry.TargetsJSON(info.Node, address)
	if err != nil {
		return result{}, err
	}
	prometheus := telemetry.PrometheusConfig(info.Node, address, telemetry.Username, password)
	alloy := telemetry.AlloyConfig(info.Node, address, telemetry.Username, password)
	data := map[string]any{
		"node": info.Node, "listen": info.Listen, "metrics_url": info.MetricsURL,
		"discovery_url": info.DiscoveryURL, "username": telemetry.Username,
		"credential_available": credential != nil, "node_credential": info.Credential,
		"password_in_output": false,
		"prometheus_config":  telemetry.PrometheusConfig(info.Node, address, telemetry.Username, telemetry.PasswordPlaceholder),
		"alloy_config":       telemetry.AlloyConfig(info.Node, address, telemetry.Username, telemetry.PasswordPlaceholder),
		"targets_json":       string(targets),
	}
	var warnings []map[string]string
	switch {
	case credential == nil:
		warnings = append(warnings, map[string]string{"code": "metrics_credential_missing", "message": "this machine has no telemetry credential; the snippets use a placeholder. Import the protected handoff or run eregion metrics rotate."})
	case !info.Credential:
		warnings = append(warnings, map[string]string{"code": "metrics_endpoint_unconfigured", "message": "the node has no telemetry endpoint yet; run eregion metrics rotate before relying on the snippets."})
	}
	if len(warnings) > 0 {
		data["warnings"] = warnings
	}
	if outPath != "" {
		content := ""
		switch format {
		case "prometheus":
			content = prometheus
		case "alloy":
			content = alloy
		case "targets":
			content = string(targets)
		}
		expanded := expandHome(strings.TrimSpace(outPath))
		if _, err := os.Lstat(expanded); err == nil {
			return result{}, contract.NewFault(contract.PreconditionFailed, expanded+" already exists", "Choose a new path; the file may contain a credential.")
		} else if !os.IsNotExist(err) {
			return result{}, err
		}
		if err := engine.WriteFileAtomic(expanded, []byte(content), 0o600); err != nil {
			return result{}, contract.NewFault(contract.ValidationFailed, "could not write the telemetry config", err.Error())
		}
		data["out"] = expanded
		data["format"] = format
		plain := "Wrote " + format + " config to " + expanded + " (owner-only).\nThe password is not printed. Keep the file private and update the scraper.\n"
		return result{data: data, plain: plain}, nil
	}
	plain := metricsConfigText(format, data)
	return result{data: data, plain: plain}, nil
}

func metricsConfigText(format string, data map[string]any) string {
	var out strings.Builder
	out.WriteString(ui.Header("Telemetry scrape config", 72))
	out.WriteString("\n\n")
	add := func(title, value string) {
		if value == "" {
			return
		}
		out.WriteString(ui.Dim(title) + "\n")
		out.WriteString(value)
		if !strings.HasSuffix(value, "\n") {
			out.WriteString("\n")
		}
		out.WriteString("\n")
	}
	switch format {
	case "prometheus":
		add("Prometheus", data["prometheus_config"].(string))
	case "alloy":
		add("Grafana Alloy", data["alloy_config"].(string))
	case "targets":
		add("HTTP service discovery", data["targets_json"].(string))
	default:
		add("Prometheus", data["prometheus_config"].(string))
		add("Grafana Alloy", data["alloy_config"].(string))
		add("HTTP service discovery", data["targets_json"].(string))
	}
	out.WriteString(ui.Note("The password is a placeholder in ordinary output. Re-run with --format <prometheus|alloy|targets> --out FILE to write an owner-only file with the real credential."))
	return out.String()
}

func nodeAddressFromListen(listen string) (string, error) {
	host, _, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil || strings.TrimSpace(host) == "" {
		return "", contract.NewFault(contract.Unavailable, "the node reported an invalid telemetry listen address", "Re-run eregion apply, then retry.")
	}
	return host, nil
}

func (a *app) metricsRotate(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	backend, ok := client.(metricsRotationClient)
	if !ok {
		return result{}, contract.NewFault(contract.Internal, "this client cannot rotate the telemetry credential", "Use a saved workspace over SSH inside WireGuard.")
	}
	endpointClient, ok := client.(metricsEndpointClient)
	if !ok {
		return result{}, contract.NewFault(contract.Internal, "this client cannot read the telemetry endpoint", "Use a saved workspace over SSH inside WireGuard.")
	}
	info, err := endpointClient.MetricsEndpoint(cmd.Context())
	if err != nil {
		return result{}, err
	}
	token := info.Node + "/metrics"
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		if _, err := backend.MetricsRotate(cmd.Context(), "", token, true); err != nil {
			return result{}, err
		}
		data := map[string]any{
			"dry_run": true, "node": info.Node, "confirm_token": token,
			"would_rotate": true, "credential_in_output": false,
		}
		plain := "Dry run: would rotate the telemetry credential for " + info.Node + ".\nconfirm: " + token + "\n"
		return result{data: data, plain: plain}, nil
	}
	confirm, _ := cmd.Flags().GetString("confirm")
	current, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		return result{}, err
	}
	credentialDir, err := workspaceCredentialDir(cmd, current)
	if err != nil {
		return result{}, err
	}
	rotation, err := rotateMetricsCredential(cmd.Context(), backend, credentialDir, info.Node, confirm, nil, func(value string) { a.secrets.Add(value) })
	if err != nil {
		return result{}, err
	}
	rotation.Warnings = append(rotation.Warnings, "scrapers using the old password now fail; update them, then run eregion secrets export to refresh the protected handoff")
	data := map[string]any{
		"rotated": true, "node": info.Node, "username": rotation.Username,
		"metrics_url": rotation.MetricsURL, "discovery_url": rotation.Discovery,
		"password_in_output": false, "handoff_stale": true,
	}
	if len(rotation.Warnings) > 0 {
		data["warnings"] = rotation.Warnings
	}
	plain := "Rotated the telemetry credential for " + info.Node + ".\nThe password is not printed; update your scrapers and run eregion secrets export to refresh the protected handoff.\n"
	for _, warning := range rotation.Warnings {
		plain += "warning: " + warning + "\n"
	}
	return result{data: data, plain: plain}, nil
}

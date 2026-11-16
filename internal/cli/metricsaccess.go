package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/spf13/cobra"
)

const metricsCredentialFile = "metrics-credential.json" //nolint:gosec // G101: the credential file's name, not a credential value.

func metricsCredentialPath(dir string) string {
	return filepath.Join(dir, metricsCredentialFile)
}

func loadMetricsCredential(dir string) (*telemetry.Credential, error) {
	path := metricsCredentialPath(dir)
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
	raw, err := os.ReadFile(path)
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
	path := metricsCredentialPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(credential, "", "  ") //nolint:gosec // G117: writing the node telemetry credential is the intended operation.
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(path, append(raw, '\n'), 0o600)
}

func removeMetricsCredential(dir string) error {
	path := metricsCredentialPath(dir)
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
		if strings.TrimSpace(value) == "" {
			return
		}
		fmt.Fprintf(&out, "\n  %s %s", ui.Dim(fmt.Sprintf("%-12s", name)), value)
	}
	field("state", info.State)
	if info.Hostname != "" {
		field("hostname", info.Hostname)
	}
	field("gateway", info.Listen)
	metricsURL, discoveryURL := info.MetricsURL, info.DiscoveryURL
	if metricsURL == "" {
		metricsURL = "unconfigured: the node has no public hostname"
	}
	if discoveryURL == "" {
		discoveryURL = "unconfigured: the node has no public hostname"
	}
	field("metrics", metricsURL)
	field("discovery", discoveryURL)
	field("access", "public HTTPS · basic authentication · raw exporter private")
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
	out.WriteString(ui.Note("The URLs are the desired public HTTPS ingress paths; configured does not assert DNS, certificate issuance, route activation or reachability. Authentication is basic auth; this does not add vaire's optional nginx source-CIDR allowlist or per-IP rate limiting. There is no central telemetry server, metric store or history on the node."))
	return out.String()
}

func (a *app) metricsConfig(cmd *cobra.Command, _ []string) (result, error) {
	request, err := a.readMetricsConfigRequest(cmd)
	if err != nil {
		return result{}, err
	}
	password := telemetry.PasswordPlaceholder
	if request.outPath != "" && request.credential != nil {
		password = request.credential.Password
	}
	documents, err := buildMetricsConfigDocuments(request.info, password)
	if err != nil {
		return result{}, err
	}
	data := map[string]any{
		"node": request.info.Node, "hostname": request.info.Hostname, "state": request.info.State,
		"listen": request.info.Listen, "metrics_url": request.info.MetricsURL,
		"discovery_url": request.info.DiscoveryURL, "username": telemetry.Username,
		"credential_available": request.credential != nil, "node_credential": request.info.Credential,
		"password_in_output": false,
		"prometheus_config":  documents.prometheusPlaceholder,
		"alloy_config":       documents.alloyPlaceholder,
		"targets_json":       string(documents.targets),
	}
	if warnings := metricsConfigWarnings(request.credential, request.info); len(warnings) > 0 {
		data["warnings"] = warnings
	}
	if request.outPath != "" {
		return writeMetricsConfigFile(request, documents, data)
	}
	return result{data: data, plain: metricsConfigText(request.format, data)}, nil
}

type metricsConfigRequest struct {
	info       telemetry.EndpointInfo
	credential *telemetry.Credential
	format     string
	outPath    string
}

func (a *app) readMetricsConfigRequest(cmd *cobra.Command) (metricsConfigRequest, error) {
	client, err := a.client(cmd)
	if err != nil {
		return metricsConfigRequest{}, err
	}
	backend, ok := client.(metricsEndpointClient)
	if !ok {
		return metricsConfigRequest{}, contract.NewFault(contract.Internal, "this client cannot read the telemetry endpoint", "Use a saved workspace over SSH inside WireGuard.")
	}
	current, err := selectedWorkspaceAccess(cmd)
	if err != nil {
		return metricsConfigRequest{}, err
	}
	info, err := backend.MetricsEndpoint(cmd.Context())
	if err != nil {
		return metricsConfigRequest{}, err
	}
	credentialDir, err := workspaceCredentialDir(cmd, current)
	if err != nil {
		return metricsConfigRequest{}, err
	}
	credential, err := loadMetricsCredential(credentialDir)
	if err != nil {
		return metricsConfigRequest{}, err
	}
	if credential != nil {
		a.secrets.Add(credential.Password)
	}
	format, _ := cmd.Flags().GetString("format")
	outPath, _ := cmd.Flags().GetString("out")
	switch format {
	case "all", "prometheus", "alloy", "targets":
	default:
		return metricsConfigRequest{}, contract.NewFault(contract.Usage, "unknown telemetry config format "+format, "Use all, prometheus, alloy or targets.")
	}
	if outPath != "" && format == "all" {
		return metricsConfigRequest{}, contract.NewFault(contract.Usage, "--out needs one --format value", "Choose prometheus, alloy or targets; all three cannot share one file.")
	}
	if outPath != "" && format != "targets" && credential == nil {
		return metricsConfigRequest{}, contract.NewFault(contract.PreconditionFailed,
			"this machine has no telemetry credential to write",
			"Import the protected handoff with eregion login, or run eregion metrics rotate.")
	}
	if info.State == telemetry.EndpointStateUnconfigured || strings.TrimSpace(info.Hostname) == "" {
		return metricsConfigRequest{}, contract.NewFault(contract.PreconditionFailed,
			"the node has no public hostname, so the telemetry endpoint is unconfigured",
			"Configure the main node's public hostname and public ingress; there is no WireGuard or listener-address fallback target.")
	}
	return metricsConfigRequest{info: info, credential: credential, format: format, outPath: outPath}, nil
}

type metricsConfigDocuments struct {
	targets               []byte
	prometheus            string
	alloy                 string
	prometheusPlaceholder string
	alloyPlaceholder      string
}

func buildMetricsConfigDocuments(info telemetry.EndpointInfo, password string) (metricsConfigDocuments, error) {
	targets, err := telemetry.TargetsJSON(info.Node, info.Hostname)
	if err != nil {
		return metricsConfigDocuments{}, contract.NewFault(contract.ValidationFailed, "the node reported an invalid telemetry hostname", "Re-run eregion apply with a valid main hostname.")
	}
	prometheus, err := telemetry.PrometheusConfig(info.Node, info.Hostname, telemetry.Username, password)
	if err != nil {
		return metricsConfigDocuments{}, contract.NewFault(contract.ValidationFailed, "the node reported an invalid telemetry hostname", "Re-run eregion apply with a valid main hostname.")
	}
	alloy, err := telemetry.AlloyConfig(info.Node, info.Hostname, telemetry.Username, password)
	if err != nil {
		return metricsConfigDocuments{}, contract.NewFault(contract.ValidationFailed, "the node reported an invalid telemetry hostname", "Re-run eregion apply with a valid main hostname.")
	}
	prometheusPlaceholder, err := telemetry.PrometheusConfig(info.Node, info.Hostname, telemetry.Username, telemetry.PasswordPlaceholder)
	if err != nil {
		return metricsConfigDocuments{}, err
	}
	alloyPlaceholder, err := telemetry.AlloyConfig(info.Node, info.Hostname, telemetry.Username, telemetry.PasswordPlaceholder)
	if err != nil {
		return metricsConfigDocuments{}, err
	}
	return metricsConfigDocuments{
		targets: targets, prometheus: prometheus, alloy: alloy,
		prometheusPlaceholder: prometheusPlaceholder, alloyPlaceholder: alloyPlaceholder,
	}, nil
}

func metricsConfigWarnings(credential *telemetry.Credential, info telemetry.EndpointInfo) []map[string]string {
	var warnings []map[string]string
	switch {
	case credential == nil:
		warnings = append(warnings, map[string]string{"code": "metrics_credential_missing", "message": "this machine has no telemetry credential; the snippets use a placeholder. Import the protected handoff or run eregion metrics rotate."})
	case !info.Credential:
		warnings = append(warnings, map[string]string{"code": "metrics_endpoint_unconfigured", "message": "the node has no telemetry gateway credential yet; run eregion metrics rotate before relying on the snippets."})
	case info.State == telemetry.EndpointStateNeedsApply:
		warnings = append(warnings, map[string]string{"code": "metrics_endpoint_needs_apply", "message": "the node has a telemetry credential but its gateway file is not the current loopback render; run eregion apply before relying on the snippets."})
	}
	return warnings
}

func writeMetricsConfigFile(request metricsConfigRequest, documents metricsConfigDocuments, data map[string]any) (result, error) {
	content := ""
	switch request.format {
	case "prometheus":
		content = documents.prometheus
	case "alloy":
		content = documents.alloy
	case "targets":
		content = string(documents.targets)
	}
	expanded := expandHome(strings.TrimSpace(request.outPath))
	if _, err := os.Lstat(expanded); err == nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, expanded+" already exists", "Choose a new path; the file may contain a credential.")
	} else if !os.IsNotExist(err) {
		return result{}, err
	}
	if err := engine.WriteFileAtomic(expanded, []byte(content), 0o600); err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, "could not write the telemetry config", err.Error())
	}
	data["out"] = expanded
	data["format"] = request.format
	plain := "Wrote " + request.format + " config to " + expanded + " (owner-only).\nThe password is not printed. Keep the file private and update the scraper.\n"
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
		add("Prometheus", metricsConfigValue(data, "prometheus_config"))
	case "alloy":
		add("Grafana Alloy", metricsConfigValue(data, "alloy_config"))
	case "targets":
		add("HTTP service discovery", metricsConfigValue(data, "targets_json"))
	default:
		add("Prometheus", metricsConfigValue(data, "prometheus_config"))
		add("Grafana Alloy", metricsConfigValue(data, "alloy_config"))
		add("HTTP service discovery", metricsConfigValue(data, "targets_json"))
	}
	out.WriteString(ui.Note("The password is a placeholder in ordinary output. Re-run with --format <prometheus|alloy|targets> --out FILE to write an owner-only file with the real credential."))
	return out.String()
}

func metricsConfigValue(data map[string]any, key string) string {
	value, _ := data[key].(string)
	return value
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
		"rotated": true, "node": info.Node, "hostname": rotation.Hostname, "username": rotation.Username,
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

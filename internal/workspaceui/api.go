// Package workspaceui implements the keyboard-driven fleet, application,
// environment, resource, and backup workspaces. It only reads platform
// inventory and submits registered platform actions through the public API
// contract; it never opens SSH, touches a database, starts a queue, or cancels
// an operation when the interface is detached.
package workspaceui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/resources"
)

// API is the subset of the authenticated API client the workspace uses. The
// live CLI adapts *api.Client to this interface; tests use a pure fake.
type API interface {
	PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error)
	SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool) (json.RawMessage, error)
	Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error)
	Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error)
}

// InventoryLimit is the largest page the platform contract accepts.
const InventoryLimit = 200

// Action is one validated platform submission. Input is canonical JSON.
type Action struct {
	Kind           string
	Target         string
	Input          json.RawMessage
	IdempotencyKey string
	Title          string
	// ConfirmPhrase is nonempty when the action must be confirmed by typing an
	// exact phrase before submission (deploy replaces the serving container).
	ConfirmPhrase string
}

// Noninteractive is the generic platform submit equivalent shown for every
// action. The wrapper uses the same kind, target, input, and idempotency key as
// the interactive path; it never adds a second submission mechanism.
// PlanToken binds a typed confirmation to the exact kind, target, and input
// that the dry run previewed. It mirrors the CLI's plan-bound rollback token so
// a changed input cannot reuse an earlier approval.
func (a Action) PlanToken() string {
	input := a.Input
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(a.Kind))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(a.Target))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(input)
	return hex.EncodeToString(digest.Sum(nil))
}

func (a Action) Noninteractive() string {
	input := string(a.Input)
	if input == "" {
		input = "{}"
	}
	var command strings.Builder
	command.WriteString("umask 077 && printf '%s' " + shellQuote(input) + " > action.json\n")
	command.WriteString("eregion platform submit")
	command.WriteString(" --kind " + a.Kind)
	command.WriteString(" --target " + a.Target)
	command.WriteString(" --input-file action.json")
	command.WriteString(" --idempotency-key " + a.IdempotencyKey)
	command.WriteString(" --json")
	return command.String()
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// Preview is the secret-free dry-run plan returned before a real submission.
type Preview struct {
	Kind     string          `json:"kind"`
	Target   string          `json:"target"`
	DryRun   bool            `json:"dry_run"`
	Input    json.RawMessage `json:"input"`
	Summary  string          `json:"summary"`
	Steps    []string        `json:"steps,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

// SubmitResult is an accepted durable operation snapshot.
type SubmitResult struct {
	Operation contract.Operation
}

// DecodePreview validates a dry-run response. It refuses a response that does
// not describe the action that was previewed.
func DecodePreview(raw json.RawMessage, kind, target string) (Preview, error) {
	var preview Preview
	if err := json.Unmarshal(raw, &preview); err != nil {
		return Preview{}, invalidAPI("dry-run response is not a plan")
	}
	if !preview.DryRun || preview.Kind != kind || preview.Target != target {
		return Preview{}, invalidAPI("dry-run response does not describe the submitted action")
	}
	if len(preview.Input) == 0 {
		preview.Input = json.RawMessage(`{}`)
	}
	return preview, nil
}

// DecodeSubmit validates a real submission response.
func DecodeSubmit(raw json.RawMessage, kind, target string) (SubmitResult, error) {
	var operation contract.Operation
	if err := json.Unmarshal(raw, &operation); err != nil || operation.Validate() != nil {
		return SubmitResult{}, invalidAPI("submission response is not a valid operation")
	}
	if operation.Kind != kind || operation.Target != target {
		return SubmitResult{}, invalidAPI("submission response does not describe the submitted action")
	}
	return SubmitResult{Operation: operation}, nil
}

func invalidAPI(message string) *contract.Fault {
	return contract.NewFault(contract.Unavailable, message, "Check binary compatibility; if submission timed out, reuse the same idempotency key instead of creating a new one.")
}

// CodeOf returns the machine-readable code carried by err, if any.
func CodeOf(err error) contract.Code {
	var fault *contract.Fault
	if errors.As(err, &fault) {
		return fault.Code
	}
	return ""
}

// ErrorText returns a sanitized, bounded description for display.
func ErrorText(err error, clean func(string) string) string {
	if err == nil {
		return ""
	}
	text := clean(err.Error())
	if len(text) > 320 {
		text = text[:317] + "..."
	}
	return text
}

type tabEnvelope struct {
	Collection string          `json:"collection"`
	Items      json.RawMessage `json:"items"`
}

// decodeInventory converts one collection response into uniform rows. The
// sanitiser is applied to every string before it can reach a renderer.
func decodeInventory(tab Tab, raw json.RawMessage, clean func(string) string) ([]Item, error) {
	items, err := inventoryItems(raw)
	if err != nil {
		return nil, err
	}
	switch tab {
	case Fleet:
		return decodeNodes(items, clean)
	case Apps:
		return decodeApps(items, clean)
	case Envs:
		return decodeEnvironments(items, clean)
	case Releases:
		return decodeReleases(items, clean)
	case Deployments:
		return decodeDeployments(items, clean)
	case Resources:
		return decodeResources(items, clean)
	case Backups:
		return decodeBackups(items, clean)
	default:
		return decodeGeneric(items, clean)
	}
}

func inventoryItems(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, invalidAPI("inventory response is empty")
	}
	switch trimmed[0] {
	case '[':
		return json.RawMessage(trimmed), nil
	case '{':
		var envelope tabEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, invalidAPI("inventory response is not an object")
		}
		if len(envelope.Items) == 0 || string(envelope.Items) == "null" {
			// Some extension providers return a bare value for a collection.
			return nil, nil
		}
		if !json.Valid(envelope.Items) {
			return nil, invalidAPI("inventory items are not valid JSON")
		}
		return envelope.Items, nil
	default:
		return nil, invalidAPI("inventory response is neither an array nor an object")
	}
}

func decodeNodes(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var nodes []platform.Node
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, invalidAPI("node inventory is not a node list")
	}
	items := make([]Item, 0, len(nodes))
	for _, node := range nodes {
		title := clean(node.Name)
		status := clean(string(node.Status))
		details := []Detail{
			{Label: "name", Value: title},
			{Label: "status", Value: status},
			{Label: "roles", Value: clean(strings.Join(node.Roles, ", "))},
			{Label: "capabilities", Value: clean(strings.Join(node.Capabilities, ", "))},
			{Label: "address", Value: clean(node.Address)},
			{Label: "last seen", Value: timeText(node.LastSeenAt, clean)},
			{Label: "created", Value: node.CreatedAt.Format("2006-01-02 15:04:05Z07:00")},
			{Label: "updated", Value: node.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")},
		}
		search := joinSearch(title, status, strings.Join(node.Roles, " "), strings.Join(node.Capabilities, " "), node.Address)
		lastSeen := ""
		if node.LastSeenAt != nil {
			lastSeen = node.LastSeenAt.UTC().Format(time.RFC3339)
		}
		items = append(items, Item{Key: title, Title: title, Subtitle: clean(strings.Join(node.Roles, ",")), Status: status, Details: details, SearchText: search, Data: map[string]string{
			"name": title, "status": status, "roles": clean(strings.Join(node.Roles, ",")), "capabilities": clean(strings.Join(node.Capabilities, ",")), "address": clean(node.Address), "last_seen_at": lastSeen,
		}})
	}
	return items, nil
}

func decodeApps(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var apps []platform.App
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, invalidAPI("application inventory is not an application list")
	}
	items := make([]Item, 0, len(apps))
	for _, app := range apps {
		name := clean(app.Name)
		created := app.CreatedAt.Format("2006-01-02 15:04:05Z07:00")
		details := []Detail{
			{Label: "name", Value: name},
			{Label: "id", Value: clean(string(app.ID))},
			{Label: "created", Value: created},
			{Label: "updated", Value: app.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")},
		}
		items = append(items, Item{Key: name, Title: name, Subtitle: clean(string(app.ID)), Status: "app", Details: details, SearchText: joinSearch(name, string(app.ID)), Data: map[string]string{"name": name, "id": clean(string(app.ID))}})
	}
	return items, nil
}

func decodeEnvironments(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var environments []platform.Environment
	if err := json.Unmarshal(raw, &environments); err != nil {
		return nil, invalidAPI("environment inventory is not an environment list")
	}
	items := make([]Item, 0, len(environments))
	for _, environment := range environments {
		app, name := clean(environment.App), clean(environment.Name)
		title := app + "/" + name
		status := clean(string(environment.HealthState))
		if status == "" || status == string(platform.HealthUnknown) {
			status = "health unknown"
		}
		details := []Detail{
			{Label: "app", Value: app},
			{Label: "environment", Value: name},
			{Label: "node", Value: clean(environment.Node)},
			{Label: "health", Value: status},
			{Label: "health detail", Value: clean(environment.HealthDetail)},
			{Label: "host port", Value: fmt.Sprint(environment.HostPort)},
			{Label: "candidate port", Value: fmt.Sprint(environment.CandidatePort)},
			{Label: "container port", Value: fmt.Sprint(environment.ContainerPort)},
			{Label: "bind", Value: clean(environment.Bind)},
			{Label: "desired release", Value: clean(string(environment.DesiredRelease))},
			{Label: "applied release", Value: clean(string(environment.AppliedRelease))},
			{Label: "serving release", Value: clean(string(environment.ServingRelease))},
			{Label: "health check", Value: clean(environment.Health.Type + " " + environment.Health.Path)},
			{Label: "created", Value: environment.CreatedAt.Format("2006-01-02 15:04:05Z07:00")},
			{Label: "updated", Value: environment.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")},
		}
		details = append(details, envDetails(environment.Env, clean)...)
		details = append(details, Detail{Label: "telemetry", Value: "not configured; no metrics graph is available"})
		search := joinSearch(title, environment.Node, string(environment.HealthState), string(environment.DesiredRelease), string(environment.AppliedRelease), string(environment.ServingRelease), environment.HealthDetail)
		items = append(items, Item{Key: title, Title: title, Subtitle: clean(environment.Node), Status: status, Details: details, SearchText: search, Data: map[string]string{
			"app": app, "environment": name, "node": clean(environment.Node), "status": status,
			"desired_release": clean(string(environment.DesiredRelease)), "serving_release": clean(string(environment.ServingRelease)),
		}})
	}
	return items, nil
}

func decodeReleases(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var releases []platform.Release
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, invalidAPI("release inventory is not a release list")
	}
	items := make([]Item, 0, len(releases))
	for _, release := range releases {
		app, environment := clean(release.App), clean(release.Environment)
		id := clean(string(release.ID))
		title := app + "/" + environment + " " + id
		status := clean(string(release.Status))
		details := []Detail{
			{Label: "release", Value: id},
			{Label: "app", Value: app},
			{Label: "environment", Value: environment},
			{Label: "status", Value: status},
			{Label: "image", Value: clean(release.Image)},
			{Label: "image id", Value: clean(release.ImageID)},
			{Label: "source", Value: clean(release.Source.Type)},
			{Label: "created", Value: release.CreatedAt.Format("2006-01-02 15:04:05Z07:00")},
		}
		if release.Source.Path != "" {
			details = append(details, Detail{Label: "source path", Value: clean(release.Source.Path)})
		}
		if release.Source.URL != "" {
			details = append(details, Detail{Label: "source URL", Value: clean(release.Source.URL)})
		}
		if release.Source.Ref != "" {
			details = append(details, Detail{Label: "source ref", Value: clean(release.Source.Ref)})
		}
		if release.Build != nil {
			details = append(details, Detail{Label: "builder", Value: clean(release.Build.Builder)})
		}
		if release.Command != nil {
			details = append(details, Detail{Label: "release command", Value: clean(strings.Join(release.Command.Argv, " "))})
		}
		if release.CompletedAt != nil {
			details = append(details, Detail{Label: "completed", Value: release.CompletedAt.Format("2006-01-02 15:04:05Z07:00")})
		}
		if release.ErrorCode != "" {
			details = append(details, Detail{Label: "error code", Value: string(release.ErrorCode)})
		}
		if release.Error != "" {
			details = append(details, Detail{Label: "error", Value: clean(release.Error)})
		}
		search := joinSearch(title, status, release.Image, release.ImageID, release.Source.Type, release.Source.URL)
		items = append(items, Item{
			Key: id, Title: title, Subtitle: clean(release.Image), Status: status, Details: details, SearchText: search,
			Data: map[string]string{
				"release": id, "app": app, "environment": environment, "status": status,
				"image": clean(release.Image), "image_id": clean(release.ImageID), "host": "",
			},
			Payload: marshalPayload(release),
		})
	}
	return items, nil
}

func decodeDeployments(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var deployments []platform.Deployment
	if err := json.Unmarshal(raw, &deployments); err != nil {
		return nil, invalidAPI("deployment inventory is not a deployment list")
	}
	items := make([]Item, 0, len(deployments))
	for _, deployment := range deployments {
		app, environment := clean(deployment.App), clean(deployment.Environment)
		action := clean(string(deployment.Action))
		title := app + "/" + environment + " " + action
		status := clean(string(deployment.State))
		operation := clean(string(deployment.OperationID))
		details := []Detail{
			{Label: "app", Value: app},
			{Label: "environment", Value: environment},
			{Label: "action", Value: action},
			{Label: "state", Value: status},
			{Label: "release", Value: clean(string(deployment.ReleaseID))},
			{Label: "previous release", Value: clean(string(deployment.PreviousReleaseID))},
			{Label: "operation", Value: operation},
			{Label: "created", Value: deployment.CreatedAt.Format("2006-01-02 15:04:05Z07:00")},
			{Label: "updated", Value: deployment.UpdatedAt.Format("2006-01-02 15:04:05Z07:00")},
		}
		if deployment.CompletedAt != nil {
			details = append(details, Detail{Label: "completed", Value: deployment.CompletedAt.Format("2006-01-02 15:04:05Z07:00")})
		}
		if deployment.ErrorCode != "" {
			details = append(details, Detail{Label: "error code", Value: string(deployment.ErrorCode)})
		}
		if deployment.Message != "" {
			details = append(details, Detail{Label: "message", Value: clean(deployment.Message)})
		}
		search := joinSearch(title, status, operation, string(deployment.ReleaseID), string(deployment.PreviousReleaseID), deployment.Message)
		items = append(items, Item{
			Key: clean(string(deployment.ID)), Title: title, Subtitle: operation, Status: status, Details: details, SearchText: search,
			Data: map[string]string{
				"app": app, "environment": environment, "state": status, "action": action,
				"release": clean(string(deployment.ReleaseID)), "operation": operation,
			},
			Payload: marshalPayload(deployment),
		})
	}
	return items, nil
}

func decodeResources(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var observations []resources.ResourceInventoryItem
	if err := json.Unmarshal(raw, &observations); err != nil {
		return nil, invalidAPI("resource inventory is not a resource list")
	}
	items := make([]Item, 0, len(observations))
	for _, observation := range observations {
		spec := observation.Spec
		name, host := clean(spec.Name), clean(spec.Host)
		status := clean(string(observation.State))
		if observation.Health != nil && observation.Health.Status != "" {
			status = clean(string(observation.Health.Status))
		}
		details := []Detail{
			{Label: "name", Value: name},
			{Label: "engine", Value: clean(string(spec.Engine))},
			{Label: "version", Value: clean(spec.Version)},
			{Label: "host", Value: host},
			{Label: "database", Value: clean(spec.Database)},
			{Label: "username", Value: clean(spec.Username)},
			{Label: "protection", Value: clean(string(spec.Protection))},
			{Label: "state", Value: clean(string(observation.State))},
		}
		if observation.Endpoint != nil {
			details = append(details,
				Detail{Label: "endpoint", Value: clean(fmt.Sprintf("%s:%d", observation.Endpoint.Address, observation.Endpoint.Port))},
				Detail{Label: "sslmode", Value: clean(observation.Endpoint.SSLMode)},
			)
		}
		if observation.Credential != nil {
			details = append(details, Detail{Label: "credential file", Value: clean(observation.Credential.PasswordFile)})
		}
		if observation.Health != nil {
			details = append(details,
				Detail{Label: "health", Value: clean(string(observation.Health.Status))},
				Detail{Label: "health detail", Value: clean(observation.Health.Detail)},
				Detail{Label: "health checked", Value: observation.Health.CheckedAt.Format("2006-01-02 15:04:05Z07:00")},
			)
		}
		details = append(details, Detail{Label: "telemetry", Value: "source-dependent; no metric is invented"})
		search := joinSearch(name, host, string(spec.Engine), spec.Database, string(observation.State), status)
		items = append(items, Item{
			Key: name + "@" + host, Title: name, Subtitle: host, Status: status, Details: details, SearchText: search,
			Data: map[string]string{
				"name": name, "host": host, "engine": clean(string(spec.Engine)), "status": status,
				"database": clean(spec.Database), "version": clean(spec.Version),
			},
			Payload: marshalPayload(spec),
		})
	}
	return items, nil
}

func decodeBackups(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var artifacts []resources.BackupArtifact
	if err := json.Unmarshal(raw, &artifacts); err != nil {
		return nil, invalidAPI("backup inventory is not a backup list")
	}
	items := make([]Item, 0, len(artifacts))
	for _, artifact := range artifacts {
		archive := clean(artifact.Archive)
		title := clean(artifact.Resource) + " " + pathBase(archive)
		status := "unverified"
		if artifact.Verified {
			status = "verified"
		}
		details := []Detail{
			{Label: "archive", Value: archive},
			{Label: "manifest", Value: clean(artifact.Manifest)},
			{Label: "resource", Value: clean(artifact.Resource)},
			{Label: "host", Value: clean(artifact.Host)},
			{Label: "database", Value: clean(artifact.Database)},
			{Label: "effect", Value: clean(artifact.EffectID)},
			{Label: "size", Value: fmt.Sprintf("%d bytes", artifact.SizeBytes)},
			{Label: "sha256", Value: clean(artifact.SHA256)},
			{Label: "reason", Value: clean(artifact.Reason)},
			{Label: "created", Value: artifact.CreatedAt.Format("2006-01-02 15:04:05Z07:00")},
		}
		if artifact.VerifiedAt != nil {
			details = append(details, Detail{Label: "verified", Value: artifact.VerifiedAt.Format("2006-01-02 15:04:05Z07:00")})
		}
		search := joinSearch(title, artifact.Resource, artifact.Host, artifact.Database, artifact.Archive, artifact.SHA256, status)
		items = append(items, Item{
			Key: artifact.Archive, Title: title, Subtitle: clean(artifact.Host), Status: status, Details: details, SearchText: search,
			Data: map[string]string{
				"archive": archive, "resource": clean(artifact.Resource), "host": clean(artifact.Host),
				"database": clean(artifact.Database), "sha256": clean(artifact.SHA256), "status": status,
			},
			Payload: marshalPayload(artifact),
		})
	}
	return items, nil
}

func marshalPayload(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return raw
}

func pathBase(value string) string {
	if index := strings.LastIndexAny(value, `/\`); index >= 0 && index+1 < len(value) {
		return value[index+1:]
	}
	return value
}

func envDetails(values map[string]string, clean func(string) string) []Detail {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	details := make([]Detail, 0, min(len(keys), 12)+1)
	for index, key := range keys {
		if index == 12 {
			details = append(details, Detail{Label: "configuration", Value: fmt.Sprintf("%d more keys", len(keys)-12)})
			break
		}
		value := clean(values[key])
		if secretKey(key) {
			value = "[redacted]"
		}
		details = append(details, Detail{Label: "env " + clean(key), Value: value})
	}
	return details
}

// decodeGeneric renders extension collections without assuming their schema.
// It keeps scalar fields, redacts secret-looking keys, and never invents rows.
func decodeGeneric(raw json.RawMessage, clean func(string) string) ([]Item, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values []map[string]any
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, invalidAPI("extension inventory is not a list of objects")
	}
	items := make([]Item, 0, len(values))
	for index, value := range values {
		title := genericTitle(value, clean)
		if title == "" {
			title = fmt.Sprintf("item-%d", index+1)
		}
		status := genericStatus(value)
		details := make([]Detail, 0, min(len(value), 48))
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var search strings.Builder
		search.WriteString(title + " " + status)
		for _, key := range keys {
			if len(details) >= 48 {
				details = append(details, Detail{Label: "fields", Value: fmt.Sprintf("%d more", len(keys)-len(details))})
				break
			}
			text := genericValue(value[key], clean, key)
			if text == "" {
				continue
			}
			details = append(details, Detail{Label: clean(key), Value: text})
			search.WriteString(" " + key + " " + text)
		}
		searchText := strings.ToLower(clean(search.String()))
		items = append(items, Item{Key: fmt.Sprintf("%s-%d", title, index), Title: title, Status: status, Details: details, SearchText: searchText, Data: map[string]string{"name": title, "status": status}})
	}
	return items, nil
}

func genericTitle(value map[string]any, clean func(string) string) string {
	for _, key := range []string{"name", "id", "resource", "host", "target"} {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return clean(text)
		}
	}
	return ""
}

func genericStatus(value map[string]any) string {
	for _, key := range []string{"status", "state", "health"} {
		raw, ok := value[key]
		if !ok {
			continue
		}
		switch item := raw.(type) {
		case string:
			return strings.ToLower(item)
		case map[string]any:
			if state, ok := item["state"].(string); ok {
				return strings.ToLower(state)
			}
		}
	}
	return ""
}

func genericValue(value any, clean func(string) string, key string) string {
	if secretKey(key) {
		return "[redacted]"
	}
	switch item := value.(type) {
	case nil:
		return ""
	case string:
		return clean(item)
	case bool:
		if item {
			return "true"
		}
		return "false"
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", item), "0"), ".")
	default:
		raw, err := json.Marshal(redactNested(item, clean))
		if err != nil {
			return ""
		}
		text := clean(string(raw))
		if len(text) > 320 {
			text = text[:317] + "..."
		}
		return text
	}
}

func redactNested(value any, clean func(string) string) any {
	switch item := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(item))
		for key, nested := range item {
			if secretKey(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = redactNested(nested, clean)
		}
		return out
	case []any:
		out := make([]any, len(item))
		for index, nested := range item {
			out[index] = redactNested(nested, clean)
		}
		return out
	case string:
		return clean(item)
	default:
		return value
	}
}

func secretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, marker := range []string{"password", "passwd", "secret", "token", "credential", "private_key", "private-key", "apikey", "api_key", "access_key"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func timeText(value *time.Time, clean func(string) string) string {
	if value == nil {
		return "never"
	}
	return clean(value.UTC().Format("2006-01-02 15:04:05Z07:00"))
}

func joinSearch(values ...string) string {
	clean := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			clean = append(clean, value)
		}
	}
	return strings.ToLower(strings.Join(clean, " "))
}

// Sanitize is the single text boundary for server data. Control characters are
// escaped and registered secrets are replaced before any renderer sees a value.
func Sanitize(registry interface{ Redact(string) string }, text string) string {
	if registry != nil {
		text = registry.Redact(text)
	}
	return output.SafeText(text)
}

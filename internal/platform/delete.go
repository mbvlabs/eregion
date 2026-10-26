package platform

import (
	"encoding/json"
)

// Application and environment deletion are durable, target-confirmed actions.
// The confirmation token names the exact target (MASTER D12); the control plane
// checks it against the records it just loaded, so a stale or wrong name never
// deletes anything.
const (
	KindAppDelete         = "platform.app.delete"
	KindEnvironmentDelete = "platform.environment.delete"
)

// AppDeleteInput removes an application and every environment it owns. It is
// accepted only with Confirm equal to the current application name.
type AppDeleteInput struct {
	App     string `json:"app,omitempty"`
	Confirm string `json:"confirm,omitempty"`
}

func (in *AppDeleteInput) Target() Target { return Target{Kind: TargetApp, App: in.App} }
func (in *AppDeleteInput) Preview() any   { return in }

// EnvironmentDeleteInput removes one environment. Deleting the last environment
// also removes the application, as deploycrate does (MASTER D13).
type EnvironmentDeleteInput struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Confirm     string `json:"confirm,omitempty"`
}

func (in *EnvironmentDeleteInput) Target() Target {
	return Target{Kind: TargetEnvironment, App: in.App, Environment: in.Environment}
}
func (in *EnvironmentDeleteInput) Preview() any { return in }

func normalizeAppDelete(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetApp {
		return nil, invalid("target", "platform.app.delete requires target app/<app>.")
	}
	var input AppDeleteInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if input.App != "" && input.App != target.App {
		return nil, invalid("app", "The app field must match the target app, or be omitted.")
	}
	input.App = target.App
	if err := ValidateName(input.App); err != nil {
		return nil, err
	}
	if input.Confirm != "" {
		if err := ValidateTargetConfirmation(input.Target(), input.Confirm); err != nil {
			return nil, err
		}
	}
	return &input, nil
}

func normalizeEnvironmentDelete(target Target, raw json.RawMessage) (ActionInput, error) {
	if target.Kind != TargetEnvironment {
		return nil, invalid("target", "platform.environment.delete requires target env/<app>/<env>.")
	}
	var input EnvironmentDeleteInput
	if err := strictDecode(raw, &input); err != nil {
		return nil, err
	}
	if (input.App != "" && input.App != target.App) || (input.Environment != "" && input.Environment != target.Environment) {
		return nil, invalid("environment", "The app and environment fields must match the target, or be omitted.")
	}
	input.App, input.Environment = target.App, target.Environment
	if err := ValidateName(input.App); err != nil {
		return nil, err
	}
	if err := ValidateName(input.Environment); err != nil {
		return nil, err
	}
	if input.Confirm != "" {
		if err := ValidateTargetConfirmation(input.Target(), input.Confirm); err != nil {
			return nil, err
		}
	}
	return &input, nil
}

// DeletionSteps returns the ordered effects a deletion performs. Workload and
// route cleanup come first: no database row is removed while a container or a
// public route could still use it. Operation history is never listed here.
func DeletionSteps(app, environment string) []string {
	steps := []string{
		"stop and remove the workload containers selected by ownership labels",
		"remove the Caddy routes and reload Caddy",
		"delete the vault secret namespaces",
		"delete domain bindings",
		"delete deployments",
		"delete releases",
		"delete builds",
		"delete environment revisions",
		"delete change history",
	}
	if environment == "" {
		steps = append(steps, "delete every environment row")
		steps = append(steps, "delete the application row")
	} else {
		steps = append(steps, "delete the environment row (and the application when it was the last one)")
	}
	return steps
}

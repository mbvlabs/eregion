package appdef

import (
	"errors"
	"fmt"

	"github.com/mbvlabs/eregion/internal/platform"
)

// MaxDefinitionBytes bounds a definition received by the server as well as
// one read from a local file. Definitions contain configuration, not secret
// values; callers must not log the document on validation failures.
const MaxDefinitionBytes = 128 << 10

// ResolveSelection parses an untrusted definition and selects exactly one
// app/environment target. An explicit app may supply a missing document app,
// but cannot override one. Other environment declarations are not selected or
// interpreted as deletion requests.
//
// This only validates and resolves the document; it does not read inventory,
// validate references against the server, or authorize an apply.
func ResolveSelection(raw []byte, app, environment string) (*Resolved, error) {
	if len(raw) > MaxDefinitionBytes {
		return nil, errors.New("definition exceeds 128 KiB")
	}
	if err := platform.ValidateName(environment); err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}
	definition, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	if app != "" && definition.App != "" && app != definition.App {
		return nil, errors.New("definition app disagrees with selected app")
	}
	if app == "" {
		app = definition.App
	}
	if err := platform.ValidateName(app); err != nil {
		return nil, fmt.Errorf("app: %w", err)
	}
	resolved, err := definition.Resolve(environment)
	if err != nil {
		return nil, err
	}
	resolved.App = app
	return resolved, nil
}

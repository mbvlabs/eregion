// Package platform defines the shared application-platform domain: canonical
// targets, validated action inputs, inventory records, and the node steps used by
// releases and deployments. It has no database, HTTP, or River dependency so the
// API, worker, CLI, and extension packages can share one contract.
package platform

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

type AppID string
type EnvironmentID string
type RevisionID string
type ReleaseID string
type DeploymentID string
type ChangeID string

var (
	appIDPattern         = regexp.MustCompile(`^app_[0-9a-f]{32}$`)
	environmentIDPattern = regexp.MustCompile(`^env_[0-9a-f]{32}$`)
	revisionIDPattern    = regexp.MustCompile(`^erv_[0-9a-f]{32}$`)
	releaseIDPattern     = regexp.MustCompile(`^rel_[0-9a-f]{32}$`)
	deploymentIDPattern  = regexp.MustCompile(`^dep_[0-9a-f]{32}$`)
	changeIDPattern      = regexp.MustCompile(`^chg_[0-9a-f]{32}$`)
	namePattern          = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	imagePattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,254}$`)
	capabilityPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	envKeyPattern        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	shaPattern           = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
)

func newID(prefix string) (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func NewAppID() (AppID, error) {
	value, err := newID("app_")
	return AppID(value), err
}

func NewEnvironmentID() (EnvironmentID, error) {
	value, err := newID("env_")
	return EnvironmentID(value), err
}

func NewEnvironmentRevisionID() (RevisionID, error) {
	value, err := newID("erv_")
	return RevisionID(value), err
}

func NewReleaseID() (ReleaseID, error) {
	value, err := newID("rel_")
	return ReleaseID(value), err
}

func NewDeploymentID() (DeploymentID, error) {
	value, err := newID("dep_")
	return DeploymentID(value), err
}

func NewChangeID() (ChangeID, error) {
	value, err := newID("chg_")
	return ChangeID(value), err
}

func (id AppID) Validate() error {
	if !appIDPattern.MatchString(string(id)) {
		return contract.NewFault(contract.ValidationFailed, "invalid app ID", "Use the app ID returned by inventory.")
	}
	return nil
}

func (id EnvironmentID) Validate() error {
	if !environmentIDPattern.MatchString(string(id)) {
		return contract.NewFault(contract.ValidationFailed, "invalid environment ID", "Use the environment ID returned by inventory.")
	}
	return nil
}

func (id RevisionID) Validate() error {
	if !revisionIDPattern.MatchString(string(id)) {
		return contract.NewFault(contract.ValidationFailed, "invalid environment revision ID", "Use the revision ID returned by inventory.")
	}
	return nil
}

func (id ReleaseID) Validate() error {
	if !releaseIDPattern.MatchString(string(id)) {
		return contract.NewFault(contract.ValidationFailed, "invalid release ID", "Use the release ID returned by inventory.")
	}
	return nil
}

func (id DeploymentID) Validate() error {
	if !deploymentIDPattern.MatchString(string(id)) {
		return contract.NewFault(contract.ValidationFailed, "invalid deployment ID", "Use the deployment ID returned by inventory.")
	}
	return nil
}

func (id ChangeID) Validate() error {
	if !changeIDPattern.MatchString(string(id)) {
		return contract.NewFault(contract.ValidationFailed, "invalid change ID", "Use the change ID returned by inventory.")
	}
	return nil
}

// ValidateName validates an app, environment, or node name.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid platform name", "Use 1–40 lowercase letters, digits, and inner hyphens; start with a letter and end with a letter or digit.")
	}
	return nil
}

// ValidateImage validates an OCI image reference. It rejects shell
// metacharacters and a leading dash; it is a shape check, not a registry lookup.
func ValidateImage(image string) error {
	if !imagePattern.MatchString(image) {
		return contract.NewFault(contract.ValidationFailed, "invalid container image reference", "Use a registry/repository:tag or digest reference without spaces or shell characters.")
	}
	return nil
}

type TargetKind string

const (
	TargetPlatform    TargetKind = "platform"
	TargetApp         TargetKind = "app"
	TargetEnvironment TargetKind = "env"
	TargetNode        TargetKind = "node"
)

// Target is a parsed canonical operation target.
type Target struct {
	Kind        TargetKind
	App         string
	Environment string
	Node        string
}

func PlatformTarget() Target { return Target{Kind: TargetPlatform} }

func NewAppTarget(app string) (Target, error) {
	if err := ValidateName(app); err != nil {
		return Target{}, err
	}
	return Target{Kind: TargetApp, App: app}, nil
}

func NewEnvironmentTarget(app, environment string) (Target, error) {
	if err := ValidateName(app); err != nil {
		return Target{}, err
	}
	if err := ValidateName(environment); err != nil {
		return Target{}, err
	}
	return Target{Kind: TargetEnvironment, App: app, Environment: environment}, nil
}

func NewNodeTarget(node string) (Target, error) {
	if err := ValidateName(node); err != nil {
		return Target{}, err
	}
	return Target{Kind: TargetNode, Node: node}, nil
}

func (t Target) String() string {
	switch t.Kind {
	case TargetPlatform:
		return "platform"
	case TargetApp:
		return "app/" + t.App
	case TargetEnvironment:
		return "env/" + t.App + "/" + t.Environment
	case TargetNode:
		return "node/" + t.Node
	default:
		return ""
	}
}

func (t Target) Validate() error {
	_, err := ParseTarget(t.String())
	if err != nil {
		return err
	}
	switch t.Kind {
	case TargetPlatform:
		return nil
	case TargetApp:
		return ValidateName(t.App)
	case TargetEnvironment:
		if err := ValidateName(t.App); err != nil {
			return err
		}
		return ValidateName(t.Environment)
	case TargetNode:
		return ValidateName(t.Node)
	default:
		return contract.NewFault(contract.ValidationFailed, "invalid platform target", "Use platform, app/<app>, env/<app>/<env>, or node/<node>.")
	}
}

// ParseTarget parses the canonical target grammar. It does not accept aliases.
func ParseTarget(raw string) (Target, error) {
	if raw == "" || len(raw) > 200 || strings.TrimSpace(raw) != raw {
		return Target{}, contract.NewFault(contract.ValidationFailed, "invalid platform target", "Use platform, app/<app>, env/<app>/<env>, or node/<node>.")
	}
	parts := strings.Split(raw, "/")
	switch {
	case len(parts) == 1 && parts[0] == "platform":
		return PlatformTarget(), nil
	case len(parts) == 2 && parts[0] == "app":
		return NewAppTarget(parts[1])
	case len(parts) == 3 && parts[0] == "env":
		return NewEnvironmentTarget(parts[1], parts[2])
	case len(parts) == 2 && parts[0] == "node":
		return NewNodeTarget(parts[1])
	default:
		return Target{}, contract.NewFault(contract.ValidationFailed, "invalid platform target", "Use platform, app/<app>, env/<app>/<env>, or node/<node>.")
	}
}

// ContainerName returns the deterministic Docker container name for one
// environment role. It is bounded and derived only from validated values.
func ContainerName(environmentID EnvironmentID, role string) string {
	short := strings.TrimPrefix(string(environmentID), "env_")
	if len(short) > 12 {
		short = short[:12]
	}
	switch role {
	case "serving", "candidate", "previous":
	default:
		role = "candidate"
	}
	return "eregion-" + short + "-" + role
}

// CandidateContainerName includes the operation identity so two deployments of
// the same environment cannot share a candidate effect key or container.
func CandidateContainerName(environmentID EnvironmentID, operationID contract.OperationID) string {
	short := strings.TrimPrefix(string(environmentID), "env_")
	if len(short) > 12 {
		short = short[:12]
	}
	op := strings.TrimPrefix(string(operationID), "op_")
	if len(op) > 12 {
		op = op[:12]
	}
	return "eregion-" + short + "-candidate-" + op
}

func (id EnvironmentID) Container(role string) string {
	return ContainerName(id, role)
}

func trimForError(value string) string {
	if len(value) > 160 {
		return value[:160] + "…"
	}
	return value
}

func validatePort(port, minimum int) error {
	if port < minimum || port > 65535 {
		return contract.NewFault(contract.ValidationFailed, "port out of range", fmt.Sprintf("Use a port from %d to 65535.", minimum))
	}
	return nil
}

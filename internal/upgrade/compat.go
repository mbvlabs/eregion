package upgrade

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Environment describes the running binary and database the release would be
// applied to. SchemaVersion is the database's current forward-only schema.
type Environment struct {
	Target        Target
	OS            string
	Arch          string
	Version       string
	Contract      string
	NodeProtocol  int
	SchemaVersion int
}

// Check is one compatibility decision. Detail is safe to display.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Compatibility is the complete, reviewable result of a compatibility check.
type Compatibility struct {
	Target     Target  `json:"target"`
	Compatible bool    `json:"compatible"`
	Checks     []Check `json:"checks"`
}

// Err converts an incompatible report into a precondition failure with details.
func (c Compatibility) Err() error {
	if c.Compatible {
		return nil
	}
	failures := make([]string, 0, len(c.Checks))
	for _, check := range c.Checks {
		if !check.OK {
			failures = append(failures, check.Name+": "+check.Detail)
		}
	}
	fault := contract.NewFault(contract.PreconditionFailed, "release is not compatible with this installation", "Resolve every failed compatibility check before applying the release.")
	fault.Details = map[string]any{"checks": failures}
	return fault
}

func appendCheck(report *Compatibility, name string, ok bool, detail string) {
	report.Checks = append(report.Checks, Check{Name: name, OK: ok, Detail: detail})
	if !ok {
		report.Compatible = false
	}
}

// CheckCompatibility evaluates target, OS, architecture, contract, node
// protocol, database schema direction, and version transitions. It performs no
// network or filesystem access.
func CheckCompatibility(m Manifest, env Environment) Compatibility {
	target := env.Target
	if target == "" {
		target = TargetOperator
	}
	report := Compatibility{Target: target, Compatible: true}
	if err := m.Validate(); err != nil {
		appendCheck(&report, "manifest", false, compactError(err))
		return report
	}
	if !target.Supports(env.OS) {
		appendCheck(&report, "platform", false, string(target)+" releases do not support "+display(env.OS))
	} else {
		appendCheck(&report, "platform", true, display(env.OS)+"/"+display(env.Arch))
	}
	artifact, artifactErr := SelectArtifact(m, target, env.OS, env.Arch)
	if artifactErr != nil {
		appendCheck(&report, "artifact", false, "no signed "+string(target)+" artifact for "+display(env.OS)+"/"+display(env.Arch))
	} else {
		appendCheck(&report, "artifact", true, artifact.Name)
	}
	coreChecks(m, env, &report)
	return report
}

// coreChecks holds the checks shared by one installation and a control-plane
// inventory. The artifact selection is local and separate.
func coreChecks(m Manifest, env Environment, report *Compatibility) {
	protocolOK := env.NodeProtocol > 0 && m.NodeProtocol == env.NodeProtocol
	appendCheck(report, "node protocol", protocolOK, fmt.Sprintf("manifest %d, running %d", m.NodeProtocol, env.NodeProtocol))

	contractOK := contractCompatible(m.Contract, env.Contract)
	appendCheck(report, "contract", contractOK, "manifest "+m.Contract+", running "+display(env.Contract))

	switch {
	case env.SchemaVersion <= 0:
		appendCheck(report, "database schema", false, "current schema version is unknown; inspect migrations before upgrading")
	case env.SchemaVersion > m.SchemaVersion:
		appendCheck(report, "database schema", false, fmt.Sprintf("database schema %d is newer than release schema %d; schema changes are forward-only and downgrades are unsupported", env.SchemaVersion, m.SchemaVersion))
	default:
		appendCheck(report, "database schema", true, fmt.Sprintf("database schema %d can migrate forward to %d", env.SchemaVersion, m.SchemaVersion))
	}

	if env.Version == "" {
		if len(m.UpgradeFrom) > 0 {
			appendCheck(report, "version transition", false, "running version is unknown but the release restricts upgrade_from")
		} else {
			appendCheck(report, "version transition", true, "running version is unknown; no upgrade_from restriction is published")
		}
		return
	}
	if env.Version == "dev" {
		appendCheck(report, "version transition", true, "development build; upgrade_from is not enforced")
		return
	}
	current, err := ParseVersion(env.Version)
	if err != nil {
		appendCheck(report, "version transition", false, "running version is not a semantic version")
		return
	}
	next, _ := ParseVersion(m.Version)
	switch {
	case next.Compare(current) < 0:
		appendCheck(report, "version transition", false, "manifest version "+m.Version+" is older than running version "+env.Version)
	case next.Compare(current) == 0:
		appendCheck(report, "version transition", true, "reinstalling the same version")
	case len(m.UpgradeFrom) == 0:
		appendCheck(report, "version transition", true, "no upgrade_from restriction is published")
	case containsVersion(m.UpgradeFrom, current):
		appendCheck(report, "version transition", true, env.Version+" is a published upgrade source")
	default:
		appendCheck(report, "version transition", false, env.Version+" is not listed in manifest upgrade_from")
	}
}

// SelectArtifact returns the unique artifact for target/os/arch.
func SelectArtifact(m Manifest, target Target, goos, arch string) (Artifact, error) {
	for _, artifact := range m.Artifacts {
		if artifact.Target == target && artifact.OS == goos && artifact.Arch == arch {
			return artifact, nil
		}
	}
	return Artifact{}, contract.NewFault(contract.NotFound, "release has no artifact for this target and platform", "Choose a release that publishes "+string(target)+" for "+goos+"/"+arch+".")
}

func containsVersion(values []string, want Version) bool {
	for _, value := range values {
		version, err := ParseVersion(value)
		if err == nil && version.Compare(want) == 0 {
			return true
		}
	}
	return false
}

func display(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}

func compactError(err error) string {
	return strings.TrimSpace(contract.Classify(err).Message)
}

// ControlPlaneInstance is one running control-plane process. Instance IDs are
// operator-supplied labels and are never inferred from network position.
type ControlPlaneInstance struct {
	ID            string `json:"id"`
	Version       string `json:"version"`
	Contract      string `json:"contract"`
	NodeProtocol  int    `json:"node_protocol"`
	SchemaVersion int    `json:"schema_version"`
}

// CheckControlPlaneCompatibility verifies that every running control-plane
// process is a supported upgrade source and agrees on the database schema,
// envelope contract, and node protocol. It does not prove high availability or
// observe live processes; it checks the inventory the operator supplies.
func CheckControlPlaneCompatibility(m Manifest, instances []ControlPlaneInstance) error {
	if len(instances) == 0 {
		return contract.NewFault(contract.InputRequired, "control-plane inventory is empty", "Supply at least one running control-plane instance; an empty inventory is not evidence of a coordinated upgrade.")
	}
	failures := []string{}
	for _, instance := range instances {
		report := Compatibility{Target: TargetOperator, Compatible: true}
		coreChecks(m, Environment{
			Target: TargetOperator, OS: "linux", Arch: "amd64", Version: instance.Version,
			Contract: instance.Contract, NodeProtocol: instance.NodeProtocol, SchemaVersion: instance.SchemaVersion,
		}, &report)
		for _, check := range report.Checks {
			if !check.OK {
				label := instance.ID
				if label == "" {
					label = instance.Version
				}
				failures = append(failures, label+" "+check.Name+": "+check.Detail)
			}
		}
	}
	first := instances[0]
	for _, instance := range instances[1:] {
		label := instance.ID
		if label == "" {
			label = instance.Version
		}
		if instance.SchemaVersion != first.SchemaVersion {
			failures = append(failures, label+" database schema differs from the inventory: "+fmt.Sprintf("%d vs %d", instance.SchemaVersion, first.SchemaVersion))
		}
		if instance.NodeProtocol != first.NodeProtocol {
			failures = append(failures, label+" node protocol differs from the inventory: "+fmt.Sprintf("%d vs %d", instance.NodeProtocol, first.NodeProtocol))
		}
		if instance.Contract != first.Contract {
			failures = append(failures, label+" contract differs from the inventory: "+instance.Contract+" vs "+first.Contract)
		}
	}
	if len(failures) > 0 {
		fault := contract.NewFault(contract.PreconditionFailed, "control-plane instances are not compatible with this release", "Upgrade or drain incompatible instances first; do not claim a coordinated upgrade.")
		fault.Details = map[string]any{"instances": failures}
		return fault
	}
	return nil
}

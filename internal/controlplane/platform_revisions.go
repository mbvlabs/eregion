package controlplane

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

// insertEnvironmentRevisionTx appends one immutable revision. The environment
// row lock held by the caller (or the just-inserted row in this transaction)
// serializes the per-environment number.
func insertEnvironmentRevisionTx(ctx context.Context, tx *entities.StoreTx, envID platform.EnvironmentID, opID contract.OperationID, actor contract.Actor, operation, summary string, state platform.RevisionState) (platform.RevisionID, error) {
	if err := actor.Validate(); err != nil {
		return "", err
	}
	if len(operation) < 1 || len(operation) > 64 {
		return "", contract.NewFault(contract.ValidationFailed, "invalid revision operation", "Use an operation name between 1 and 64 bytes.")
	}
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return "", err
	}
	revisionID, err := platform.NewEnvironmentRevisionID()
	if err != nil {
		return "", unavailable()
	}
	number, err := tx.Platform().Revisions().NextNumber(ctx, envID)
	if err != nil {
		return "", unavailable(err)
	}
	if _, err := tx.Platform().Revisions().Insert(ctx, entities.RevisionInsert{
		ID: revisionID, EnvironmentID: envID, Number: number, OperationID: opID,
		Actor: actor, Operation: operation, Summary: summary, State: state,
	}); err != nil {
		return "", platformWriteError(err, "environment revision could not be recorded")
	}
	return revisionID, nil
}

func nullableOperationID(id contract.OperationID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

// insertEnvironmentTx writes the environment row, its first revision and the
// current-revision pointer in one transaction.
func (s *Service) insertEnvironmentTx(ctx context.Context, tx *entities.StoreTx, app platform.App, spec *platform.EnvironmentSpec, opID contract.OperationID, actor contract.Actor, operation, summary string, deployIntent bool) (platform.EnvironmentID, error) {
	nodeName, buildNode, err := s.resolveEnvironmentPlacement(ctx, tx, spec)
	if err != nil {
		return "", err
	}
	spec.Node = nodeName
	spec.Runtime.Node = nodeName
	hostPort, candidatePort, err := allocatePorts(ctx, tx, spec.HostPort)
	if err != nil {
		return "", err
	}
	state := revisionStateFromSpec(*spec, nodeName, deployIntent)
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return "", err
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		return "", unavailable()
	}
	secretRefs := environmentSecretRefs(spec, state)
	containerPort := spec.ContainerPort
	for _, process := range state.Processes {
		if process.Kind == "web" && process.ContainerPort > 0 {
			containerPort = process.ContainerPort
			break
		}
	}
	protected := platform.DefaultEnvironmentProtection(spec.Name)
	if spec.Protected != nil {
		protected = *spec.Protected
	}
	if _, err := tx.Platform().Environments().Insert(ctx, entities.EnvironmentInsert{
		ID: envID, AppID: app.ID, Name: spec.Name, Node: nodeName, BuildNode: buildNode,
		HostPort: hostPort, CandidatePort: candidatePort, ContainerPort: containerPort,
		Bind: spec.Bind, HealthType: state.Runtime.HealthType, HealthPath: state.Runtime.HealthPath,
		HealthTimeoutSeconds: state.Runtime.HealthTimeoutSeconds, Env: state.Config,
		SecretRefs: secretRefs, Protected: protected,
	}); err != nil {
		return "", platformWriteError(err, "environment name or port already exists")
	}
	revisionID, err := insertEnvironmentRevisionTx(ctx, tx, envID, opID, actor, operation, summary, state)
	if err != nil {
		return "", err
	}
	if _, err := lockPointerEnvironmentTx(ctx, tx, envID); err != nil {
		return "", err
	}
	if err := lockRevisionPointerTx(ctx, tx, envID, revisionID); err != nil {
		return "", err
	}
	if _, err := tx.Platform().Environments().SetCurrentRevision(ctx, envID, revisionID); err != nil {
		return "", unavailable(err)
	}
	// The optional first domain bindings commit before the caller queues a
	// first deploy, so the deploy's deployability gate sees the primary binding.
	if err := s.insertEnvironmentDomains(ctx, tx, app, envID, spec); err != nil {
		return "", err
	}
	return envID, nil
}

func (s *Service) resolveEnvironmentPlacement(ctx context.Context, tx *entities.StoreTx, spec *platform.EnvironmentSpec) (string, string, error) {
	node, err := nodeByNameTx(ctx, tx, spec.Node)
	if err != nil {
		return "", "", err
	}
	if err := s.checkPlacement(node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return "", "", err
	}
	buildNode := ""
	if spec.BuildNode != "" {
		builder, err := nodeByNameTx(ctx, tx, spec.BuildNode)
		if err != nil {
			return "", "", err
		}
		if err := s.checkPlacement(builder, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
			return "", "", err
		}
		buildNode = builder.Name
	}
	return node.Name, buildNode, nil
}

func environmentSecretRefs(spec *platform.EnvironmentSpec, state platform.RevisionState) map[string]string {
	secretRefs := spec.SecretRefs
	if secretRefs == nil {
		secretRefs = map[string]string{}
	}
	if len(state.SecretNames) > 0 {
		return map[string]string{}
	}
	return secretRefs
}

func (s *Service) insertEnvironmentDomains(ctx context.Context, tx *entities.StoreTx, app platform.App, envID platform.EnvironmentID, spec *platform.EnvironmentSpec) error {
	env := platform.Environment{ID: envID, App: app.Name, Name: spec.Name, Node: spec.Node}
	if len(spec.Domains) == 0 {
		if spec.Domain == "" {
			return nil
		}
		_, err := s.insertDomainTx(ctx, tx, env, platform.DomainInput{
			App: app.Name, Environment: spec.Name, Domain: spec.Domain,
			Primary: spec.DomainPrimary, Addresses: spec.DomainAddresses, CheckPath: spec.DomainCheckPath,
		})
		return err
	}
	for _, declaration := range spec.Domains {
		if _, err := s.insertDomainTx(ctx, tx, env, platform.DomainInput{
			App: app.Name, Environment: spec.Name, Domain: declaration.Domain,
			Primary: declaration.Primary, Addresses: spec.DomainAddresses, CheckPath: declaration.CheckPath,
		}); err != nil {
			return err
		}
	}
	return nil
}

func revisionStateFromSpec(spec platform.EnvironmentSpec, node string, deployIntent bool) platform.RevisionState {
	state := platform.RevisionState{
		Source:            spec.Source,
		AutoDeploy:        spec.AutoDeploy,
		WebhookSecretName: spec.WebhookSecretName,
		Runtime:           spec.Runtime,
		Processes:         spec.Processes,
		Config:            spec.Config,
		SecretNames:       spec.SecretNames,
		Resources:         spec.Resources,
		Retention:         spec.Retention.Clone(),
		DeployIntent:      deployIntent,
	}
	if state.Runtime.Node == "" {
		state.Runtime.Node = node
	}
	if len(state.Config) == 0 && len(spec.Env) > 0 {
		state.Config = spec.Env
	}
	if len(state.Processes) == 0 {
		port := spec.ContainerPort
		if port == 0 {
			port = platform.DefaultContainerPort
		}
		state.Processes = []platform.ProcessSpec{{Name: "web", Kind: "web", Args: []string{}, Replicas: 1, ContainerPort: port}}
	}
	return state
}

// publishEnvironmentRevisionTx appends a revision and mirrors the legacy
// columns the current deploy worker still reads. The mirror goes away with the
// per-process container work in work list row 5.
func publishEnvironmentRevisionTx(ctx context.Context, tx *entities.StoreTx, env platform.Environment, opID contract.OperationID, actor contract.Actor, operation, summary string, state platform.RevisionState, buildNode *string) (platform.RevisionID, error) {
	if _, err := lockPointerEnvironmentTx(ctx, tx, env.ID); err != nil {
		return "", err
	}
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return "", err
	}
	revisionID, err := insertEnvironmentRevisionTx(ctx, tx, env.ID, opID, actor, operation, summary, state)
	if err != nil {
		return "", err
	}
	if err := lockRevisionPointerTx(ctx, tx, env.ID, revisionID); err != nil {
		return "", err
	}
	node := state.Runtime.Node
	if node == "" {
		node = env.Node
	}
	containerPort := env.ContainerPort
	for _, process := range state.Processes {
		if process.Kind == "web" && process.ContainerPort > 0 {
			containerPort = process.ContainerPort
			break
		}
	}
	refs := env.SecretRefs
	if len(state.SecretNames) > 0 {
		refs = map[string]string{}
	}
	buildNodeValue := env.BuildNode
	if buildNode != nil {
		buildNodeValue = *buildNode
	}
	config := state.Config
	if _, err := tx.Platform().Environments().PublishMirror(ctx, entities.EnvironmentMirror{
		ID: env.ID, Node: node, BuildNode: buildNodeValue, ContainerPort: containerPort,
		HealthType: state.Runtime.HealthType, HealthPath: state.Runtime.HealthPath,
		HealthTimeoutSeconds: state.Runtime.HealthTimeoutSeconds, Env: config,
		SecretRefs: refs, RevisionID: revisionID,
	}); err != nil {
		return "", platformWriteError(err, "environment revision could not be applied")
	}
	return revisionID, nil
}

func currentRevisionStateTx(_ *entities.StoreTx, env platform.Environment) (platform.RevisionState, error) {
	if env.Revision != nil && env.RevisionID != "" && env.Revision.ID == env.RevisionID {
		return platform.CloneRevisionState(env.Revision.RevisionState), nil
	}
	return platform.RevisionState{}, contract.NewFault(contract.PreconditionFailed, "environment current revision is missing or inconsistent", "Inspect the environment revision before retrying.")
}

// ---------------------------------------------------------------- inventory

func (s *Service) revisionByID(ctx context.Context, id platform.RevisionID) (platform.EnvironmentRevision, error) {
	revision, found, err := s.entities.Platform().Revisions().GetByID(ctx, id)
	if err != nil {
		return revision, unavailable(err)
	}
	if !found {
		return revision, contract.NewFault(contract.NotFound, "environment revision not found", "Use a revision ID from inventory.")
	}
	return revision, nil
}

func (s *Service) inventoryRevisions(ctx context.Context, query platform.InventoryQuery) ([]platform.EnvironmentRevision, error) {
	items, err := s.entities.Platform().Revisions().List(ctx, entities.RevisionFilter{
		App: query.App, Environment: query.Environment, State: query.State, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	if items == nil {
		items = []platform.EnvironmentRevision{}
	}
	return items, nil
}

func (s *Service) inventoryDeployability(ctx context.Context, query platform.InventoryQuery) ([]platform.EnvironmentDeployabilityReport, error) {
	environments, err := s.inventoryEnvironments(ctx, query)
	if err != nil {
		return nil, err
	}
	items := make([]platform.EnvironmentDeployabilityReport, 0, len(environments))
	for _, env := range environments {
		items = append(items, platform.EnvironmentDeployabilityReport{
			App: env.App, Environment: env.Name, CurrentRevisionID: env.RevisionID,
			Deployability: env.Deployability,
		})
	}
	return items, nil
}

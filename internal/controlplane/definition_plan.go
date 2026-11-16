package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// definitionSnapshot is the timestamp-free database material to be compared
// under admission locks. Provider reads occur only after its transaction ends.
type definitionSnapshot struct {
	Workspace        WorkspaceIdentity
	AppID            platform.AppID
	ImageRepository  string
	EnvironmentID    platform.EnvironmentID
	RevisionID       platform.RevisionID
	Protected        bool
	Bindings         []definitionBinding
	Resources        []definitionResource
	Secrets          []definitionSecret
	Policy           api.DefinitionDNSPolicy
	ProviderRevision int64
	Credentials      []definitionCredential
	Nodes            []definitionNode
	ActiveOperations []definitionOperation
}

type definitionBinding struct {
	ID                platform.DomainID
	Name              string
	Primary           bool
	CheckPath         string
	Revision          int64
	ZoneID            string
	Generation        int64
	Provider          string
	ZoneName          string
	Ownership         string
	Origins           []string
	Proxied           bool
	State             string
	AppliedGeneration int64
	LastOperationID   string
}
type definitionCredential struct {
	ZoneID   string
	Revision int64
}
type definitionNode struct {
	Name                string
	Roles, Capabilities []string
	Status              string
	EnrollmentState     string
	Transport           string
	LastCheckState      string
}
type definitionOperation struct{ ID, Kind, Target, State string }
type definitionResource struct {
	ID          string
	Name        string
	Node        string
	Spec        json.RawMessage
	State       string
	OperationID string
}
type definitionSecret struct {
	Name     string
	Revision int64
	State    string
}

// Only compare non-secret metadata and report a bounded category. Several
// categories can change concurrently; this is the first observed difference.
func definitionSnapshotChange(a, b definitionSnapshot) string {
	switch {
	case !reflect.DeepEqual(a.Workspace, b.Workspace), a.AppID != b.AppID, a.ImageRepository != b.ImageRepository,
		a.EnvironmentID != b.EnvironmentID, a.RevisionID != b.RevisionID, a.Protected != b.Protected:
		return "definition workspace, target or revision changed"
	case !reflect.DeepEqual(a.Bindings, b.Bindings), !reflect.DeepEqual(a.Resources, b.Resources), !reflect.DeepEqual(a.Secrets, b.Secrets):
		return "definition binding, resource or secret metadata changed"
	case !reflect.DeepEqual(a.Policy, b.Policy), a.ProviderRevision != b.ProviderRevision, !reflect.DeepEqual(a.Credentials, b.Credentials):
		return "definition DNS policy or credential changed"
	case !reflect.DeepEqual(a.Nodes, b.Nodes):
		return "definition placement changed"
	case !reflect.DeepEqual(a.ActiveOperations, b.ActiveOperations):
		return "definition relevant operations changed"
	default:
		return "definition snapshot changed"
	}
}

func (s *Service) readDefinitionSnapshot(ctx context.Context, r *appdef.Resolved) (definitionSnapshot, platform.Environment, error) {
	var snap definitionSnapshot
	var env platform.Environment
	err := s.entities.WithinReadTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		snap, env, err = readDefinitionSnapshotTx(ctx, tx, r)
		return err
	})
	if err != nil {
		return snap, env, unavailable(err)
	}
	return snap, env, nil
}

// readDefinitionSnapshotTx also permits admission to reread the exact same
// material under its own transaction and locks.
func readDefinitionSnapshotTx(ctx context.Context, tx *entities.StoreTx, r *appdef.Resolved) (definitionSnapshot, platform.Environment, error) {
	var snap definitionSnapshot
	var env platform.Environment
	if err := readDefinitionSnapshotWorkspaceTx(ctx, tx, r, &snap, &env); err != nil {
		return snap, env, err
	}
	if err := readDefinitionSnapshotFactsTx(ctx, tx, r, &snap); err != nil {
		return snap, env, err
	}
	if err := readDefinitionSnapshotDNSTx(ctx, tx, &snap); err != nil {
		return snap, env, err
	}
	if err := readDefinitionSnapshotPlacementTx(ctx, tx, r, env, &snap); err != nil {
		return snap, env, err
	}
	if err := readDefinitionSnapshotActiveTx(ctx, tx, r, &snap); err != nil {
		return snap, env, err
	}
	return snap, env, nil
}

func readDefinitionSnapshotWorkspaceTx(ctx context.Context, tx *entities.StoreTx, r *appdef.Resolved, snap *definitionSnapshot, env *platform.Environment) error {
	var err error
	if snap.Workspace, err = readWorkspaceSchema(ctx, tx.Workspace()); err != nil {
		return unavailable(err)
	}
	if !snap.Workspace.Configured {
		return contract.NewFault(contract.PreconditionFailed, "workspace is not configured", "Initialize the workspace before planning a definition.")
	}
	repository, found, err := tx.Platform().Apps().Repository(ctx, r.App)
	if err != nil {
		return unavailable(err)
	}
	if !found {
		return nil
	}
	snap.AppID, snap.ImageRepository = repository.ID, repository.ImageRepository
	observation, found, err := tx.Platform().Environments().GetByName(ctx, r.App, r.Environment)
	if err != nil {
		return unavailable(err)
	}
	if found {
		*env = observation.Environment
		snap.EnvironmentID, snap.RevisionID, snap.Protected = env.ID, env.RevisionID, env.Protected
	}
	return nil
}

func readDefinitionSnapshotFactsTx(ctx context.Context, tx *entities.StoreTx, r *appdef.Resolved, snap *definitionSnapshot) error {
	bindings, err := tx.Platform().Domains().DefinitionSnapshot(ctx, snap.EnvironmentID)
	if err != nil {
		return unavailable(err)
	}
	for _, binding := range bindings {
		snap.Bindings = append(snap.Bindings, definitionBinding{
			ID: binding.ID, Name: binding.Domain, Primary: binding.Primary,
			CheckPath: binding.CheckPath, Revision: binding.DesiredRevision,
			ZoneID: binding.ZoneID, Generation: binding.Generation, Provider: binding.Provider,
			ZoneName: binding.ZoneName, Ownership: binding.OwnershipMarker, Origins: binding.DesiredOrigins,
			Proxied: binding.DesiredProxied, State: binding.State,
			AppliedGeneration: binding.AppliedGeneration, LastOperationID: string(binding.LastOperationID),
		})
	}
	resources, err := tx.Resources().DefinitionSnapshot(ctx, snap.EnvironmentID)
	if err != nil {
		return unavailable(err)
	}
	for _, resource := range resources {
		snap.Resources = append(snap.Resources, definitionResource{
			ID: string(resource.ID), Name: resource.Name, Node: resource.Node,
			Spec: resource.Spec, State: string(resource.State), OperationID: string(resource.OperationID),
		})
	}
	secrets, err := tx.Secrets().DefinitionSnapshot(ctx, r.App, r.Environment)
	if err != nil {
		return unavailable(err)
	}
	for _, secret := range secrets {
		if secret.Revision > math.MaxInt64 {
			return definitionSnapshotRevisionFault()
		}
		snap.Secrets = append(snap.Secrets, definitionSecret{
			Name: secret.Name, Revision: int64(secret.Revision), State: secret.State,
		})
	}
	return nil
}

func readDefinitionSnapshotDNSTx(ctx context.Context, tx *entities.StoreTx, snap *definitionSnapshot) error {
	var err error
	if snap.Policy, err = readDefinitionDNSPolicy(ctx, tx); err != nil {
		return err
	}
	if snap.Policy.Enabled {
		revision, found, err := tx.Providers().RevisionForZone(ctx, "cloudflare", snap.Policy.ZoneID)
		if err != nil || !found {
			return unavailable(err)
		}
		if revision > math.MaxInt64 {
			return definitionSnapshotRevisionFault()
		}
		snap.ProviderRevision = int64(revision)
	}
	credentials, err := tx.Providers().ZoneRevisionsForDefinition(ctx, "cloudflare", snap.Policy.ZoneID, string(snap.EnvironmentID))
	if err != nil {
		return unavailable(err)
	}
	for _, credential := range credentials {
		if credential.Revision > math.MaxInt64 {
			return definitionSnapshotRevisionFault()
		}
		snap.Credentials = append(snap.Credentials, definitionCredential{ZoneID: credential.ZoneID, Revision: int64(credential.Revision)})
	}
	return nil
}

func readDefinitionSnapshotPlacementTx(ctx context.Context, tx *entities.StoreTx, r *appdef.Resolved, env platform.Environment, snap *definitionSnapshot) error {
	// Updates select nodes by exact name, not by discovery over the node table.
	// Resources run on the current environment node before the revision update
	// and retain their existing node identity. Include both current and proposed
	// runtime/build nodes, as well as every existing resource node.
	placement := map[string]bool{}
	for _, name := range []string{env.Node, env.BuildNodeName(), r.Node, r.BuildNode} {
		if name != "" {
			placement[name] = true
		}
	}
	for _, resource := range snap.Resources {
		if resource.Node != "" {
			placement[resource.Node] = true
		}
	}
	names := make([]string, 0, len(placement))
	for name := range placement {
		names = append(names, name)
	}
	sort.Strings(names)
	nodes, err := tx.Nodes().DefinitionSnapshot(ctx, names)
	if err != nil {
		return unavailable(err)
	}
	for _, node := range nodes {
		sort.Strings(node.Roles)
		sort.Strings(node.Capabilities)
		snap.Nodes = append(snap.Nodes, definitionNode{
			Name: node.Name, Roles: node.Roles, Capabilities: node.Capabilities,
			Status: string(node.Status), EnrollmentState: string(node.Enrollment),
			Transport: node.Transport, LastCheckState: node.LastCheckState,
		})
	}
	return nil
}

func readDefinitionSnapshotActiveTx(ctx context.Context, tx *entities.StoreTx, r *appdef.Resolved, snap *definitionSnapshot) error {
	// Unrelated work cannot affect this application's definition. Keep all
	// same-app environment/resource work and builds, including other environments,
	// since admission fences the whole application.
	activeOperations, err := tx.Operations().DefinitionActiveOperations(ctx, snap.AppID,
		"app/"+r.App, "env/"+r.App+"/", "resource/"+r.App+"/")
	if err != nil {
		return unavailable(err)
	}
	for _, operation := range activeOperations {
		snap.ActiveOperations = append(snap.ActiveOperations, definitionOperation{
			ID: string(operation.ID), Kind: operation.Kind, Target: operation.Target, State: string(operation.State),
		})
	}
	return nil
}

// PlanDefinition has no durable side effects. It is deliberately separate
// from generic platform submission and never follows source URLs.
func (s *Service) PlanDefinition(ctx context.Context, actor contract.Actor, input api.DefinitionPlanInput) (api.DefinitionPlan, error) {
	return s.planDefinition(ctx, actor, input, nil)
}

type definitionPlanningMaterial struct {
	Input       api.DefinitionPlanInput
	Snapshot    definitionSnapshot
	Environment platform.Environment
	Resolved    *appdef.Resolved
	Steps       []definitionWorkStep
}

func definitionSnapshotRevisionFault() *contract.FaultError {
	return contract.NewFault(contract.Unavailable, "definition snapshot revision is outside the supported range", "Repair the stored revision metadata before planning a definition.")
}

func (s *Service) planDefinition(ctx context.Context, actor contract.Actor, input api.DefinitionPlanInput, material *definitionPlanningMaterial) (api.DefinitionPlan, error) {
	var p api.DefinitionPlan
	input, r, err := prepareDefinitionPlanInput(actor, input)
	if err != nil {
		return p, err
	}
	if s == nil || s.entities == nil {
		return p, unavailable()
	}
	snap, env, err := s.readDefinitionSnapshot(ctx, r)
	if err != nil {
		return p, err
	}
	p = definitionPlanHeader(input, snap, r)
	definitionPlanPlacementBlockers(&p, snap, r)
	current, byName := definitionPlanBindings(snap, r)
	definitionPlanEnvironment(&p, r, env, current)
	resources, resourceErr := r.ResourceApplySpecs()
	if resourceErr != nil {
		p.Blockers = append(p.Blockers, resourceErr.Error())
	}
	definitionPlanSecrets(&p, r, snap, env, resources)
	definitionPlanDomains(&p, r, snap, input.PlanNonce, byName)
	if err := s.definitionPlanDNSCleanup(ctx, actor, &p, byName); err != nil {
		return api.DefinitionPlan{}, err
	}
	if err := s.definitionPlanDNSApply(ctx, actor, &p, byName, snap); err != nil {
		return api.DefinitionPlan{}, err
	}
	if input.Deploy {
		p.Blockers = append(p.Blockers, "definition updates do not deploy; deploy separately after prerequisites")
	}
	compiled := definitionPlanWork(&p, input, r, snap, env)
	if p.ConfirmTarget != "" {
		p.Diff = append(p.Diff, "removal requires exact confirmation: "+p.ConfirmTarget)
	}
	p.Warnings = append(p.Warnings, "This read-only preview does not authorize provider writes or prove route, TLS, or public readiness.")
	// Exact input bytes preserve every TOML presence distinction, including
	// unexported parser presence bits. Whitespace changes require a new plan.
	digest, err := definitionPlanDigest(input, snap, p)
	if err != nil {
		return api.DefinitionPlan{}, contract.NewFault(contract.Internal, "cannot encode definition plan", "Report the binary version.")
	}
	p.Digest = digest
	p.CanApply = len(p.Blockers) == 0
	if material != nil {
		*material = definitionPlanningMaterial{Input: input, Snapshot: snap, Environment: env, Resolved: r, Steps: compiled}
	}
	return p, nil
}

func prepareDefinitionPlanInput(actor contract.Actor, input api.DefinitionPlanInput) (api.DefinitionPlanInput, *appdef.Resolved, error) {
	if err := requireCloudflareOperator(actor); err != nil {
		return input, nil, err
	}
	if input.PlanNonce == "" {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return input, nil, unavailable()
		}
		input.PlanNonce = hex.EncodeToString(nonce[:])
	} else if decoded, err := hex.DecodeString(input.PlanNonce); err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != input.PlanNonce {
		return input, nil, contract.NewFault(contract.ValidationFailed, "invalid definition plan nonce", "Use the nonce returned by the plan.")
	}
	r, err := appdef.ResolveSelection([]byte(input.Definition), input.App, input.Environment)
	if err != nil {
		return input, nil, contract.NewFault(contract.ValidationFailed, "invalid app definition", err.Error())
	}
	return input, r, nil
}

func definitionPlanHeader(input api.DefinitionPlanInput, snap definitionSnapshot, r *appdef.Resolved) api.DefinitionPlan {
	return api.DefinitionPlan{PlanNonce: input.PlanNonce, WorkspaceID: snap.Workspace.ID, App: r.App, Environment: r.Environment, AppExists: snap.AppID != "", EnvironmentExists: snap.EnvironmentID != "", CurrentRevision: snap.RevisionID, Changes: []string{}, Diff: []string{}, Domains: []api.DefinitionDomainChange{}, DNS: []integrations.DefinitionDNSPlan{}, DNSCleanup: []integrations.DomainDNSPlan{}, Blockers: []string{}, Warnings: []string{}}
}

func definitionPlanPlacementBlockers(p *api.DefinitionPlan, snap definitionSnapshot, r *appdef.Resolved) {
	knownNodes := make(map[string]bool, len(snap.Nodes))
	for _, node := range snap.Nodes {
		knownNodes[node.Name] = true
	}
	for _, name := range []string{r.Node, r.BuildNode} {
		if name != "" && !knownNodes[name] {
			p.Blockers = append(p.Blockers, "placement node is not registered: "+name)
		}
	}
	if snap.AppID != "" && r.ImageRepository != "" && r.ImageRepository != snap.ImageRepository {
		p.Blockers = append(p.Blockers, fmt.Sprintf("image_repository: definition value %q does not match the app's repository %q", r.ImageRepository, snap.ImageRepository))
	}
}

func definitionPlanBindings(snap definitionSnapshot, r *appdef.Resolved) ([]platform.DomainBinding, map[string]definitionBinding) {
	current := make([]platform.DomainBinding, 0, len(snap.Bindings))
	byName := map[string]definitionBinding{}
	for _, b := range snap.Bindings {
		byName[b.Name] = b
		current = append(current, platform.DomainBinding{ID: b.ID, Domain: b.Name, App: r.App, Environment: r.Environment, Primary: b.Primary, CheckPath: b.CheckPath})
	}
	return current, byName
}

func definitionPlanEnvironment(p *api.DefinitionPlan, r *appdef.Resolved, env platform.Environment, current []platform.DomainBinding) {
	if p.EnvironmentExists {
		_, changes, diff, err := r.UpdatePatch(env, current)
		if err != nil {
			p.Blockers = append(p.Blockers, err.Error())
			return
		}
		p.Changes = append(p.Changes, changes...)
		p.Diff = append(p.Diff, diff...)
		return
	}
	if !p.AppExists {
		p.Blockers = append(p.Blockers, "definition apply requires an existing app")
	} else {
		p.Blockers = append(p.Blockers, "definition apply requires an existing environment")
	}
	_, err := r.CreateSpec(nil)
	if err != nil {
		p.Blockers = append(p.Blockers, err.Error())
	}
	if !p.AppExists {
		p.Changes = append(p.Changes, "app.create")
	}
	p.Changes = append(p.Changes, "environment.create")
}

func definitionPlanSecrets(p *api.DefinitionPlan, r *appdef.Resolved, snap definitionSnapshot, env platform.Environment, resources []platform.ResourceApplyInput) {
	secretByName := map[string]definitionSecret{}
	for _, v := range snap.Secrets {
		secretByName[v.Name] = v
	}
	needed := map[string]bool{}
	for _, name := range r.SecretNames {
		needed[name] = true
	}
	if r.Source != nil && r.Source.CredentialSecret != nil && *r.Source.CredentialSecret != "" {
		needed[*r.Source.CredentialSecret] = true
	}
	for _, v := range resources {
		for _, name := range v.SecretEnv {
			needed[name] = true
		}
	}
	autoDeploy, webhookSecret := definitionPlanAutomation(env, r)
	if autoDeploy && webhookSecret != "" {
		needed[webhookSecret] = true
	}
	names := make([]string, 0, len(needed))
	for name := range needed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v, ok := secretByName[name]; !ok || v.State != "active" || v.Revision == 0 {
			p.Blockers = append(p.Blockers, "missing or archived secret: "+name)
		}
	}
}

func definitionPlanAutomation(env platform.Environment, r *appdef.Resolved) (bool, string) {
	autoDeploy, webhookSecret := false, ""
	if env.Revision != nil {
		autoDeploy, webhookSecret = env.Revision.AutoDeploy, env.Revision.WebhookSecretName
	}
	if r.Automation != nil {
		if r.Automation.AutoDeploy != nil {
			autoDeploy = *r.Automation.AutoDeploy
		}
		if r.Automation.WebhookSecretName != nil {
			webhookSecret = *r.Automation.WebhookSecretName
		}
	}
	return autoDeploy, webhookSecret
}

func definitionPlanDomains(p *api.DefinitionPlan, r *appdef.Resolved, snap definitionSnapshot, nonce string, byName map[string]definitionBinding) {
	want := map[string]bool{}
	domains := r.Domains
	if !r.DomainsSet {
		for _, b := range snap.Bindings {
			domains = append(domains, appdef.Domain{Name: b.Name, Primary: b.Primary, CheckPath: b.CheckPath})
		}
	}
	for _, d := range domains {
		want[d.Name] = true
		b, exists := byName[d.Name]
		action := "unchanged"
		if !exists {
			action = "add"
			b.ID = definitionPlannedDomainID(snap.Workspace.ID, r.App, r.Environment, nonce, d.Name)
		} else if b.Primary != d.Primary || b.CheckPath != d.CheckPath {
			action = "update"
		}
		p.Domains = append(p.Domains, api.DefinitionDomainChange{Action: action, Name: d.Name, ID: b.ID, Primary: d.Primary, CheckPath: d.CheckPath})
	}
	for _, b := range snap.Bindings {
		if !want[b.Name] {
			p.Domains = append(p.Domains, api.DefinitionDomainChange{Action: "remove", Name: b.Name, ID: b.ID, Primary: b.Primary, CheckPath: b.CheckPath})
			p.ConfirmTarget = r.App + "/" + r.Environment
			p.Warnings = append(p.Warnings, "removal requires explicit confirmation: "+b.Name)
		}
	}
	if r.DomainsSet && len(domains) == 0 && len(snap.Bindings) > 0 {
		p.Blockers = append(p.Blockers, "definition cannot remove every domain binding")
	}
}

// definitionPlanDNSCleanup belongs to the existing binding ledger even if the
// current installation-wide policy has since been disabled or changed zones.
func (s *Service) definitionPlanDNSCleanup(ctx context.Context, actor contract.Actor, p *api.DefinitionPlan, byName map[string]definitionBinding) error {
	for _, d := range p.Domains {
		b := byName[d.Name]
		if d.Action != "remove" || b.ZoneID == "" {
			continue
		}
		if s.domainDNS == nil {
			p.Blockers = append(p.Blockers, "Cloudflare DNS planner is unavailable")
			break
		}
		if err := s.domainDNS.AuthorizeRead(ctx, actor); err != nil {
			return err
		}
		cleanup, err := s.domainDNS.PlanRemove(ctx, integrations.DomainDNSRequest{ZoneID: b.ZoneID, ZoneName: b.ZoneName, DomainID: string(d.ID), Ownership: b.Ownership, Host: d.Name})
		if err != nil {
			return err
		}
		cleanup.ObservedAt = time.Time{}
		p.DNSCleanup = append(p.DNSCleanup, cleanup)
	}
	return nil
}

func (s *Service) definitionPlanDNSApply(ctx context.Context, actor contract.Actor, p *api.DefinitionPlan, byName map[string]definitionBinding, snap definitionSnapshot) error {
	if !snap.Policy.Enabled {
		p.Warnings = append(p.Warnings, "Definition DNS policy is disabled; DNS must be managed manually.")
		return nil
	}
	for i := range p.Domains {
		d := &p.Domains[i]
		if d.Action == "remove" {
			continue
		}
		if b := byName[d.Name]; b.ZoneID != "" && b.ZoneID != snap.Policy.ZoneID {
			p.Blockers = append(p.Blockers, "managed binding uses a different DNS zone: "+d.Name)
			continue
		}
		if s.domainDNS == nil {
			p.Blockers = append(p.Blockers, "Cloudflare DNS planner is unavailable")
			break
		}
		req := integrations.DomainDNSRequest{ZoneID: snap.Policy.ZoneID, ZoneName: snap.Policy.ZoneName, DomainID: string(d.ID), Ownership: integrations.DomainDNSOwnership(string(d.ID), snap.Policy.ZoneID), Host: d.Name, Addresses: snap.Policy.Origins, Proxied: snap.Policy.ProxyMode == "proxied"}
		plan, err := s.domainDNS.PlanDefinitionApply(ctx, actor, req)
		if err != nil {
			return err
		}
		p.DNS = append(p.DNS, plan)
	}
	return nil
}

func definitionPlanWork(p *api.DefinitionPlan, input api.DefinitionPlanInput, r *appdef.Resolved, snap definitionSnapshot, env platform.Environment) []definitionWorkStep {
	if !p.EnvironmentExists {
		return nil
	}
	steps, err := compileDefinitionWork(input, r, snap, env, p)
	if err != nil {
		p.Blockers = append(p.Blockers, err.Error())
		return nil
	}
	p.Diff = append(p.Diff, fmt.Sprintf("worklist: %d steps", len(steps)))
	for _, step := range steps {
		if step.Kind == platform.KindResourceApply {
			p.Changes = append(p.Changes, "resource.apply:"+strings.TrimPrefix(step.Target, "resource/"+r.App+"/"+r.Environment+"/"))
		}
		if step.DNSPlan != nil || step.DNSCleanup != nil {
			p.Diff = append(p.Diff, "DNS child: "+step.Target+" "+string(step.DomainID))
		}
	}
	return steps
}

// definitionWorkStep is the secret-free, normalized action and its approved
// provider snapshot. AllocatedDomainID is set only for a new binding; later
// admission must reserve it and consume the nonce atomically.
type definitionWorkStep struct {
	Kind              string                          `json:"kind"`
	Target            string                          `json:"target"`
	Input             json.RawMessage                 `json:"input"`
	DomainID          platform.DomainID               `json:"domain_id,omitempty"`
	AllocatedDomainID platform.DomainID               `json:"allocated_domain_id,omitempty"`
	DNSPlan           *integrations.DefinitionDNSPlan `json:"dns_plan,omitempty"`
	DNSCleanup        *integrations.DomainDNSPlan     `json:"dns_cleanup,omitempty"`
}

// compileDefinitionWork builds the frozen, ordered work for an existing target.
// It never deploys or destroys resources omitted from the revision.
func compileDefinitionWork(input api.DefinitionPlanInput, r *appdef.Resolved, snap definitionSnapshot, env platform.Environment, plan *api.DefinitionPlan) ([]definitionWorkStep, error) {
	if snap.AppID == "" || snap.EnvironmentID == "" {
		return nil, fmt.Errorf("definition work requires an existing app and environment")
	}
	if input.Deploy {
		return nil, fmt.Errorf("definition updates do not deploy")
	}
	if env.Revision == nil {
		return nil, fmt.Errorf("definition work requires current revision inventory")
	}
	target := "env/" + r.App + "/" + r.Environment
	steps := []definitionWorkStep{}
	resourceSteps, err := compileDefinitionResourceSteps(r, snap)
	if err != nil {
		return nil, err
	}
	steps = append(steps, resourceSteps...)
	current := make([]platform.DomainBinding, 0, len(snap.Bindings))
	for _, b := range snap.Bindings {
		current = append(current, platform.DomainBinding{ID: b.ID, Domain: b.Name, App: r.App, Environment: r.Environment, Primary: b.Primary, CheckPath: b.CheckPath})
	}
	patch, changes, _, err := r.UpdatePatch(env, current)
	if err != nil {
		return nil, err
	}
	base := env.Revision.RevisionState
	if err := platform.NormalizeRevisionState(&base); err != nil {
		return nil, err
	}
	desired := platform.ApplyEnvironmentPatch(base, patch)
	platform.ResolveWebhookSecretName(&desired)
	if platform.EnvironmentUpdateRemovesState(base, desired) {
		patch.Confirm = r.App + "/" + r.Environment
		plan.ConfirmTarget = patch.Confirm
	}
	if len(changes) != 0 {
		step, err := newDefinitionWorkStep(platform.KindEnvironmentUpdate, target, patch)
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	// Primary first makes promotion safe before old aliases are removed.
	domainSteps, err := compileDefinitionDomainSteps(input, r, snap, plan, target)
	if err != nil {
		return nil, err
	}
	steps = append(steps, domainSteps...)
	return steps, nil
}

func compileDefinitionResourceSteps(r *appdef.Resolved, snap definitionSnapshot) ([]definitionWorkStep, error) {
	resources, err := r.ResourceApplySpecs()
	if err != nil {
		return nil, err
	}
	steps := []definitionWorkStep{}
	for _, resource := range resources {
		rt := "resource/" + r.App + "/" + r.Environment + "/" + resource.Name
		raw, err := json.Marshal(resource)
		if err != nil {
			return nil, err
		}
		n, err := platform.Default().Normalize(platform.KindResourceApply, rt, raw)
		if err != nil {
			return nil, err
		}
		canonical, err := typedOperationInput[*platform.ResourceApplyInput](n.Input)
		if err != nil {
			return nil, err
		}
		if !definitionResourceChanged(resource.Name, canonical.ResourceSpec, snap) {
			continue
		}
		steps = append(steps, definitionWorkStep{Kind: n.Kind, Target: rt, Input: n.Canonical})
	}
	return steps, nil
}

func definitionResourceChanged(name string, desired platform.ResourceSpec, snap definitionSnapshot) bool {
	for _, previous := range snap.Resources {
		if previous.Name != name {
			continue
		}
		var spec platform.ResourceSpec
		if json.Unmarshal(previous.Spec, &spec) != nil {
			return true
		}
		normalized, err := platform.NormalizeResourceSpec(spec)
		return err != nil || !reflect.DeepEqual(normalized, desired) || previous.State != "ready"
	}
	return true
}

func compileDefinitionDomainSteps(input api.DefinitionPlanInput, r *appdef.Resolved, snap definitionSnapshot, plan *api.DefinitionPlan, target string) ([]definitionWorkStep, error) {
	steps := []definitionWorkStep{}
	for _, d := range definitionOrderedDomains(plan.Domains) {
		step, include, err := compileDefinitionDomainStep(input, r, snap, plan, target, d)
		if err != nil {
			return nil, err
		}
		if include {
			steps = append(steps, step)
		}
	}
	return steps, nil
}

func compileDefinitionDomainStep(input api.DefinitionPlanInput, r *appdef.Resolved, snap definitionSnapshot, plan *api.DefinitionPlan, target string, d api.DefinitionDomainChange) (definitionWorkStep, bool, error) {
	id := d.ID
	if d.Action == "add" {
		id = definitionPlannedDomainID(snap.Workspace.ID, r.App, r.Environment, input.PlanNonce, d.Name)
		if id != d.ID {
			return definitionWorkStep{}, false, fmt.Errorf("planned domain identity mismatch: %s", d.Name)
		}
	}
	dns, err := definitionDNSPlanForDomain(plan.DNS, d.Name, id)
	if err != nil {
		return definitionWorkStep{}, false, err
	}
	cleanup, err := definitionDNSCleanupForDomain(plan.DNSCleanup, d.Name, id)
	if err != nil {
		return definitionWorkStep{}, false, err
	}
	if d.Action != "remove" && snap.Policy.Enabled && dns == nil {
		return definitionWorkStep{}, false, fmt.Errorf("missing DNS plan: %s", d.Name)
	}
	if d.Action == "remove" && definitionDomainCleanupMissing(id, cleanup, snap) {
		return definitionWorkStep{}, false, fmt.Errorf("missing DNS cleanup plan: %s", d.Name)
	}
	if d.Action == "unchanged" {
		if dns == nil || definitionDNSAlreadyConverged(id, dns, snap) {
			return definitionWorkStep{}, false, nil
		}
	}
	step, err := definitionDomainWorkStep(d, id, dns, cleanup, snap, target)
	if err != nil {
		return definitionWorkStep{}, false, err
	}
	return step, true, nil
}

func definitionDomainCleanupMissing(id platform.DomainID, cleanup *integrations.DomainDNSPlan, snap definitionSnapshot) bool {
	for _, b := range snap.Bindings {
		if b.ID == id && b.ZoneID != "" && cleanup == nil {
			return true
		}
	}
	return false
}

func definitionOrderedDomains(domains []api.DefinitionDomainChange) []api.DefinitionDomainChange {
	ordered := append([]api.DefinitionDomainChange(nil), domains...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return definitionDomainRank(ordered[i]) < definitionDomainRank(ordered[j])
	})
	return ordered
}

func definitionDomainRank(d api.DefinitionDomainChange) int {
	if d.Action == "remove" {
		return 2
	}
	if d.Primary {
		return 0
	}
	return 1
}

func definitionDNSPlanForDomain(plans []integrations.DefinitionDNSPlan, host string, id platform.DomainID) (*integrations.DefinitionDNSPlan, error) {
	var dns *integrations.DefinitionDNSPlan
	for i := range plans {
		if plans[i].Host != host {
			continue
		}
		if dns != nil || plans[i].DomainID != string(id) {
			return nil, fmt.Errorf("DNS plan identity mismatch: %s", host)
		}
		dns = &plans[i]
	}
	return dns, nil
}

func definitionDNSCleanupForDomain(plans []integrations.DomainDNSPlan, host string, id platform.DomainID) (*integrations.DomainDNSPlan, error) {
	var cleanup *integrations.DomainDNSPlan
	for i := range plans {
		if plans[i].Host != host {
			continue
		}
		if cleanup != nil || plans[i].DomainID != string(id) {
			return nil, fmt.Errorf("DNS cleanup identity mismatch: %s", host)
		}
		cleanup = &plans[i]
	}
	return cleanup, nil
}

func definitionDNSAlreadyConverged(id platform.DomainID, dns *integrations.DefinitionDNSPlan, snap definitionSnapshot) bool {
	for _, b := range snap.Bindings {
		if b.ID != id {
			continue
		}
		return len(dns.Changes) == 0 && b.Provider == "cloudflare" &&
			b.ZoneID == dns.ZoneID && b.ZoneName == dns.Zone && b.Ownership == dns.Ownership &&
			b.State == domainDNSStateApplied && b.Generation > 0 && b.AppliedGeneration == b.Generation &&
			reflect.DeepEqual(b.Origins, snap.Policy.Origins) && b.Proxied == (snap.Policy.ProxyMode == "proxied")
	}
	return false
}

func definitionDomainWorkStep(d api.DefinitionDomainChange, id platform.DomainID, dns *integrations.DefinitionDNSPlan, cleanup *integrations.DomainDNSPlan, snap definitionSnapshot, target string) (definitionWorkStep, error) {
	kind := platform.KindDomainAdd
	v := platform.DomainInput{Domain: d.Name, Primary: d.Primary, CheckPath: d.CheckPath}
	if d.Action == "remove" {
		kind = platform.KindDomainRemove
		v.Confirm = d.Name
	}
	if dns != nil {
		proxied := snap.Policy.ProxyMode == "proxied"
		v.DNSMode = "cloudflare"
		v.DNSZone = snap.Policy.ZoneID
		v.DNSOrigins = snap.Policy.Origins
		v.DNSProxied = &proxied
	}
	step, err := newDefinitionWorkStep(kind, target, v)
	if err != nil {
		return definitionWorkStep{}, err
	}
	step.DomainID, step.DNSPlan, step.DNSCleanup = id, dns, cleanup
	if d.Action == "add" {
		step.AllocatedDomainID = id
	}
	return step, nil
}

func newDefinitionWorkStep(kind, target string, value any) (definitionWorkStep, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return definitionWorkStep{}, err
	}
	n, err := platform.Default().Normalize(kind, target, raw)
	if err != nil {
		return definitionWorkStep{}, err
	}
	return definitionWorkStep{Kind: kind, Target: target, Input: n.Canonical}, nil
}

func definitionPlannedDomainID(workspace, app, environment, nonce, hostname string) platform.DomainID {
	// JSON framing prevents ambiguity between target components.
	encoded, _ := json.Marshal([]string{workspace, app, environment, nonce, hostname})
	h := sha256.Sum256(encoded)
	return platform.DomainID("dom_" + hex.EncodeToString(h[:16]))
}

func definitionPlanDigest(input api.DefinitionPlanInput, snap definitionSnapshot, p api.DefinitionPlan) (string, error) {
	p.Digest = ""
	p.CanApply = false
	canonical, err := json.Marshal(struct {
		Version  int
		Input    api.DefinitionPlanInput
		Snapshot definitionSnapshot
		Plan     api.DefinitionPlan
	}{1, input, snap, p})
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(canonical)
	return hex.EncodeToString(h[:]), nil
}

var _ api.DefinitionPlanningService = (*Service)(nil)

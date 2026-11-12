package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// definitionSnapshot is the timestamp-free database material to be compared
// under admission locks. Provider reads occur only after its transaction ends.
type definitionSnapshot struct {
	Workspace        WorkspaceIdentity
	AppID            platform.AppID
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
	Spec        json.RawMessage
	State       string
	OperationID string
}
type definitionSecret struct {
	Name     string
	Revision int64
	State    string
}

func (s *Service) readDefinitionSnapshot(ctx context.Context, app, environment string) (definitionSnapshot, platform.Environment, error) {
	var snap definitionSnapshot
	var env platform.Environment
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return snap, env, unavailable()
	}
	defer rollback(tx)
	snap, env, err = readDefinitionSnapshotTx(ctx, tx, app, environment)
	if err != nil {
		return snap, env, err
	}
	if err = tx.Commit(ctx); err != nil {
		return snap, env, unavailable()
	}
	return snap, env, nil
}

// readDefinitionSnapshotTx also permits admission to reread the exact same
// material under its own transaction and locks.
func readDefinitionSnapshotTx(ctx context.Context, tx pgx.Tx, app, environment string) (definitionSnapshot, platform.Environment, error) {
	var snap definitionSnapshot
	var env platform.Environment
	var err error
	if snap.Workspace, err = readWorkspace(ctx, tx); err != nil {
		return snap, env, unavailable()
	}
	if !snap.Workspace.Configured {
		return snap, env, contract.NewFault(contract.PreconditionFailed, "workspace is not configured", "Initialize the workspace before planning a definition.")
	}
	err = tx.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name=$1`, app).Scan(&snap.AppID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return snap, env, unavailable()
	}
	if snap.AppID != "" {
		env, err = scanEnvironmentRow(tx.QueryRow(ctx, environmentSelect+` WHERE a.name=$1 AND e.name=$2`, app, environment))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return snap, env, unavailable()
		}
		if err == nil {
			snap.EnvironmentID, snap.RevisionID, snap.Protected = env.ID, env.RevisionID, env.Protected
		}
	}
	rows, err := tx.Query(ctx, `SELECT d.id,d.name,d.is_primary,coalesce(d.check_path,''),d.desired_revision,coalesce(x.zone_id,''),coalesce(x.generation,0),coalesce(x.provider,''),coalesce(x.zone_name,''),coalesce(x.ownership_marker,''),coalesce(x.desired_origins,'{}'),coalesce(x.desired_proxied,false),coalesce(x.state,''),coalesce(x.applied_generation,0),coalesce(x.last_operation_id,'') FROM eregion_platform_domains d LEFT JOIN eregion_platform_domain_dns x ON x.domain_id=d.id WHERE d.environment_id=$1 ORDER BY d.name`, snap.EnvironmentID)
	if err != nil {
		return snap, env, unavailable()
	}
	for rows.Next() {
		var b definitionBinding
		if err = rows.Scan(&b.ID, &b.Name, &b.Primary, &b.CheckPath, &b.Revision, &b.ZoneID, &b.Generation, &b.Provider, &b.ZoneName, &b.Ownership, &b.Origins, &b.Proxied, &b.State, &b.AppliedGeneration, &b.LastOperationID); err != nil {
			break
		}
		snap.Bindings = append(snap.Bindings, b)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return snap, env, unavailable()
	}
	rows, err = tx.Query(ctx, `SELECT id,name,spec,state,coalesce(operation_id,'') FROM eregion_platform_resources WHERE environment_id=$1 ORDER BY name`, snap.EnvironmentID)
	if err != nil {
		return snap, env, unavailable()
	}
	for rows.Next() {
		var r definitionResource
		if err = rows.Scan(&r.ID, &r.Name, &r.Spec, &r.State, &r.OperationID); err != nil {
			break
		}
		snap.Resources = append(snap.Resources, r)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return snap, env, unavailable()
	}
	rows, err = tx.Query(ctx, `SELECT name,latest_revision,state FROM eregion_secrets WHERE app=$1 AND environment=$2 ORDER BY name`, app, environment)
	if err != nil {
		return snap, env, unavailable()
	}
	for rows.Next() {
		var v definitionSecret
		if err = rows.Scan(&v.Name, &v.Revision, &v.State); err != nil {
			break
		}
		snap.Secrets = append(snap.Secrets, v)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return snap, env, unavailable()
	}
	if snap.Policy, err = scanDefinitionDNSPolicy(tx.QueryRow(ctx, definitionDNSPolicySelect)); err != nil {
		return snap, env, err
	}
	if snap.Policy.Enabled {
		err = tx.QueryRow(ctx, `SELECT latest_revision FROM eregion_provider_credentials WHERE provider='cloudflare' AND zone_id=$1`, snap.Policy.ZoneID).Scan(&snap.ProviderRevision)
		if err != nil {
			return snap, env, unavailable()
		}
	}
	rows, err = tx.Query(ctx, `SELECT zone_id,latest_revision FROM eregion_provider_credentials WHERE provider='cloudflare' AND (zone_id=$1 OR zone_id IN (SELECT x.zone_id FROM eregion_platform_domain_dns x JOIN eregion_platform_domains d ON d.id=x.domain_id WHERE d.environment_id=$2)) ORDER BY zone_id`, snap.Policy.ZoneID, snap.EnvironmentID)
	if err != nil {
		return snap, env, unavailable()
	}
	for rows.Next() {
		var c definitionCredential
		if err = rows.Scan(&c.ZoneID, &c.Revision); err != nil {
			break
		}
		snap.Credentials = append(snap.Credentials, c)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return snap, env, unavailable()
	}
	// Capture all placement candidates and unfinished operations: admission will
	// decide the precise conflict scope under locks, without weakening the pin.
	rows, err = tx.Query(ctx, `SELECT name,roles,capabilities,status,enrollment_state,transport,last_check_state FROM eregion_platform_nodes ORDER BY name`)
	if err != nil {
		return snap, env, unavailable()
	}
	for rows.Next() {
		var n definitionNode
		if err = rows.Scan(&n.Name, &n.Roles, &n.Capabilities, &n.Status, &n.EnrollmentState, &n.Transport, &n.LastCheckState); err != nil {
			break
		}
		snap.Nodes = append(snap.Nodes, n)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return snap, env, unavailable()
	}
	rows, err = tx.Query(ctx, `SELECT id,kind,target,state FROM eregion_operations WHERE state IN ('queued','running','cancelling') ORDER BY id`)
	if err != nil {
		return snap, env, unavailable()
	}
	for rows.Next() {
		var o definitionOperation
		if err = rows.Scan(&o.ID, &o.Kind, &o.Target, &o.State); err != nil {
			break
		}
		snap.ActiveOperations = append(snap.ActiveOperations, o)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return snap, env, unavailable()
	}
	return snap, env, nil
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

func (s *Service) planDefinition(ctx context.Context, actor contract.Actor, input api.DefinitionPlanInput, material *definitionPlanningMaterial) (api.DefinitionPlan, error) {
	var p api.DefinitionPlan
	if err := requireCloudflareOperator(actor); err != nil {
		return p, err
	}
	if input.PlanNonce == "" {
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return p, unavailable()
		}
		input.PlanNonce = hex.EncodeToString(nonce[:])
	} else if decoded, err := hex.DecodeString(input.PlanNonce); err != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != input.PlanNonce {
		return p, contract.NewFault(contract.ValidationFailed, "invalid definition plan nonce", "Use the nonce returned by the plan.")
	}
	r, err := appdef.ResolveSelection([]byte(input.Definition), input.App, input.Environment)
	if err != nil {
		return p, contract.NewFault(contract.ValidationFailed, "invalid app definition", err.Error())
	}
	if s == nil || s.pool == nil {
		return p, unavailable()
	}
	snap, env, err := s.readDefinitionSnapshot(ctx, r.App, r.Environment)
	if err != nil {
		return p, err
	}
	p = api.DefinitionPlan{PlanNonce: input.PlanNonce, WorkspaceID: snap.Workspace.ID, App: r.App, Environment: r.Environment, AppExists: snap.AppID != "", EnvironmentExists: snap.EnvironmentID != "", CurrentRevision: snap.RevisionID, Changes: []string{}, Diff: []string{}, Domains: []api.DefinitionDomainChange{}, DNS: []integrations.DefinitionDNSPlan{}, DNSCleanup: []integrations.DomainDNSPlan{}, Blockers: []string{}, Warnings: []string{}}
	current := make([]platform.DomainBinding, 0, len(snap.Bindings))
	byName := map[string]definitionBinding{}
	for _, b := range snap.Bindings {
		byName[b.Name] = b
		current = append(current, platform.DomainBinding{ID: b.ID, Domain: b.Name, App: r.App, Environment: r.Environment, Primary: b.Primary, CheckPath: b.CheckPath})
	}
	var compiled []definitionWorkStep
	if p.EnvironmentExists {
		_, changes, diff, e := r.UpdatePatch(env, current)
		if e != nil {
			p.Blockers = append(p.Blockers, e.Error())
		} else {
			p.Changes = append(p.Changes, changes...)
			p.Diff = append(p.Diff, diff...)
		}
	} else {
		if !p.AppExists {
			p.Blockers = append(p.Blockers, "definition apply requires an existing app")
		} else {
			p.Blockers = append(p.Blockers, "definition apply requires an existing environment")
		}
		_, e := r.CreateSpec(nil)
		if e != nil {
			p.Blockers = append(p.Blockers, e.Error())
		}
		if !p.AppExists {
			p.Changes = append(p.Changes, "app.create")
		}
		p.Changes = append(p.Changes, "environment.create")
	}
	resources, e := r.ResourceApplySpecs()
	if e != nil {
		p.Blockers = append(p.Blockers, e.Error())
	}
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
	if autoDeploy && webhookSecret != "" {
		needed[webhookSecret] = true
	}
	var names []string
	for name := range needed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if v, ok := secretByName[name]; !ok || v.State != "active" || v.Revision == 0 {
			p.Blockers = append(p.Blockers, "missing or archived secret: "+name)
		}
	}
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
			b.ID = definitionPlannedDomainID(snap.Workspace.ID, r.App, r.Environment, input.PlanNonce, d.Name)
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
	// Managed cleanup belongs to the existing binding ledger even if the current
	// installation-wide policy has since been disabled or changed zones.
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
			return api.DefinitionPlan{}, err
		}
		cleanup, err := s.domainDNS.PlanRemove(ctx, integrations.DomainDNSRequest{ZoneID: b.ZoneID, ZoneName: b.ZoneName, DomainID: string(d.ID), Ownership: b.Ownership, Host: d.Name})
		if err != nil {
			return api.DefinitionPlan{}, err
		}
		cleanup.ObservedAt = time.Time{}
		p.DNSCleanup = append(p.DNSCleanup, cleanup)
	}
	if !snap.Policy.Enabled {
		p.Warnings = append(p.Warnings, "Definition DNS policy is disabled; DNS must be managed manually.")
	} else {
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
			plan, e := s.domainDNS.PlanDefinitionApply(ctx, actor, req)
			if e != nil {
				return api.DefinitionPlan{}, e
			}
			p.DNS = append(p.DNS, plan)
		}
	}
	if input.Deploy {
		p.Blockers = append(p.Blockers, "definition updates do not deploy; deploy separately after prerequisites")
	}
	if p.EnvironmentExists {
		steps, e := compileDefinitionWork(input, r, snap, env, &p)
		if e != nil {
			p.Blockers = append(p.Blockers, e.Error())
		} else {
			compiled = steps
			p.Diff = append(p.Diff, fmt.Sprintf("worklist: %d steps", len(steps)))
			for _, step := range steps {
				if step.Kind == platform.KindResourceApply {
					p.Changes = append(p.Changes, "resource.apply:"+strings.TrimPrefix(step.Target, "resource/"+r.App+"/"+r.Environment+"/"))
				}
				if step.DNSPlan != nil || step.DNSCleanup != nil {
					p.Diff = append(p.Diff, "DNS child: "+step.Target+" "+string(step.DomainID))
				}
			}
		}
	}
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
	add := func(kind, target string, value any) (*definitionWorkStep, error) {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		n, err := platform.Default().Normalize(kind, target, raw)
		if err != nil {
			return nil, err
		}
		steps = append(steps, definitionWorkStep{Kind: kind, Target: target, Input: n.Canonical})
		return &steps[len(steps)-1], nil
	}
	resources, err := r.ResourceApplySpecs()
	if err != nil {
		return nil, err
	}
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
		canonical := n.Input.(*platform.ResourceApplyInput)
		changed := true
		for _, previous := range snap.Resources {
			if previous.Name == resource.Name {
				var spec platform.ResourceSpec
				if json.Unmarshal(previous.Spec, &spec) == nil {
					spec, err = platform.NormalizeResourceSpec(spec)
					if err == nil && reflect.DeepEqual(spec, canonical.ResourceSpec) && previous.State == "ready" {
						changed = false
					}
				}
				break
			}
		}
		if changed {
			steps = append(steps, definitionWorkStep{Kind: n.Kind, Target: rt, Input: n.Canonical})
		}
	}
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
		if _, err = add(platform.KindEnvironmentUpdate, target, patch); err != nil {
			return nil, err
		}
	}
	// Primary first makes promotion safe before old aliases are removed.
	ordered := append([]api.DefinitionDomainChange(nil), plan.Domains...)
	sort.SliceStable(ordered, func(i, j int) bool {
		rank := func(d api.DefinitionDomainChange) int {
			if d.Action == "remove" {
				return 2
			}
			if d.Primary {
				return 0
			}
			return 1
		}
		return rank(ordered[i]) < rank(ordered[j])
	})
	for _, d := range ordered {
		id := d.ID
		if d.Action == "add" {
			id = definitionPlannedDomainID(snap.Workspace.ID, r.App, r.Environment, input.PlanNonce, d.Name)
			if id != d.ID {
				return nil, fmt.Errorf("planned domain identity mismatch: %s", d.Name)
			}
		}
		var dns *integrations.DefinitionDNSPlan
		for i := range plan.DNS {
			if plan.DNS[i].Host == d.Name {
				if dns != nil || plan.DNS[i].DomainID != string(id) {
					return nil, fmt.Errorf("DNS plan identity mismatch: %s", d.Name)
				}
				dns = &plan.DNS[i]
			}
		}
		var cleanup *integrations.DomainDNSPlan
		for i := range plan.DNSCleanup {
			if plan.DNSCleanup[i].Host == d.Name {
				if cleanup != nil || plan.DNSCleanup[i].DomainID != string(id) {
					return nil, fmt.Errorf("DNS cleanup identity mismatch: %s", d.Name)
				}
				cleanup = &plan.DNSCleanup[i]
			}
		}
		if d.Action != "remove" && snap.Policy.Enabled && dns == nil {
			return nil, fmt.Errorf("missing DNS plan: %s", d.Name)
		}
		if d.Action == "remove" {
			for _, b := range snap.Bindings {
				if b.ID == id && b.ZoneID != "" && cleanup == nil {
					return nil, fmt.Errorf("missing DNS cleanup plan: %s", d.Name)
				}
			}
		}
		if d.Action == "unchanged" && dns == nil {
			continue
		}
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
		step, err := add(kind, target, v)
		if err != nil {
			return nil, err
		}
		step.DomainID, step.DNSPlan, step.DNSCleanup = id, dns, cleanup
		if d.Action == "add" {
			step.AllocatedDomainID = id
		}
	}
	return steps, nil
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

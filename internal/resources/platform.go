//go:build !resources_no_platform

package resources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// Services maps one explicit application host to its configured Service. It is
// the only mutable process state this package adds; callers configure it before
// registering the platform adapters.
type Services struct {
	mu     sync.RWMutex
	byHost map[string]*Service
}

func NewServices() *Services { return &Services{byHost: make(map[string]*Service)} }

// Configure builds and stores the Service for config.Host. Reconfiguring a host
// replaces the in-process adapter; it does not change Docker or database state.
func (s *Services) Configure(config Config) error {
	service, err := New(config)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byHost == nil {
		s.byHost = make(map[string]*Service)
	}
	s.byHost[config.Host] = service
	return nil
}

func (s *Services) Service(_ context.Context, host string) (*Service, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	service := s.byHost[host]
	if service == nil {
		return nil, notFound("resource host is not configured", "Configure the application host before submitting resource or backup actions for it.")
	}
	return service, nil
}

func (s *Services) Hosts() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	hosts := make([]string, 0, len(s.byHost))
	for host := range s.byHost {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

var defaultServices = NewServices()

// DefaultServices returns the process-wide host registry used by RegisterDefault.
func DefaultServices() *Services { return defaultServices }

// Configure is the process-wide convenience used before RegisterDefault.
func Configure(config Config) error { return defaultServices.Configure(config) }

// RegisterDefault registers the extension actions and inventory providers in the
// process-wide platform registry. Call it before controlplane.New.
func RegisterDefault() error { return Register(platform.Default(), defaultServices) }

// Register adds the durable resource and backup actions plus the resources and
// backups inventory collections to a platform registry.
func Register(registry *platform.Registry, services *Services) error {
	if registry == nil || services == nil {
		return invalidResource("platform registration requires a registry and services", "Configure resource hosts and pass the platform registry explicitly.")
	}
	executor := &Executor{services: services}
	definitions := []platform.Definition{
		{Kind: KindResourceCreate, Summary: "Create or converge a PostgreSQL application resource", Normalize: normalizeResourceCreate, Executor: executor},
		{Kind: KindResourceHealth, Summary: "Observe a PostgreSQL application resource", Normalize: normalizeResourceHealth, Executor: executor},
		{Kind: KindResourceDestroy, Summary: "Destroy an application resource with exact confirmation", Normalize: normalizeResourceDestroy, Executor: executor},
		{Kind: KindResourceRecover, Summary: "Recover an interrupted database restore", Normalize: normalizeResourceRecover, Executor: executor},
		{Kind: KindResourceCredentials, Summary: "Export resource credentials to one protected file", Normalize: normalizeResourceCredentials, Executor: executor},
		{Kind: KindBackupCreate, Summary: "Create a PostgreSQL logical backup archive", Normalize: normalizeBackupCreate, Executor: executor},
		{Kind: KindBackupVerify, Summary: "Verify a backup archive checksum and table of contents", Normalize: normalizeBackupVerify, Executor: executor},
		{Kind: KindBackupRestore, Summary: "Restore a verified backup with a safety backup and rollback", Normalize: normalizeBackupRestore, Executor: executor},
		{Kind: KindBackupSchedule, Summary: "Record a backup schedule and retention policy", Normalize: normalizeBackupSchedule, Executor: executor},
	}
	for _, definition := range definitions {
		if err := registry.Register(definition); err != nil {
			return err
		}
	}
	if err := registry.RegisterInventory(&resourceInventory{services: services}); err != nil {
		return err
	}
	return registry.RegisterInventory(&backupInventory{services: services})
}

// Executor decodes one registered action and runs it through a host Service.
type Executor struct {
	services *Services
}

var _ platform.Executor = (*Executor)(nil)

func (e *Executor) Execute(ctx context.Context, execution platform.Execution) (platform.Result, error) {
	if e.services == nil {
		return platform.Result{}, unavailable("resource services are not configured", "Configure the application host before executing resource actions.")
	}
	request, spec, err := requestFromPlatform(execution.Kind, execution.Input)
	if err != nil {
		return platform.Result{}, err
	}
	// Preserve the control plane's durable operation identity. It is the
	// idempotency key for a lost acknowledgement and is never synthesized from
	// the Host string.
	request.OperationID = string(execution.OperationID)
	service, err := e.services.Service(ctx, spec.Host)
	if err != nil {
		return platform.Result{}, err
	}
	report := func(stage, message string) {
		if execution.Report != nil {
			execution.Report(platform.Event{Level: "info", Stage: stage, Message: message})
		}
	}
	stage := strings.TrimPrefix(execution.Kind, "platform.")
	report(stage, "resource action started")
	result, err := service.Execute(ctx, request)
	if err != nil {
		report(stage, "resource action failed")
		return platform.Result{}, err
	}
	for _, warning := range result.Warnings {
		if execution.Report != nil {
			execution.Report(platform.Event{Level: "warning", Stage: stage, Message: warning})
		}
	}
	message := platformMessage(result)
	report(stage, message)
	return platform.Result{Message: message, Data: result}, nil
}

func platformMessage(result Result) string {
	switch result.Action {
	case ActionCreate:
		if result.Changed {
			return fmt.Sprintf("resource %s converged on %s", result.Resource, result.Host)
		}
		return fmt.Sprintf("resource %s was already present", result.Resource)
	case ActionDestroy:
		return fmt.Sprintf("resource %s destruction completed", result.Resource)
	case ActionHealth:
		return fmt.Sprintf("resource %s observed as %s", result.Resource, result.State)
	case ActionBackup:
		return fmt.Sprintf("backup for %s created", result.Resource)
	case ActionVerify:
		return fmt.Sprintf("backup for %s verified", result.Resource)
	case ActionRestore:
		return fmt.Sprintf("resource %s restored", result.Resource)
	case ActionRecover:
		if result.Recovery != nil && result.Recovery.Required {
			return fmt.Sprintf("resource %s restore recovery completed", result.Resource)
		}
		return fmt.Sprintf("resource %s has no interrupted restore", result.Resource)
	case ActionCredentials:
		if result.Export != nil {
			return fmt.Sprintf("resource %s credentials written to %s", result.Resource, result.Export.Destination)
		}
		return fmt.Sprintf("resource %s credentials exported", result.Resource)
	case ActionSchedule:
		return fmt.Sprintf("resource %s backup schedule policy recorded", result.Resource)
	default:
		return "resource action completed"
	}
}

func requestFromPlatform(kind string, raw json.RawMessage) (Request, Spec, error) {
	switch kind {
	case KindResourceCreate:
		var input ResourceCreateInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionCreate, Spec: input.Spec}, input.Spec, nil
	case KindResourceHealth:
		var input ResourceHealthInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionHealth, Spec: input.Spec}, input.Spec, nil
	case KindResourceDestroy:
		var input ResourceDestroyInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionDestroy, Spec: input.Spec, Destroy: &DestroyRequest{Confirmation: input.Confirmation, RemoveVolume: input.RemoveVolume}}, input.Spec, nil
	case KindResourceRecover:
		var input ResourceRecoverInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionRecover, Spec: input.Spec}, input.Spec, nil
	case KindResourceCredentials:
		var input ResourceCredentialsInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionCredentials, Spec: input.Spec, Export: &ExportRequest{Destination: input.Destination}}, input.Spec, nil
	case KindBackupSchedule:
		var input BackupScheduleInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		policy := input.Policy
		return Request{Action: ActionSchedule, Spec: input.Spec, Policy: &policy}, input.Spec, nil
	case KindBackupCreate:
		var input BackupCreateInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionBackup, Spec: input.Spec, Backup: &BackupRequest{Reason: input.Reason, Policy: input.Policy}}, input.Spec, nil
	case KindBackupVerify:
		var input BackupVerifyInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionVerify, Spec: input.Spec, Verify: &VerifyRequest{Archive: input.Archive}}, input.Spec, nil
	case KindBackupRestore:
		var input BackupRestoreInput
		if err := decodePlatformInput(raw, &input); err != nil {
			return Request{}, Spec{}, err
		}
		return Request{Action: ActionRestore, Spec: input.Spec, Restore: &RestoreRequest{Archive: input.Archive, Confirmation: input.Confirmation, Policy: input.Policy}}, input.Spec, nil
	default:
		return Request{}, Spec{}, invalidResource("unknown resource action kind", "Use a registered platform.resource.* or platform.backup.* kind.")
	}
}

// Platform inputs. Every one carries only the secret-free resource spec plus
// non-secret options. The canonical target is node/<explicit-host>.

type ResourceCreateInput struct {
	Spec Spec `json:"spec"`
}

func (in ResourceCreateInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in ResourceCreateInput) Preview() any {
	return staticPreview(ActionCreate, in.Spec, false)
}

type ResourceHealthInput struct {
	Spec Spec `json:"spec"`
}

func (in ResourceHealthInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in ResourceHealthInput) Preview() any {
	return staticPreview(ActionHealth, in.Spec, false)
}

type ResourceDestroyInput struct {
	Spec         Spec   `json:"spec"`
	RemoveVolume bool   `json:"remove_volume,omitempty"`
	Confirmation string `json:"confirmation"`
}

func (in ResourceDestroyInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in ResourceDestroyInput) Preview() any {
	return staticPreview(ActionDestroy, in.Spec, in.RemoveVolume)
}

type ResourceRecoverInput struct {
	Spec Spec `json:"spec"`
}

func (in ResourceRecoverInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in ResourceRecoverInput) Preview() any {
	return staticPreview(ActionRecover, in.Spec, false)
}

type ResourceCredentialsInput struct {
	Spec        Spec   `json:"spec"`
	Destination string `json:"destination"`
}

func (in ResourceCredentialsInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in ResourceCredentialsInput) Preview() any {
	return staticPreview(ActionCredentials, in.Spec, false)
}

type BackupCreateInput struct {
	Spec   Spec    `json:"spec"`
	Reason string  `json:"reason,omitempty"`
	Policy *Policy `json:"policy,omitempty"`
}

func (in BackupCreateInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in BackupCreateInput) Preview() any {
	return staticPreview(ActionBackup, in.Spec, false)
}

type BackupVerifyInput struct {
	Spec    Spec   `json:"spec"`
	Archive string `json:"archive"`
}

func (in BackupVerifyInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in BackupVerifyInput) Preview() any {
	return staticPreview(ActionVerify, in.Spec, false)
}

type BackupRestoreInput struct {
	Spec         Spec    `json:"spec"`
	Archive      string  `json:"archive"`
	Confirmation string  `json:"confirmation"`
	Policy       *Policy `json:"policy,omitempty"`
}

func (in BackupRestoreInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in BackupRestoreInput) Preview() any {
	return staticPreview(ActionRestore, in.Spec, false)
}

type BackupScheduleInput struct {
	Spec   Spec   `json:"spec"`
	Policy Policy `json:"policy"`
}

func (in BackupScheduleInput) Target() platform.Target {
	return platform.Target{Kind: platform.TargetNode, Node: in.Spec.Host}
}
func (in BackupScheduleInput) Preview() any {
	return staticPreview(ActionSchedule, in.Spec, false)
}

func decodePlatformInput(raw json.RawMessage, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return invalidResource("invalid resource action input", "Use the documented object fields; unknown fields are rejected.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalidResource("resource action input has trailing data", "Provide exactly one JSON object.")
	}
	return nil
}

func validatePlatformSpec(target platform.Target, spec Spec) error {
	if target.Kind != platform.TargetNode || target.Node == "" {
		return invalidResource("resource action requires node/<host> target", "Use the explicit application host as the canonical node target.")
	}
	if spec.Host != target.Node {
		return invalidResource("resource host does not match the target", "Use node/<host> with the same explicit application host as spec.host.")
	}
	if spec.Protection == ProtectionControlPlane {
		return invalidResource("control-plane databases are not application resources", "Use the separate control-plane recovery procedure; extension actions cannot manage it.")
	}
	return ValidateSpec(spec)
}

func normalizeResourceCreate(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input ResourceCreateInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeResourceHealth(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input ResourceHealthInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeResourceDestroy(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input ResourceDestroyInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	if !confirmed(input.Confirmation, input.Spec.Name) {
		return nil, confirmationFault(input.Spec.Name)
	}
	return &input, nil
}

func normalizeResourceRecover(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input ResourceRecoverInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeResourceCredentials(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input ResourceCredentialsInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	if err := validateExportDestination(input.Destination); err != nil {
		return nil, err
	}
	return &input, nil
}

func normalizeBackupCreate(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input BackupCreateInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	if input.Reason != "" && !validBackupReason(input.Reason) {
		return nil, invalidResource("invalid backup reason", "Use manual, scheduled, or safety.")
	}
	if input.Policy != nil {
		if err := input.Policy.Validate(); err != nil {
			return nil, err
		}
	}
	return &input, nil
}

func normalizeBackupVerify(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input BackupVerifyInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Archive) == "" {
		return nil, invalidResource("backup archive is required", "Name an archive inside the configured backup directory.")
	}
	return &input, nil
}

func normalizeBackupRestore(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input BackupRestoreInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Archive) == "" {
		return nil, invalidResource("backup archive is required", "Name a verified archive inside the configured backup directory.")
	}
	if !confirmed(input.Confirmation, input.Spec.Name) {
		return nil, confirmationFault(input.Spec.Name)
	}
	if input.Policy != nil {
		if err := input.Policy.Validate(); err != nil {
			return nil, err
		}
	}
	return &input, nil
}

func normalizeBackupSchedule(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	var input BackupScheduleInput
	if err := decodePlatformInput(raw, &input); err != nil {
		return nil, err
	}
	if err := validatePlatformSpec(target, input.Spec); err != nil {
		return nil, err
	}
	if err := input.Policy.Validate(); err != nil {
		return nil, err
	}
	return &input, nil
}

// ResourceInventoryItem is a secret-free observation for the resources
// collection.
type ResourceInventoryItem struct {
	Spec       Spec                 `json:"spec"`
	State      State                `json:"state"`
	Endpoint   *Endpoint            `json:"endpoint,omitempty"`
	Credential *CredentialReference `json:"credential,omitempty"`
	Health     *Health              `json:"health,omitempty"`
}

type resourceInventory struct {
	services *Services
}

func (*resourceInventory) Collection() string { return "resources" }

func (p *resourceInventory) Read(ctx context.Context, query platform.InventoryQuery) (any, error) {
	limit := inventoryLimit(query.Limit)
	hosts, err := inventoryHosts(p.services, query.Node)
	if err != nil {
		return nil, err
	}
	items := make([]ResourceInventoryItem, 0)
	for _, host := range hosts {
		service, err := p.services.Service(ctx, host)
		if err != nil {
			return nil, err
		}
		specs, err := service.ListResources()
		if err != nil {
			return nil, err
		}
		for _, spec := range specs {
			health, err := service.health(ctx, spec)
			var item ResourceInventoryItem
			if err != nil {
				classification := contract.Classify(err)
				item = ResourceInventoryItem{
					Spec: spec, State: StateUnknown,
					Health: &Health{Status: HealthUnknown, Detail: classification.Message, CheckedAt: service.now()},
				}
			} else {
				item = ResourceInventoryItem{
					Spec: spec, State: health.State, Endpoint: health.Endpoint,
					Credential: health.Credential, Health: health.Health,
				}
			}
			if query.State != "" && string(item.State) != query.State {
				continue
			}
			items = append(items, item)
			if len(items) >= limit {
				break
			}
		}
		if len(items) >= limit {
			break
		}
	}
	return platform.Inventory{Collection: "resources", Items: items, Limit: limit}, nil
}

type backupInventory struct {
	services *Services
}

func (*backupInventory) Collection() string { return "backups" }

func (p *backupInventory) Read(ctx context.Context, query platform.InventoryQuery) (any, error) {
	limit := inventoryLimit(query.Limit)
	hosts, err := inventoryHosts(p.services, query.Node)
	if err != nil {
		return nil, err
	}
	items := make([]BackupArtifact, 0)
	for _, host := range hosts {
		service, err := p.services.Service(ctx, host)
		if err != nil {
			return nil, err
		}
		artifacts, err := service.ListBackups()
		if err != nil {
			return nil, err
		}
		items = append(items, artifacts...)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	if len(items) > limit {
		items = items[:limit]
	}
	return platform.Inventory{Collection: "backups", Items: items, Limit: limit}, nil
}

func inventoryHosts(services *Services, node string) ([]string, error) {
	if services == nil {
		return nil, unavailable("resource services are not configured", "Configure an application host before reading resource inventory.")
	}
	if node != "" {
		if _, err := services.Service(context.Background(), node); err != nil {
			return nil, err
		}
		return []string{node}, nil
	}
	hosts := services.Hosts()
	if len(hosts) == 0 {
		return nil, nil
	}
	return hosts, nil
}

func inventoryLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

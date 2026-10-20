package resources

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func platformInput(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPlatformRegistrationAndCreateExecution(t *testing.T) {
	spec := testSpec()
	fake := newFakeHost(spec)
	services := NewServices()
	if err := services.Configure(Config{
		Host: spec.Host, StateDir: t.TempDir(), BackupDir: t.TempDir(), Runner: fake,
	}); err != nil {
		t.Fatal(err)
	}
	registry := platform.NewRegistry()
	if err := Register(registry, services); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup(KindResourceCreate); !ok {
		t.Fatal("resource create kind was not registered")
	}
	if _, ok := registry.InventoryProvider("resources"); !ok {
		t.Fatal("resources inventory was not registered")
	}
	if _, ok := registry.InventoryProvider("backups"); !ok {
		t.Fatal("backups inventory was not registered")
	}

	raw := platformInput(t, ResourceCreateInput{Spec: spec})
	normalized, err := registry.Normalize(KindResourceCreate, "node/app-1", raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(normalized.Canonical), "password") {
		t.Fatalf("canonical operation input contains a credential field: %s", normalized.Canonical)
	}
	executor := normalized.Executor()
	if executor == nil {
		t.Fatal("registered action has no executor")
	}
	result, err := executor.Execute(context.Background(), platform.Execution{
		Kind: KindResourceCreate, Target: "node/app-1", OperationID: "op-platform-create", Input: normalized.Canonical,
		Report: func(platform.Event) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	created, ok := result.Data.(Result)
	if !ok {
		t.Fatalf("unexpected executor data %T", result.Data)
	}
	if created.State != StateReady || created.Credential == nil {
		t.Fatalf("unexpected resource result: %+v", created)
	}
	if strings.Contains(result.Message, "password") || strings.Contains(result.Message, created.Credential.PasswordFile) {
		t.Fatalf("platform message leaks credential material: %s", result.Message)
	}

	provider, _ := registry.InventoryProvider("resources")
	inventory, err := provider.Read(context.Background(), platform.InventoryQuery{Collection: "resources"})
	if err != nil {
		t.Fatal(err)
	}
	page, ok := inventory.(platform.Inventory)
	if !ok {
		t.Fatalf("unexpected inventory type %T", inventory)
	}
	items, ok := page.Items.([]ResourceInventoryItem)
	if !ok || len(items) != 1 || items[0].Spec.Name != "appdb" || items[0].Credential == nil {
		t.Fatalf("unexpected inventory: %#v", page.Items)
	}
}

func TestPlatformDestroyAndRestoreRequireConfirmation(t *testing.T) {
	spec := testSpec()
	services := NewServices()
	if err := services.Configure(Config{Host: spec.Host, StateDir: t.TempDir(), BackupDir: t.TempDir(), Runner: newFakeHost(spec)}); err != nil {
		t.Fatal(err)
	}
	registry := platform.NewRegistry()
	if err := Register(registry, services); err != nil {
		t.Fatal(err)
	}
	destroy := platformInput(t, ResourceDestroyInput{Spec: spec, Confirmation: "wrong"})
	if _, err := registry.Normalize(KindResourceDestroy, "node/app-1", destroy); err == nil || contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("destroy confirmation was not enforced: %v", err)
	}
	restore := platformInput(t, BackupRestoreInput{Spec: spec, Archive: "x.dump", Confirmation: "wrong"})
	if _, err := registry.Normalize(KindBackupRestore, "node/app-1", restore); err == nil || contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("restore confirmation was not enforced: %v", err)
	}
	protected := spec
	protected.Protection = ProtectionControlPlane
	raw := platformInput(t, ResourceCreateInput{Spec: protected})
	if _, err := registry.Normalize(KindResourceCreate, "node/app-1", raw); err == nil {
		t.Fatal("control-plane protection was accepted by the platform adapter")
	}
	wrongHost := platformInput(t, ResourceCreateInput{Spec: testSpec()})
	if _, err := registry.Normalize(KindResourceCreate, "node/other", wrongHost); err == nil {
		t.Fatal("target host mismatch was accepted")
	}
}

func TestPeriodicBackupDeclarationUsesTheGenericActionPath(t *testing.T) {
	service, _ := newTestService(t, testSpec())
	policy := Policy{
		Schedule: Schedule{EverySeconds: 86400}, Retention: Retention{KeepLast: 7}, VerifyAfter: true,
	}
	scheduled, err := service.PeriodicBackup(testSpec(), policy)
	if err != nil {
		t.Fatal(err)
	}
	if scheduled.Kind != KindBackupCreate || scheduled.Target != "node/app-1" || scheduled.Key == "" {
		t.Fatalf("unexpected schedule: %+v", scheduled)
	}
	var request Request
	if err := json.Unmarshal(scheduled.Input, &request); err != nil {
		t.Fatal(err)
	}
	if request.Action != ActionBackup || request.Backup == nil || request.Backup.Reason != "scheduled" || request.Backup.Policy == nil {
		t.Fatalf("unexpected periodic request: %+v", request)
	}
	if strings.Contains(string(scheduled.Input), "password") {
		t.Fatalf("periodic input contains credential material: %s", scheduled.Input)
	}
	if _, err := service.PeriodicBackup(testSpec(), Policy{Schedule: Schedule{EverySeconds: 60}, Retention: Retention{}}); err == nil {
		t.Fatal("policy without retention was accepted")
	}
}

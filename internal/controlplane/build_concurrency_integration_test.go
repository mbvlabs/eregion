package controlplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestIntegrationBuildConcurrencySettingAndAdmission(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := testRuntime(t, pool, 0, t.TempDir()).Service
	if err := insertFixtureNode(ctx, pool, "main", []string{platform.RoleBuilder}, []string{platform.CapabilityDocker, platform.CapabilityBuildpacks}, ""); err != nil {
		t.Fatal(err)
	}
	app := api.NewAppService(service, nil, nil)
	operator := contract.Actor{Kind: "operator", Subject: "integration-operator"}
	view, err := app.GetBuildConcurrency(ctx, operator)
	if err != nil || view.Node != "main" || view.Effective != 1 {
		t.Fatalf("default setting: %+v, %v", view, err)
	}
	for _, value := range []int{0, 17} {
		if _, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: value}); err == nil {
			t.Fatalf("invalid capacity %d was accepted", value)
		}
	}
	if _, err := app.SetBuildConcurrency(ctx, testActor, api.BuildConcurrencyInput{Value: 2}); err == nil {
		t.Fatal("non-operator changed build capacity")
	}
	if _, err := app.GetBuildConcurrency(ctx, testActor); err == nil {
		t.Fatal("non-operator read build capacity")
	}
	preview, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: 2, DryRun: true})
	if err != nil || preview.Previous != 1 || preview.Effective != 2 || !preview.DryRun {
		t.Fatalf("preview: %+v, %v", preview, err)
	}
	view, err = app.GetBuildConcurrency(ctx, operator)
	if err != nil || view.Effective != 1 {
		t.Fatalf("preview wrote state: %+v, %v", view, err)
	}
	set, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: 2})
	if err != nil || set.Previous != 1 || set.Effective != 2 {
		t.Fatalf("set: %+v, %v", set, err)
	}
	first, err := service.acquireBuildSlot(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := service.acquireBuildSlot(ctx, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	if _, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: 1}); err != nil {
		t.Fatal(err)
	}
	first() // slot zero is free, but slot one still occupies the reduced limit
	oldPoll, oldWait := buildSlotPoll, buildSlotWait
	buildSlotPoll, buildSlotWait = 20*time.Millisecond, 250*time.Millisecond
	t.Cleanup(func() { buildSlotPoll, buildSlotWait = oldPoll, oldWait })
	if release, err := service.acquireBuildSlot(ctx, "main", nil); err == nil {
		release()
		t.Fatal("admitted above the reduced capacity")
	} else {
		wantViolation(t, err, ReasonBuildCapacity)
	}
	buildSlotWait = 5 * time.Second
	waiting := make(chan struct{}, 1)
	admitted := make(chan func(), 1)
	admissionErr := make(chan error, 1)
	go func() {
		release, err := service.acquireBuildSlot(ctx, "main", func() { waiting <- struct{}{} })
		if err != nil {
			admissionErr <- err
			return
		}
		admitted <- release
	}()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("admission did not wait at the reduced capacity")
	}
	if _, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case release := <-admitted:
		release()
	case err := <-admissionErr:
		t.Fatalf("waiting admission did not reread capacity: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("waiting admission ignored increased capacity")
	}
	if _, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: 1}); err != nil {
		t.Fatal(err)
	}
	second()
	release, err := service.acquireBuildSlot(ctx, "main", nil)
	if err != nil {
		t.Fatalf("released occupancy did not admit: %v", err)
	}
	release()

	// Simultaneous writers are row-serialized: each observes the committed
	// value left by its predecessor, regardless of which wins last.
	results := make(chan api.BuildConcurrencyResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, value := range []int{3, 4} {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			result, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: value})
			results <- result
			errs <- err
		}(value)
	}
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	a, b := <-results, <-results
	if !((a.Previous == 1 && b.Previous == a.Effective) || (b.Previous == 1 && a.Previous == b.Effective)) {
		t.Fatalf("writers did not serialize: %+v / %+v", a, b)
	}
	view, err = app.GetBuildConcurrency(ctx, operator)
	if err != nil || (view.Effective != a.Effective && view.Effective != b.Effective) {
		t.Fatalf("final capacity: %+v, %v", view, err)
	}
	identity := service.registryHostIdentity
	service.registryHostIdentity = nil
	if _, err := app.SetBuildConcurrency(ctx, operator, api.BuildConcurrencyInput{Value: 2}); err == nil {
		t.Fatal("unverified host was allowed to change capacity")
	}
	service.registryHostIdentity = identity
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_nodes SET transport='ssh' WHERE name='main'`); err != nil {
		t.Fatal(err)
	}
	if _, err := app.GetBuildConcurrency(ctx, operator); err == nil {
		t.Fatal("non-local main-node placement was accepted")
	}
}

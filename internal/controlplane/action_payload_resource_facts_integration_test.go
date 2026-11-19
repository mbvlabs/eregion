package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

func readActionPayloadError(t *testing.T, service *Service, op contract.Operation) error {
	t.Helper()
	var raw json.RawMessage
	err := service.entities.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		var err error
		raw, err = service.actionPayloadTx(context.Background(), tx, &op, contract.Succeeded)
		return err
	})
	if err == nil {
		t.Fatalf("action payload accepted corrupt stored resource facts: %s", raw)
	}
	return err
}

func TestIntegrationActionPayloadResourceStoredFactsFailClosed(t *testing.T) {
	pool, _ := testPool(t, true)
	service := &Service{entities: newTestEntities(t, pool)}
	f := newPlatformFixture(t, pool, "payload-resource-facts")
	ctx := context.Background()
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	resourceID, err := platform.NewResourceID()
	if err != nil {
		t.Fatal(err)
	}
	target := "resource/" + f.app.Name + "/" + f.env.Name + "/cache"
	spec := platform.ResourceSpec{
		Image:  "redis:7",
		Mounts: []platform.ResourceMount{{Name: "data", Destination: "/data"}},
		Health: platform.ResourceHealthSpec{Type: "exec", Command: []string{"redis-cli", "ping"}},
	}
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Operations().Insert(ctx, entities.OperationInsert{
			ID: operationID, Key: entities.OperationKey{Kind: platform.KindResourceApply, Target: target, Actor: contract.Actor{Kind: "ci", Subject: "platform"}, IdempotencyKey: "payload-resource-facts"},
			InputHash: "hash", Input: json.RawMessage(`{}`),
		}); err != nil {
			return err
		}
		_, err := tx.Resources().Insert(ctx, entities.ResourceInsert{
			ID: resourceID, AppID: f.app.ID, EnvironmentID: f.env.ID, Name: "cache", Node: f.node,
			Spec: spec, Image: spec.Image, OperationID: operationID,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	volumeName := "er-res-" + strings.TrimPrefix(string(resourceID), "res_") + "-data"
	volumes, err := json.Marshal([]platform.ResourceVolumeStatus{{Name: volumeName, Destination: "/data"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET state='ready',volumes=$2::jsonb,
		health_state='healthy',health_checked_at=clock_timestamp(),updated_at=clock_timestamp()
		WHERE id=$1`, resourceID, string(volumes)); err != nil {
		t.Fatal(err)
	}
	var storedUpdatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT updated_at FROM eregion_platform_resources WHERE id=$1`, resourceID).Scan(&storedUpdatedAt); err != nil {
		t.Fatal(err)
	}
	op := contract.Operation{ID: operationID, Kind: platform.KindResourceApply, Target: target}
	var valid resourcePayload
	raw := readActionPayload(t, service, op, &valid)
	if valid.CurrentObservation == nil || valid.CurrentObservation.ID != string(resourceID) ||
		valid.CurrentObservation.State != string(platform.ResourceReady) ||
		!valid.CurrentObservation.UpdatedAt.Equal(storedUpdatedAt) ||
		len(valid.CurrentObservation.VolumeNames) != 1 || valid.CurrentObservation.VolumeNames[0] != volumeName {
		t.Fatalf("valid stored resource facts: %s", raw)
	}

	// A stored spec that the declaration normalizer rejects must not be
	// projected as a partial observation.
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET spec='{"image":""}'::jsonb,
		updated_at=clock_timestamp() WHERE id=$1`, resourceID); err != nil {
		t.Fatal(err)
	}
	if err := readActionPayloadError(t, service, op); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("invalid stored spec error = %v, want unavailable", err)
	}

	validSpec, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	// A volume that does not belong to the resource identity must fail closed
	// even when the stored spec is valid.
	if _, err := pool.Exec(ctx, `UPDATE eregion_platform_resources SET spec=$2::jsonb,
		volumes='[{"name":"er-res-00000000000000000000000000000000-data","destination":"/data"}]'::jsonb,
		updated_at=clock_timestamp() WHERE id=$1`, resourceID, string(validSpec)); err != nil {
		t.Fatal(err)
	}
	if err := readActionPayloadError(t, service, op); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("foreign stored volume error = %v, want unavailable", err)
	}
}

package controlplane

import (
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/resources"
)

func TestNextScheduledRunUsesDeclarationAndLastAttempt(t *testing.T) {
	declared := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	policy := resources.Policy{Schedule: resources.Schedule{EverySeconds: 3600}, Retention: resources.Retention{KeepLast: 3}}

	slot, due, err := nextScheduledRun(policy, declared, time.Time{}, declared.Add(30*time.Minute))
	if err != nil || due || !slot.Equal(declared.Add(time.Hour)) {
		t.Fatalf("before the first interval: slot=%s due=%v err=%v", slot, due, err)
	}
	slot, due, err = nextScheduledRun(policy, declared, time.Time{}, declared.Add(time.Hour))
	if err != nil || !due || !slot.Equal(declared.Add(time.Hour)) {
		t.Fatalf("at the first interval: slot=%s due=%v err=%v", slot, due, err)
	}
	// A completed attempt advances the base, so a retried tick is not due again.
	last := declared.Add(time.Hour)
	slot, due, err = nextScheduledRun(policy, declared, last, last.Add(10*time.Minute))
	if err != nil || due || !slot.Equal(last.Add(time.Hour)) {
		t.Fatalf("after an attempt: slot=%s due=%v err=%v", slot, due, err)
	}
	// Only the next window is considered per tick, even if several windows
	// elapsed while no worker ran. The next tick advances one window at a time.
	slot, due, err = nextScheduledRun(policy, declared, last, last.Add(2*time.Hour))
	if err != nil || !due || !slot.Equal(last.Add(time.Hour)) {
		t.Fatalf("two windows after an attempt: slot=%s due=%v err=%v", slot, due, err)
	}
}

func TestNextScheduledRunHonoursCronAndTimezone(t *testing.T) {
	declared := time.Date(2026, 10, 20, 4, 0, 0, 0, time.UTC)
	policy := resources.Policy{
		Schedule:  resources.Schedule{Cron: "0 3 * * *", Timezone: "UTC"},
		Retention: resources.Retention{KeepLast: 1},
	}
	slot, due, err := nextScheduledRun(policy, declared, time.Time{}, time.Date(2026, 10, 21, 3, 30, 0, 0, time.UTC))
	if err != nil || !due || !slot.Equal(time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("cron slot = %s due=%v err=%v", slot, due, err)
	}
	last := slot
	slot, due, err = nextScheduledRun(policy, declared, last, time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC))
	if err != nil || due || !slot.Equal(time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("cron after an attempt = %s due=%v err=%v", slot, due, err)
	}

	local := resources.Policy{
		Schedule:  resources.Schedule{Cron: "0 3 * * *", Timezone: "Europe/Berlin"},
		Retention: resources.Retention{KeepLast: 1},
	}
	// 03:00 Berlin on 2026-10-21 is 01:00 UTC (CEST).
	slot, due, err = nextScheduledRun(local, declared, time.Time{}, time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC))
	if err != nil || !due || !slot.Equal(time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)) {
		t.Fatalf("timezone cron slot = %s due=%v err=%v", slot, due, err)
	}
}

func TestScheduledBackupKeyIsStablePerWindow(t *testing.T) {
	slot := time.Date(2026, 10, 21, 3, 0, 0, 0, time.UTC)
	inLocal := slot.In(time.FixedZone("offset", 5*3600))
	if scheduledBackupKey("appdb", slot) != scheduledBackupKey("appdb", inLocal) {
		t.Fatal("the idempotency key depends on the caller's timezone")
	}
	if scheduledBackupKey("appdb", slot) == scheduledBackupKey("appdb", slot.Add(time.Hour)) {
		t.Fatal("two windows share one idempotency key")
	}
	if scheduledBackupKey("appdb", slot) == scheduledBackupKey("otherdb", slot) {
		t.Fatal("two resources share one idempotency key")
	}
}

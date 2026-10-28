package controlplane

import (
	"testing"
	"time"
)

func TestContainerCleanupBackoff(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: time.Minute},
		{attempts: 1, want: time.Minute},
		{attempts: 2, want: 2 * time.Minute},
		{attempts: 3, want: 4 * time.Minute},
		{attempts: 6, want: 32 * time.Minute},
		{attempts: 7, want: time.Hour},
		{attempts: 20, want: time.Hour},
	}
	for _, test := range tests {
		if got := containerCleanupBackoff(test.attempts); got != test.want {
			t.Errorf("containerCleanupBackoff(%d) = %s, want %s", test.attempts, got, test.want)
		}
	}
}

func TestCleanupOperationIDIsStablePerLeak(t *testing.T) {
	created := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	row := containerCleanupRow{ContainerName: "eregion-demo-production-web-1", CreatedAt: created}
	first := cleanupOperationID(row)
	if second := cleanupOperationID(row); second != first {
		t.Fatalf("cleanup operation ID changed: %s != %s", first, second)
	}
	row.CreatedAt = created.Add(time.Second)
	if again := cleanupOperationID(row); again == first {
		t.Fatal("a new leak reused the old cleanup operation ID")
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("cleanup operation ID is invalid: %v", err)
	}
	if got := containerCleanupBackoff(1); got != time.Minute {
		t.Fatalf("first cleanup retry = %s", got)
	}
}

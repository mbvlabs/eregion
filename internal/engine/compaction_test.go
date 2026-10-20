package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestCompactDropsOldTerminalTasksAndKeepsTheRest(t *testing.T) {
	eng := newTestEngine(t)
	journal := NewJournal("local")
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
	recent := time.Now().UTC().Format(time.RFC3339Nano)
	journal.Tasks["op_old:step-v1"] = Task{Status: TaskCompleted, CompletedAt: old, StartedAt: old}
	journal.Tasks["op_old:step-v2"] = Task{Status: TaskFailed, CompletedAt: old, StartedAt: old}
	journal.Tasks["op_recent:step-v1"] = Task{Status: TaskCompleted, CompletedAt: recent, StartedAt: recent}
	journal.Tasks["op_running:step-v1"] = Task{Status: TaskRunning, StartedAt: recent}
	if err := journal.Save(eng.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	removed, err := Compact(context.Background(), eng.StateDir, Retention{MaxAge: 24 * time.Hour, Keep: 1})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d tasks, want 2", removed)
	}
	compacted, err := LoadJournal(eng.JournalPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := compacted.Tasks["op_old:step-v1"]; ok {
		t.Fatal("an old terminal task survived compaction")
	}
	if _, ok := compacted.Tasks["op_recent:step-v1"]; !ok {
		t.Fatal("a recent terminal task was compacted")
	}
	if _, ok := compacted.Tasks["op_running:step-v1"]; !ok {
		t.Fatal("a live task was compacted")
	}
}

func TestCompactKeepsTheMostRecentTasksRegardlessOfAge(t *testing.T) {
	eng := newTestEngine(t)
	journal := NewJournal("local")
	old := time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)
	for _, key := range []string{"op_a:step-v1", "op_b:step-v1", "op_c:step-v1"} {
		journal.Tasks[key] = Task{Status: TaskCompleted, CompletedAt: old, StartedAt: old}
	}
	if err := journal.Save(eng.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	removed, err := Compact(context.Background(), eng.StateDir, Retention{MaxAge: time.Hour, Keep: 3})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d tasks, want 0", removed)
	}
}

func TestCompactLeavesEffectRecordsInPlace(t *testing.T) {
	eng := newTestEngine(t)
	exec := testExecution(t, "test-step-v1", 1)
	step := testStep(t, exec, "", false)
	if _, err := eng.Run(context.Background(), step, exec, nil); err != nil {
		t.Fatal(err)
	}
	journal := NewJournal("local")
	journal.Tasks[exec.EffectKey()] = Task{
		OperationID: string(exec.OperationID), Step: exec.Step, Status: TaskCompleted,
		CompletedAt: time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano),
		StartedAt:   time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano),
	}
	if err := journal.Save(eng.JournalPath(), &secrets.Registry{}); err != nil {
		t.Fatal(err)
	}
	if _, err := Compact(context.Background(), eng.StateDir, Retention{MaxAge: time.Hour, Keep: 0}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := LoadEffectRecord(eng.EffectRecordPath(exec)); err != nil || !ok {
		t.Fatalf("the durable effect record was compacted: %v %v", ok, err)
	}
	status, err := eng.Status(context.Background(), exec)
	if err != nil || status.Status != StatusCompleted {
		t.Fatalf("a compacted effect is no longer visible: %+v %v", status, err)
	}
}

func TestCompactFailsClosedOnACorruptJournal(t *testing.T) {
	eng := newTestEngine(t)
	if err := os.WriteFile(eng.JournalPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Compact(context.Background(), eng.StateDir, DefaultRetention()); err == nil {
		t.Fatal("a corrupt journal was compacted silently")
	}
}

func TestCompactReportsNothingToDo(t *testing.T) {
	eng := newTestEngine(t)
	removed, err := Compact(context.Background(), eng.StateDir, DefaultRetention())
	if err != nil || removed != 0 {
		t.Fatalf("compaction of an empty journal: %d %v", removed, err)
	}
	if _, err := os.Stat(filepath.Join(eng.StateDir, "journal.json")); !os.IsNotExist(err) {
		t.Fatal("compaction created a journal file")
	}
}

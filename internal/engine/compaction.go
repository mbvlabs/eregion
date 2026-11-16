package engine

import (
	"context"
	"sort"
	"time"

	"github.com/mbvlabs/eregion/internal/secrets"
)

// Retention bounds the node journal. The journal is the engine's index of the
// latest attempt per effect; the effect records beside it are the durable
// exactly-once evidence and are not compacted automatically.
type Retention struct {
	// MaxAge drops a terminal task that is older than this. Zero keeps every
	// terminal task regardless of age.
	MaxAge time.Duration
	// Keep is the number of most recent terminal tasks that are always kept,
	// even when they are older than MaxAge.
	Keep int
}

// DefaultRetention is the policy a node service uses.
func DefaultRetention() Retention {
	return Retention{MaxAge: 7 * 24 * time.Hour, Keep: 500}
}

// Compact removes journal tasks that fall outside the retention window and
// reports how many it removed.
//
// It never removes a live task: a running task stays visible until it reaches
// a terminal state, or until it is older than MaxAge, at which point its owner
// is long gone and the effect lock it held is released by the kernel.
//
// The effect records are deliberately left in place. They are the durable
// record of the side effect; dropping one would let a stale retry re-apply an
// effect. Pruning them is an explicit operator action.
func Compact(ctx context.Context, stateDir string, retention Retention) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	path := JournalPath(stateDir)
	removed := 0
	err := WithFileLock(path+".lock", func() error {
		journal, err := LoadJournal(path)
		if err != nil {
			return err
		}
		drop := selectTasks(journal, retention)
		if len(drop) == 0 {
			return nil
		}
		for _, key := range drop {
			delete(journal.Tasks, key)
		}
		removed = len(drop)
		return journal.Save(path, &secrets.Registry{})
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// selectTasks returns the journal keys that fall outside the retention window.
func selectTasks(journal *Journal, retention Retention) []string {
	terminal := terminalTaskKeys(journal)
	sort.Slice(terminal, func(i, j int) bool {
		left, right := journal.Tasks[terminal[i]], journal.Tasks[terminal[j]]
		if left.CompletedAt != right.CompletedAt {
			return left.CompletedAt > right.CompletedAt
		}
		return terminal[i] < terminal[j]
	})
	cutoff := time.Now().UTC().Add(-retention.MaxAge)
	drop := terminalExpiredKeys(journal, terminal, retention, cutoff)
	return append(drop, runningExpiredKeys(journal, retention, cutoff)...)
}

// terminalTaskKeys returns every task key that is not a live running task.
func terminalTaskKeys(journal *Journal) []string {
	terminal := make([]string, 0, len(journal.Tasks))
	for key, task := range journal.Tasks {
		if task.Status == TaskRunning {
			continue
		}
		terminal = append(terminal, key)
	}
	return terminal
}

// terminalExpiredKeys returns terminal tasks outside the retention window.
func terminalExpiredKeys(journal *Journal, terminal []string, retention Retention, cutoff time.Time) []string {
	var drop []string
	for index, key := range terminal {
		task := journal.Tasks[key]
		if index < retention.Keep || retention.MaxAge <= 0 {
			continue
		}
		completed, err := time.Parse(time.RFC3339Nano, task.CompletedAt)
		if err != nil {
			// A task without a usable completion time keeps its entry; the
			// journal is never reset silently.
			continue
		}
		if completed.Before(cutoff) {
			drop = append(drop, key)
		}
	}
	return drop
}

// runningExpiredKeys drops a running task whose owner is long gone, so the
// journal does not grow without bound.
func runningExpiredKeys(journal *Journal, retention Retention, cutoff time.Time) []string {
	if retention.MaxAge <= 0 {
		return nil
	}
	var drop []string
	for key, task := range journal.Tasks {
		if task.Status != TaskRunning {
			continue
		}
		started, err := time.Parse(time.RFC3339Nano, task.StartedAt)
		if err != nil {
			continue
		}
		if started.Before(cutoff) {
			drop = append(drop, key)
		}
	}
	return drop
}

package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestWorkspaceNameAndIDValidation(t *testing.T) {
	for _, name := range []string{"thelab", "a", "alpha-7", "team-01"} {
		if err := ValidateWorkspaceName(name); err != nil {
			t.Fatalf("valid workspace name %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "Thelab", "-thelab", "thelab-", "the_lab", strings.Repeat("a", 65), "the lab"} {
		if err := ValidateWorkspaceName(name); err == nil {
			t.Fatalf("invalid workspace name %q was accepted", name)
		}
	}
	if err := ValidateWorkspaceID("ws_0123456789abcdef01234567"); err != nil {
		t.Fatalf("valid workspace ID: %v", err)
	}
	for _, id := range []string{"", "ws_", "ws_0123", "WS_0123456789abcdef01234567", "ws_0123456789abcdef0123456g"} {
		if err := ValidateWorkspaceID(id); err == nil {
			t.Fatalf("invalid workspace ID %q was accepted", id)
		}
	}
}

func TestNewWorkspaceIDIsPortableAndRandom(t *testing.T) {
	ids := map[string]bool{}
	for i := 0; i < 32; i++ {
		id, err := NewWorkspaceID()
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateWorkspaceID(id); err != nil {
			t.Fatalf("generated ID %q: %v", id, err)
		}
		if ids[id] {
			t.Fatalf("duplicate generated ID %q", id)
		}
		ids[id] = true
	}
}

type workspaceTestRow struct {
	exists bool
	err    error
}

func (r workspaceTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*bool)) = r.exists
	return nil
}

type workspaceTestQuerier struct {
	exists           bool
	tableErr, rowErr error
	calls            int
}

func (q *workspaceTestQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	q.calls++
	if q.calls == 1 {
		return workspaceTestRow{exists: q.exists, err: q.tableErr}
	}
	return workspaceTestRow{err: q.rowErr}
}

func TestReadWorkspaceSchemaDistinguishesMissingAndEmpty(t *testing.T) {
	missing := &workspaceTestQuerier{}
	identity, err := readWorkspaceSchema(context.Background(), missing)
	if !errors.Is(err, ErrWorkspaceSchemaMissing) || identity.Configured || missing.calls != 1 {
		t.Fatalf("missing = %+v %v calls=%d", identity, err, missing.calls)
	}
	empty := &workspaceTestQuerier{exists: true, rowErr: pgx.ErrNoRows}
	identity, err = readWorkspaceSchema(context.Background(), empty)
	if err != nil || identity.Configured || identity.ID != "" || identity.Name != "" || empty.calls != 2 {
		t.Fatalf("empty = %+v %v calls=%d", identity, err, empty.calls)
	}
	denied := errors.New("permission denied")
	_, err = readWorkspaceSchema(context.Background(), &workspaceTestQuerier{tableErr: denied})
	if !errors.Is(err, denied) {
		t.Fatalf("permission = %v", err)
	}
}

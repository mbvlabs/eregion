package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

type promotionRow struct {
	value string
	err   error
}

func (r promotionRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("unexpected scan destination count")
	}
	target, ok := dest[0].(*string)
	if !ok {
		return errors.New("unexpected scan destination type")
	}
	*target = r.value
	return nil
}

type promotionQueryer struct{ row promotionRow }

func (q promotionQueryer) QueryRow(context.Context, string, ...any) pgx.Row { return q.row }

func TestPromotionDeploymentConflictRefusesAnyActiveProductionDeployment(t *testing.T) {
	target := platform.Environment{Name: "production"}
	release := platform.Release{ImageID: "sha256:new"}
	if err := promotionDeploymentConflict(context.Background(), promotionQueryer{row: promotionRow{value: "rel_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, target, release); contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("active different artifact = %v, want conflict", err)
	} else if !strings.Contains(err.Error(), "rel_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb") {
		t.Fatalf("conflict does not name active release: %v", err)
	}
	if err := promotionDeploymentConflict(context.Background(), promotionQueryer{row: promotionRow{err: pgx.ErrNoRows}}, target, release); err != nil {
		t.Fatalf("no active deployment = %v", err)
	}
	if err := promotionDeploymentConflict(context.Background(), promotionQueryer{row: promotionRow{err: errors.New("db down")}}, target, release); contract.Classify(err).Code != contract.Unavailable {
		t.Fatalf("database error = %v, want unavailable", err)
	}
}

func TestValidatePromotionStateRefusesUnsafeSources(t *testing.T) {
	input := &platform.PromoteInput{App: "api", SourceEnvironment: "staging"}
	source := platform.Environment{App: "api", Name: "staging", Node: "main", ServingRelease: "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	target := platform.Environment{App: "api", Name: "production", Node: "main"}
	release := platform.Release{
		ID: "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", App: "api", Environment: "staging",
		Image: "registry.example/api:v1", ImageID: "sha256:abc", Status: platform.ReleaseReady,
	}
	if err := validatePromotionState(input, source, target, release); err != nil {
		t.Fatalf("valid promotion refused: %v", err)
	}

	pruned := release
	prunedAt := time.Now().UTC()
	pruned.PrunedAt = &prunedAt
	for name, tc := range map[string]struct {
		source  platform.Environment
		target  platform.Environment
		release platform.Release
		code    contract.Code
	}{
		"source has no serving release": {
			source: platform.Environment{App: "api", Name: "staging", Node: "main"},
			target: target, release: release, code: contract.PreconditionFailed,
		},
		"source serving changed": {
			source: platform.Environment{App: "api", Name: "staging", Node: "main", ServingRelease: "rel_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
			target: target, release: release, code: contract.Conflict,
		},
		"release belongs to another environment": {
			source: source, target: target,
			release: func() platform.Release { value := release; value.Environment = "qa"; return value }(),
			code:    contract.Conflict,
		},
		"release was pruned": {
			source: source, target: target, release: pruned, code: contract.PreconditionFailed,
		},
		"release is not ready": {
			source: source, target: target,
			release: func() platform.Release { value := release; value.Status = platform.ReleasePending; return value }(),
			code:    contract.PreconditionFailed,
		},
		"release has no immutable image": {
			source: source, target: target,
			release: func() platform.Release { value := release; value.ImageID = ""; return value }(),
			code:    contract.PreconditionFailed,
		},
		"different nodes": {
			source: func() platform.Environment { value := source; value.Node = "worker"; return value }(),
			target: target, release: release, code: contract.PreconditionFailed,
		},
		"cross-app target": {
			source:  source,
			target:  func() platform.Environment { value := target; value.App = "other"; return value }(),
			release: release, code: contract.ValidationFailed,
		},
		"self promotion": {
			source: func() platform.Environment { value := source; value.Name = "production"; return value }(),
			target: target, release: release, code: contract.ValidationFailed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePromotionState(input, tc.source, tc.target, tc.release); contract.Classify(err).Code != tc.code {
				t.Fatalf("refusal = %v, want %s", err, tc.code)
			}
		})
	}
}

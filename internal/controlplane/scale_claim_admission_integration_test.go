package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestIntegrationScaleClaimAdmission(t *testing.T) {
	pool, _ := testPool(t, true)
	f := newPlatformFixture(t, pool, "scaleclaim")
	run, err := New(f.store, slog.New(slog.NewTextHandler(io.Discard, nil)), WorkerConfig{
		Concurrency: 0, TestOnly: true, LocalNodeName: "local",
		NodeStateDir: t.TempDir(), RegistryEndpoint: "127.0.0.1:5000",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	target := "env/" + f.app.Name + "/" + f.env.Name
	claim := entities.TargetClaimID{OperationID: "claim-scale-op", Token: "claim-scale-token"}
	submit := func(key string) error {
		_, err := run.Service.Submit(ctx, platformSubmission(platform.KindProcessScale, target, key, map[string]any{"process": "web", "replicas": 2}))
		return err
	}
	assertConflict := func(key string) {
		t.Helper()
		if err := submit(key); contract.Classify(err).Code != contract.Conflict {
			t.Fatalf("%s: got %v, want conflict", key, err)
		}
		env, found, err := f.store.Platform().Environments().GetByName(ctx, f.app.Name, f.env.Name)
		if err != nil || !found || env.Environment.RevisionID != f.revision.ID {
			t.Fatalf("conflict changed revision: %+v %v %v", env, found, err)
		}
	}

	// A claim acquisition in flight holds the shared short gate.
	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			ok, err := tx.Platform().Locks().TryTargetXact(ctx, targetLockKey(target))
			if err != nil || !ok {
				return contract.NewFault(contract.Internal, "gate unavailable", "")
			}
			close(held)
			<-release
			ok, err = tx.AcquireTargetClaim(ctx, claim, []string{target})
			if err != nil {
				return err
			}
			if !ok {
				return contract.NewFault(contract.Internal, "claim unavailable", "")
			}
			return nil
		})
	}()
	<-held
	assertConflict("gate-held")
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	assertConflict("claim-held")
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.SealTargetClaim(ctx, claim)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	assertConflict("claim-sealed")
	if err := f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.ReleaseTargetClaim(ctx, claim)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	checkScaleAdmissionReverse(t, ctx, pool, f, run.Service, target, submit, assertConflict)
}

func checkScaleAdmissionReverse(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f *platformFixture, s *Service, target string, submit func(string) error, assertConflict func(string)) {
	t.Helper()
	// Legacy workers still hold a session-level lock on the same key.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", targetLockKey(target)); err != nil {
		t.Fatal(err)
	}
	assertConflict("legacy-lock")
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", targetLockKey(target)); err != nil {
		t.Fatal(err)
	}

	// The reverse order must also exclude acquisition until scale commits.
	scaleHeld := make(chan struct{})
	scaleRelease := make(chan struct{})
	s.beforeSubmissionCommit = func() error {
		close(scaleHeld)
		<-scaleRelease
		return nil
	}
	scaleDone := make(chan error, 1)
	go func() { scaleDone <- submit("claim-released") }()
	<-scaleHeld
	err = f.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		ok, err := tx.Platform().Locks().TryTargetXact(ctx, targetLockKey(target))
		if err == nil && ok {
			t.Error("claim acquisition obtained gate while scale was uncommitted")
		}
		return err
	})
	close(scaleRelease)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-scaleDone; err != nil {
		t.Fatalf("scale after release: %v", err)
	}
	env, found, err := f.store.Platform().Environments().GetByName(ctx, f.app.Name, f.env.Name)
	if err != nil || !found || env.Environment.RevisionID == f.revision.ID {
		t.Fatalf("scale did not publish revision: %+v %v %v", env, found, err)
	}
}

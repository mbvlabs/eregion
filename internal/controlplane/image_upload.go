package controlplane

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// ImageUploadLease is the exclusive, identity-fenced right to publish one
// image into an app repository. Its durable claim survives connection and
// process loss; its host lock protects the local node during the upload.
type ImageUploadLease struct {
	AppID           platform.AppID
	App             string
	ImageRepository string

	session     *entities.SessionStore
	store       *entities.Store
	pool        *pgxpool.Pool
	claim       entities.TargetClaimID
	hostRelease engine.Release
	released    bool
}

// AcquireImageUploadLease reads the durable app identity, takes the app target
// durable claim non-blocking, then takes the shared node host lock with
// zero wait, then re-reads the app identity and requires it to be exactly the
// one observed before the locks. No SQL transaction spans the subsequent
// upload. The caller must hold the lease until every upload helper has exited.
func AcquireImageUploadLease(ctx context.Context, config *pgx.ConnConfig, observed RegistryHostIdentity, appName string) (*ImageUploadLease, error) {
	return acquireImageUploadLease(ctx, config, observed, appName, nil)
}

// acquireImageUploadLease is the same admission with a test-only barrier after
// the initial identity read. Production callers always pass nil.
func acquireImageUploadLease(ctx context.Context, config *pgx.ConnConfig, observed RegistryHostIdentity, appName string, afterInitialRead func(context.Context) error) (*ImageUploadLease, error) {
	if config == nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the node has no control database connection", "Start the node with its managed control database configuration before uploading an image.")
	}
	if err := observed.Validate(); err != nil {
		return nil, err
	}
	if err := platform.ValidateName(appName); err != nil {
		return nil, err
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "the control database is unavailable", "Retry the upload after the node's control database is reachable.")
	}
	session := entities.NewSessionStore(conn)
	closeWithError := func(ctx context.Context, cause error) (*ImageUploadLease, error) {
		_ = session.Close(context.WithoutCancel(ctx))
		return nil, cause
	}
	binding, err := VerifyWorkspaceRegistryHost(ctx, session.Workspace(), observed)
	if err != nil {
		return closeWithError(ctx, err)
	}
	hostLockPath := node.HostLockPath(binding.StateDir)
	if strings.TrimSpace(hostLockPath) == "" {
		return closeWithError(ctx, contract.NewFault(contract.PreconditionFailed, "the verified registry host lock path is empty", "Restore the bound node state directory before uploading."))
	}
	initial, err := readImageUploadIdentity(ctx, session, appName)
	if err != nil {
		return closeWithError(ctx, err)
	}
	if afterInitialRead != nil {
		if err := afterInitialRead(ctx); err != nil {
			return closeWithError(ctx, err)
		}
	}
	lease, err := acquireImageUploadClaim(ctx, config, appName)
	if err != nil {
		return closeWithError(ctx, err)
	}
	lease.session = session
	hostRelease, err := engine.Acquire(ctx, hostLockPath, engine.LockExclusive, 0)
	if err != nil {
		lease.Release(ctx)
		if errors.Is(err, engine.ErrLockHeld) {
			return nil, contract.NewFault(contract.Unavailable, "another host mutation is running on the node", "Wait for the running build, apply, garbage collection or app deletion to finish, then retry the upload.")
		}
		return nil, contract.NewFault(contract.Unavailable, "the node host lock is not available", err.Error())
	}
	lease.hostRelease = hostRelease
	after, err := readImageUploadIdentity(ctx, session, appName)
	if err != nil {
		lease.Release(ctx)
		return nil, err
	}
	if after.AppID != initial.AppID || after.App != initial.App || after.ImageRepository != initial.ImageRepository {
		lease.Release(ctx)
		return nil, contract.NewFault(contract.Conflict, "the application was deleted or recreated during upload admission", "The upload was refused before reading the image payload; retry against the current application identity.")
	}
	if after.ImageRepository == "" {
		lease.Release(ctx)
		return nil, contract.NewFault(contract.PreconditionFailed, "application has no image repository", "Recreate the app under the app-level image repository contract before uploading an image.")
	}
	if _, err := platform.NormalizeImageRepository(after.ImageRepository); err != nil {
		lease.Release(ctx)
		return nil, err
	}
	lease.AppID, lease.App, lease.ImageRepository = after.AppID, after.App, after.ImageRepository
	return lease, nil
}

func acquireImageUploadClaim(ctx context.Context, config *pgx.ConnConfig, appName string) (*ImageUploadLease, error) {
	poolConfig, err := pgxpool.ParseConfig(config.ConnString())
	if err != nil {
		return nil, err
	}
	poolConfig.ConnConfig = config.Copy()
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, err
	}
	store, err := entities.NewStore(pool)
	if err != nil {
		pool.Close()
		return nil, err
	}
	op, err := contract.NewOperationID()
	if err != nil {
		pool.Close()
		return nil, err
	}
	token, err := contract.NewOperationID()
	if err != nil {
		pool.Close()
		return nil, err
	}
	claim := entities.TargetClaimID{OperationID: op, Token: string(token)}
	acquired := false
	err = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		var err error
		acquired, err = acquirePlatformTargetClaimTx(ctx, tx, claim, []string{"app/" + appName, "registry"})
		return err
	})
	if err != nil {
		// An uncertain commit may have installed the claim. Never perform work
		// or reuse this token; the claim remains for explicit recovery.
		pool.Close()
		return nil, contract.NewFault(contract.Unavailable, "the application upload claim is unavailable", "Retry after the control database is reachable; an uncertain claim requires recovery.")
	}
	if !acquired {
		pool.Close()
		return nil, contract.NewFault(contract.Conflict, "the application is being deleted or changed", "Wait for the current application mutation to finish, then retry the upload.")
	}
	return &ImageUploadLease{store: store, pool: pool, claim: claim}, nil
}

// Release is only safe after all admitted upload effects and helpers have
// finished. A failed seal or release leaves the durable claim in place.
func (l *ImageUploadLease) Release(ctx context.Context) {
	l.close(ctx, true)
}

// Abandon retains the claim when effects may still be running.
func (l *ImageUploadLease) Abandon(ctx context.Context) {
	l.close(ctx, false)
}

func (l *ImageUploadLease) close(ctx context.Context, finished bool) {
	if l == nil || l.released {
		return
	}
	l.released = true
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if finished && l.store != nil {
		sealed := false
		err := l.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			var err error
			sealed, err = tx.SealTargetClaim(ctx, l.claim)
			return err
		})
		if err == nil && sealed {
			_ = l.store.WithinTx(ctx, func(tx *entities.StoreTx) error {
				_, err := tx.ReleaseTargetClaim(ctx, l.claim)
				return err
			})
		}
	}
	if l.hostRelease != nil {
		l.hostRelease()
		l.hostRelease = nil
	}
	if l.session != nil {
		_ = l.session.Close(ctx)
		l.session = nil
	}
	if l.pool != nil {
		l.pool.Close()
		l.pool = nil
	}
}

type imageUploadIdentity struct {
	AppID           platform.AppID
	App             string
	ImageRepository string
}

func readImageUploadIdentity(ctx context.Context, session *entities.SessionStore, appName string) (imageUploadIdentity, error) {
	var identity imageUploadIdentity
	upload, found, err := session.Apps().UploadIdentity(ctx, appName)
	if err != nil {
		return identity, contract.NewFault(contract.Unavailable, "the application identity is unavailable", "Retry after the control database is reachable.")
	}
	if !found {
		return identity, contract.NewFault(contract.NotFound, "application not found", "Create the application before uploading an image.")
	}
	identity.AppID, identity.App, identity.ImageRepository = upload.ID, upload.Name, upload.ImageRepository
	return identity, nil
}

// verifyLocalRegistryHost re-verifies the observed public host identity
// against the immutable workspace binding. Direct service callers cannot bypass
// this by constructing a Service or by holding only a local name.
func (s *Service) verifyLocalRegistryHost(ctx context.Context) (RegistryHostIdentity, error) {
	if s == nil || s.entities == nil {
		return RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed, "the control database is unavailable", "Registry mutation requires the control-plane database.")
	}
	if s.registryHostIdentity == nil {
		return RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"this process has no verified local registry host identity",
			"Start the node through its managed composition so the public host key and state directory can be verified.")
	}
	return VerifyWorkspaceRegistryHost(ctx, s.entities.Workspace(), *s.registryHostIdentity)
}

// acquireRegistryHostLock takes the actual shared node host lock and waits for
// an in-flight upload or other host mutation to finish. Final app metadata
// deletion calls it only after every node effect has completed, so it never
// holds the host lock around the node work that itself needs it.
func (s *Service) acquireRegistryHostLock(ctx context.Context) (engine.Release, error) {
	binding, err := s.verifyLocalRegistryHost(ctx)
	if err != nil {
		return nil, err
	}
	path := node.HostLockPath(binding.StateDir)
	if strings.TrimSpace(path) == "" {
		return nil, contract.NewFault(contract.PreconditionFailed, "the verified registry host lock path is empty", "Restore the bound node state directory before deleting.")
	}
	for {
		release, err := engine.Acquire(ctx, path, engine.LockExclusive, 0)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, engine.ErrLockHeld) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, contract.NewFault(contract.Unavailable, "the node host lock is not available", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

package controlplane

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// ImageUploadLease is the exclusive, identity-fenced right to publish one
// image into an app repository. It holds the app session advisory lock and the
// node host lock through the complete upload and all child processes. The
// database connection is dedicated to the lease; it is never a pooled
// connection returned while a session lock might still be held.
type ImageUploadLease struct {
	AppID           platform.AppID
	App             string
	ImageRepository string

	conn        *pgx.Conn
	advisoryKey int64
	hostRelease engine.Release
	released    bool
}

// AcquireImageUploadLease reads the durable app identity, takes the app target
// session advisory lock non-blocking, then takes the shared node host lock with
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
	closeWithError := func(cause error) (*ImageUploadLease, error) {
		_ = conn.Close(context.Background())
		return nil, cause
	}
	binding, err := VerifyWorkspaceRegistryHost(ctx, conn, observed)
	if err != nil {
		return closeWithError(err)
	}
	hostLockPath := node.HostLockPath(binding.StateDir)
	if strings.TrimSpace(hostLockPath) == "" {
		return closeWithError(contract.NewFault(contract.PreconditionFailed, "the verified registry host lock path is empty", "Restore the bound node state directory before uploading."))
	}
	initial, err := readImageUploadIdentity(ctx, conn, appName)
	if err != nil {
		return closeWithError(err)
	}
	if afterInitialRead != nil {
		if err := afterInitialRead(ctx); err != nil {
			return closeWithError(err)
		}
	}
	key := targetLockKey("app/" + appName)
	var advisory bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&advisory); err != nil {
		return closeWithError(contract.NewFault(contract.Unavailable, "the application upload lock is unavailable", "Retry after the control database is reachable."))
	}
	if !advisory {
		return closeWithError(contract.NewFault(contract.Conflict, "the application is being deleted or changed", "Wait for the current application mutation to finish, then retry the upload."))
	}
	releaseAdvisory := func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
	}
	hostRelease, err := engine.Acquire(ctx, hostLockPath, engine.LockExclusive, 0)
	if err != nil {
		releaseAdvisory()
		if errors.Is(err, engine.ErrLockHeld) {
			return closeWithError(contract.NewFault(contract.Unavailable, "another host mutation is running on the node", "Wait for the running build, apply, garbage collection or app deletion to finish, then retry the upload."))
		}
		return closeWithError(contract.NewFault(contract.Unavailable, "the node host lock is not available", err.Error()))
	}
	releaseBoth := func() {
		hostRelease()
		releaseAdvisory()
	}
	after, err := readImageUploadIdentity(ctx, conn, appName)
	if err != nil {
		releaseBoth()
		return closeWithError(err)
	}
	if after.AppID != initial.AppID || after.App != initial.App || after.ImageRepository != initial.ImageRepository {
		releaseBoth()
		return closeWithError(contract.NewFault(contract.Conflict, "the application was deleted or recreated during upload admission", "The upload was refused before reading the image payload; retry against the current application identity."))
	}
	if after.ImageRepository == "" {
		releaseBoth()
		return closeWithError(contract.NewFault(contract.PreconditionFailed, "application has no image repository", "Recreate the app under the app-level image repository contract before uploading an image."))
	}
	if _, err := platform.NormalizeImageRepository(after.ImageRepository); err != nil {
		releaseBoth()
		return closeWithError(err)
	}
	return &ImageUploadLease{
		AppID: after.AppID, App: after.App, ImageRepository: after.ImageRepository,
		conn: conn, advisoryKey: key, hostRelease: hostRelease,
	}, nil
}

// Release drops the host lock first, then the app session advisory lock, then
// closes the dedicated connection. If the advisory unlock is uncertain the
// connection is closed anyway; it is never returned to a pool while a session
// lock might survive.
func (l *ImageUploadLease) Release() {
	if l == nil || l.released {
		return
	}
	l.released = true
	if l.hostRelease != nil {
		l.hostRelease()
		l.hostRelease = nil
	}
	if l.conn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		var unlocked bool
		_ = l.conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, l.advisoryKey).Scan(&unlocked)
		cancel()
		// The connection is dedicated to this lease. Close/discard it in every
		// case; an uncertain unlock is never returned to a pool.
		_ = l.conn.Close(context.Background())
		l.conn = nil
	}
}

type imageUploadIdentity struct {
	AppID           platform.AppID
	App             string
	ImageRepository string
}

func readImageUploadIdentity(ctx context.Context, conn *pgx.Conn, appName string) (imageUploadIdentity, error) {
	var identity imageUploadIdentity
	err := conn.QueryRow(ctx, `SELECT id,name,image_repository FROM eregion_platform_apps WHERE name=$1`, appName).
		Scan(&identity.AppID, &identity.App, &identity.ImageRepository)
	if errors.Is(err, pgx.ErrNoRows) {
		return identity, contract.NewFault(contract.NotFound, "application not found", "Create the application before uploading an image.")
	}
	if err != nil {
		return identity, contract.NewFault(contract.Unavailable, "the application identity is unavailable", "Retry after the control database is reachable.")
	}
	return identity, nil
}

// verifyLocalRegistryHost re-verifies the observed public host identity
// against the immutable workspace binding. Direct service callers cannot bypass
// this by constructing a Service or by holding only a local name.
func (s *Service) verifyLocalRegistryHost(ctx context.Context) (RegistryHostIdentity, error) {
	if s == nil || s.pool == nil {
		return RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed, "the control database is unavailable", "Registry mutation requires the control-plane database.")
	}
	if s.registryHostIdentity == nil {
		return RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"this process has no verified local registry host identity",
			"Start the node through its managed composition so the public host key and state directory can be verified.")
	}
	return VerifyWorkspaceRegistryHost(ctx, s.pool, *s.registryHostIdentity)
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

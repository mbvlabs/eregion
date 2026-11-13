package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// seededRelease is a private test-only release constructor result. It records
// the canonical image, config identity and the operation that would have
// admitted the artifact, without going through the removed
// platform.release.create action or a public submission path.
type seededRelease struct {
	ID          platform.ReleaseID
	OperationID contract.OperationID
	Image       string
	ImageID     string
}

type seededReleaseOptions struct {
	Command        *platform.CommandSpec
	ImageID        string
	OperationState contract.State
	Attempt        uint32
	Pending        bool
}

// withSeededReleaseCommand attaches the release-scoped command used by the
// release-command recovery tests.
func withSeededReleaseCommand(command platform.CommandSpec) func(*seededReleaseOptions) {
	return func(options *seededReleaseOptions) {
		options.Command = &command
	}
}

// withSeededReleaseImageID overrides the recorded config digest so a fixture
// can match the node's stubbed image inspection.
func withSeededReleaseImageID(imageID string) func(*seededReleaseOptions) {
	return func(options *seededReleaseOptions) {
		options.ImageID = imageID
	}
}

// withSeededReleaseOperation keeps the admission operation non-terminal so a
// test can drive one internal node step under it.
func withSeededReleaseOperation(state contract.State, attempt uint32) func(*seededReleaseOptions) {
	return func(options *seededReleaseOptions) {
		options.OperationState = state
		options.Attempt = attempt
	}
}

// withSeededReleasePending leaves the release pending, matching an artifact
// that still needs its image inspected before it becomes ready.
func withSeededReleasePending() func(*seededReleaseOptions) {
	return func(options *seededReleaseOptions) {
		options.Pending = true
	}
}

// seedArtifactRelease uses the production artifact-admission constructor to
// create the same canonical release shape an explicit deploy records: a
// manifest digest reference, an expected config digest, requested-selector
// provenance and its operation association. Tests that focus on later
// deployment, promotion, cleanup or release-command semantics use this instead
// of resolving a real registry artifact.
func seedArtifactRelease(t *testing.T, s *Service, app, environment, image string, options ...func(*seededReleaseOptions)) seededRelease {
	t.Helper()
	ctx := context.Background()
	settings := seededReleaseOptions{OperationState: contract.Succeeded, Attempt: 1}
	for _, option := range options {
		option(&settings)
	}
	manifest := "sha256:" + hex.EncodeToString(sha256Sum("manifest:"+image))
	config := "sha256:" + hex.EncodeToString(sha256Sum("config:"+image))
	imageID := settings.ImageID
	if imageID == "" {
		imageID = config
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	appRecord, err := appByNameForUpdateTx(ctx, tx, app)
	if err != nil {
		t.Fatal(err)
	}
	env, err := environmentByNameTx(ctx, tx, app, environment)
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	inputHash := hex.EncodeToString(sha256Sum("release-fixture:" + image))
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,0,0,clock_timestamp(),clock_timestamp(),'')`,
		operationID, platform.KindDeploy, "env/"+app+"/"+environment,
		testActor.Kind, testActor.Subject, string(operationID), inputHash,
		`{"seed":"release-fixture"}`,
		string(settings.OperationState), settings.Attempt); err != nil {
		t.Fatal(err)
	}
	canonical, err := platform.CanonicalArtifactReference("registry.example", appRecord.ImageRepository, manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact := &resolvedArtifact{
		Kind: "image", Requested: image,
		Registry: "registry.example", Repository: appRecord.ImageRepository,
		ManifestDigest: manifest, ConfigDigest: config, CanonicalImage: canonical,
		AppID: appRecord.ID, App: appRecord.Name, AppRepository: appRecord.ImageRepository,
		EnvironmentID: env.ID, Environment: env.Name, RevisionID: env.RevisionID, Node: env.Node,
	}
	releaseID, err := s.createArtifactReleaseTx(ctx, tx, operationID, env, artifact)
	if err != nil {
		t.Fatal(err)
	}
	var command any
	if settings.Command != nil {
		raw, err := json.Marshal(settings.Command)
		if err != nil {
			t.Fatal(err)
		}
		command = json.RawMessage(raw)
	}
	status := "ready"
	completedAt := "clock_timestamp()"
	if settings.Pending {
		status = "pending"
		completedAt = "NULL"
	}
	query := `UPDATE eregion_platform_releases
        SET status=$2,image_id=$3,command=$4,completed_at=` + completedAt + ` WHERE id=$1`
	if _, err := tx.Exec(ctx, query, releaseID, status, imageID, command); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return seededRelease{ID: releaseID, OperationID: operationID, Image: canonical, ImageID: imageID}
}

func sha256Sum(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

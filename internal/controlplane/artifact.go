package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"runtime"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/registrymeta"
)

// artifactRequest is one resolved deploy-time artifact selector. Repository is
// the app's durable repository; the registry endpoint is the node's configured
// private registry for tag/digest selectors.
type artifactRequest struct {
	Kind       string
	Requested  string
	Repository string
	Registry   string
	Tag        string
	Digest     string
	Image      string
	Arch       string
}

// resolvedArtifact is the immutable admission result. ManifestDigest identifies
// the manifest; ConfigDigest is the expected Docker image ID. The two are never
// treated as the same value.
type resolvedArtifact struct {
	Kind           string
	Requested      string
	Registry       string
	Repository     string
	AppRepository  string
	ManifestDigest string
	ConfigDigest   string
	CanonicalImage string
	AppID          platform.AppID
	App            string
	EnvironmentID  platform.EnvironmentID
	Environment    string
	RevisionID     platform.RevisionID
	Node           string
	Platform       string
}

func (s *Service) resolveSubmissionArtifact(ctx context.Context, normalized platform.Normalized) (*resolvedArtifact, error) {
	input, ok := normalized.Input.(*platform.DeployInput)
	if !ok {
		return nil, nil
	}
	if input.ReleaseID != "" || (input.ImageTag == "" && input.ImageDigest == "" && input.Image == "") {
		return nil, nil
	}
	app, err := s.appByName(ctx, input.App)
	if err != nil {
		return nil, err
	}
	if app.ImageRepository == "" {
		return nil, contract.NewFault(contract.PreconditionFailed,
			"application has no image repository",
			"This app predates app-level image repositories; recreate it before deploying an artifact.")
	}
	env, err := s.environment(ctx, input.App, input.Environment)
	if err != nil {
		return nil, err
	}
	node, err := s.node(ctx, env.Node)
	if err != nil {
		return nil, err
	}
	arch := strings.TrimSpace(node.Arch)
	if arch == "" {
		arch = runtime.GOARCH
	}
	request := artifactRequest{
		Repository: app.ImageRepository, Registry: s.registryEndpoint,
		Tag: input.ImageTag, Digest: input.ImageDigest, Image: input.Image, Arch: arch,
	}
	switch {
	case input.ImageTag != "":
		request.Kind = "image-tag"
		request.Requested = input.ImageTag
	case input.ImageDigest != "":
		request.Kind = "image-digest"
		request.Requested = input.ImageDigest
	default:
		request.Kind = "image"
		request.Requested = input.Image
	}
	resolve := s.resolveArtifactFn
	if resolve == nil {
		resolve = s.resolveRemoteArtifact
	}
	resolved, err := resolve(ctx, request)
	if err != nil {
		return nil, err
	}
	resolved.AppID = app.ID
	resolved.App = app.Name
	resolved.AppRepository = app.ImageRepository
	resolved.EnvironmentID = env.ID
	resolved.Environment = env.Name
	resolved.RevisionID = env.RevisionID
	resolved.Node = env.Node
	return &resolved, nil
}

// resolveRemoteArtifact performs bounded, read-only, anonymous registry
// metadata reads. It never runs inside a SQL transaction. A private repository
// selector uses only the configured loopback registry over HTTP; an external
// fully qualified reference uses HTTPS with anonymous bearer-token auth when
// the registry asks for it.
func (s *Service) resolveRemoteArtifact(ctx context.Context, request artifactRequest) (resolvedArtifact, error) {
	registry, repository, reference, private, err := artifactSelector(request)
	if err != nil {
		return resolvedArtifact{}, err
	}
	if reference == "" {
		return resolvedArtifact{}, contract.NewFault(contract.ValidationFailed, "artifact selector is empty", "Name a tag or digest explicitly; there is no implicit latest.")
	}
	_, base, err := registryBase(registry, private)
	if err != nil {
		return resolvedArtifact{}, err
	}
	client := &registrymeta.Client{HTTPClient: registrymeta.NewHTTPClient(private), Base: base, AnonymousAuth: !private}
	if s.registryHTTPClient != nil {
		client.HTTPClient = s.registryHTTPClient(private)
	}
	client.RedirectHTTPClient = s.registryRedirectHTTPClient
	client.LookupIP = s.registryLookupIP
	resolution, err := client.Resolve(ctx, repository, reference, registrymeta.Options{Architecture: request.Arch, AllowIndex: true})
	if err != nil {
		return resolvedArtifact{}, err
	}
	manifestDigest, configDigest, mediaType := resolution.ManifestDigest, resolution.ConfigDigest, resolution.MediaType
	if !platform.IsManifestDigest(manifestDigest) || !platform.IsManifestDigest(configDigest) {
		return resolvedArtifact{}, contract.NewFault(contract.PreconditionFailed, "the registry returned an unsupported manifest identity", "Use a schema-2 image manifest with a sha256 config digest.")
	}
	if manifestDigest == configDigest {
		return resolvedArtifact{}, contract.NewFault(contract.PreconditionFailed, "the registry returned the manifest digest as the image config digest", "Do not accept an image whose config identity is not distinct.")
	}
	canonical, err := platform.CanonicalArtifactReference(registry, repository, manifestDigest)
	if err != nil {
		return resolvedArtifact{}, err
	}
	platforms := ""
	if mediaType != "" {
		platforms = mediaType
	}
	return resolvedArtifact{
		Kind: request.Kind, Requested: request.Requested, Registry: registry,
		Repository: repository, ManifestDigest: manifestDigest, ConfigDigest: configDigest,
		CanonicalImage: canonical, Platform: platforms,
	}, nil
}

// artifactSelector resolves the requested selector to its registry, repository
// and immutable reference, and reports whether the private loopback path applies.
func artifactSelector(request artifactRequest) (registry, repository, reference string, private bool, err error) {
	repository = request.Repository
	switch request.Kind {
	case "image-tag":
		registry = platform.CanonicalRegistryAddress(request.Registry)
		reference = strings.TrimSpace(request.Tag)
		if registry == "" {
			return "", "", "", false, contract.NewFault(contract.PreconditionFailed, "the node has no configured private registry endpoint", "Start the worker with its managed loopback registry endpoint before deploying a private artifact.")
		}
		private = true
	case "image-digest":
		registry = platform.CanonicalRegistryAddress(request.Registry)
		reference = strings.ToLower(strings.TrimSpace(request.Digest))
		if registry == "" {
			return "", "", "", false, contract.NewFault(contract.PreconditionFailed, "the node has no configured private registry endpoint", "Start the worker with its managed loopback registry endpoint before deploying a private artifact.")
		}
		private = true
	case "image":
		parsed, parseErr := platform.ParseFullyQualifiedImage(request.Image)
		if parseErr != nil {
			return "", "", "", false, parseErr
		}
		registry = platform.CanonicalRegistryAddress(parsed.Registry)
		if parseErr := platform.ValidateImage(parsed.Original); parseErr != nil {
			return "", "", "", false, parseErr
		}
		if canonical := platform.CanonicalRegistryAddress(request.Registry); canonical != "" && canonical == registry {
			if parsed.Repository != request.Repository {
				return "", "", "", false, contract.NewFault(contract.ValidationFailed,
					"the image names another repository in this private registry",
					"Use the app's repository "+request.Repository+", or select --image-tag/--image-digest.")
			}
			private = true
		}
		repository = parsed.Repository
		if parsed.Digest != "" {
			reference = parsed.Digest
		} else {
			reference = parsed.Tag
		}
	default:
		return "", "", "", false, contract.NewFault(contract.Internal, "unsupported artifact selector", "Report the command and binary version.")
	}
	return registry, repository, reference, private, nil
}

func registryBase(registry string, private bool) (scheme, base string, err error) {
	if registry == "" {
		return "", "", contract.NewFault(contract.ValidationFailed, "the image reference has no registry host", "Use a fully qualified registry/repository:tag reference.")
	}
	host := registry
	if parsedHost, _, splitErr := net.SplitHostPort(registry); splitErr == nil {
		host = parsedHost
	}
	if private {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return "", "", contract.NewFault(contract.PreconditionFailed, "the private registry endpoint is not loopback", "The private artifact path uses only the node's loopback registry over HTTP.")
		}
		return "http", "http://" + registry, nil
	}
	return "https", "https://" + registry, nil
}

// createArtifactReleaseTx rechecks the app/repository/environment placement
// facts under the admission transaction, then records the canonical immutable
// release, pending deployment and initial state in one commit. It never reads
// the registry.
func (s *Service) createArtifactReleaseTx(ctx context.Context, tx *entities.StoreTx, opID contract.OperationID, env platform.Environment, artifact *resolvedArtifact) (platform.ReleaseID, error) {
	if _, err := lockPointerEnvironmentTx(ctx, tx, env.ID); err != nil {
		return "", err
	}
	app, err := appByNameForUpdateTx(ctx, tx, env.App)
	if err != nil {
		return "", err
	}
	if app.ID != artifact.AppID || app.ImageRepository != artifact.AppRepository {
		return "", contract.NewFault(contract.Conflict, "the application image repository changed during admission", "Retry the deploy; an app's repository is immutable.")
	}
	if env.ID != artifact.EnvironmentID || env.RevisionID != artifact.RevisionID {
		return "", contract.NewFault(contract.Conflict, "the environment changed during admission", "Retry the deploy from the environment's current revision.")
	}
	if env.Node != artifact.Node {
		return "", contract.NewFault(contract.Conflict, "the environment placement changed during admission", "Retry the deploy so the artifact is resolved for the current application node.")
	}
	if err := platform.ValidateImage(artifact.CanonicalImage); err != nil {
		return "", err
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		return "", unavailable()
	}
	source, _ := json.Marshal(platform.Source{
		Type: "image", Image: artifact.Requested,
		Selector: artifact.Requested, SelectorKind: artifact.Kind,
	})
	if _, err := tx.Platform().Releases().Insert(ctx, entities.ReleaseInsert{
		ID: releaseID, OperationID: opID, AppID: app.ID, EnvironmentID: env.ID,
		Image: artifact.CanonicalImage, ImageID: artifact.ConfigDigest, Source: source,
		Status: platform.ReleasePending,
	}); err != nil {
		return "", platformWriteError(err, "artifact release could not be recorded")
	}
	return releaseID, nil
}

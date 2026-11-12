package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const (
	artifactManifestBodyLimit = 4 << 20
	artifactTokenBodyLimit    = 64 << 10
	artifactRequestTimeout    = 20 * time.Second
)

var artifactManifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

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
		resolve = resolveRemoteArtifact
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
func resolveRemoteArtifact(ctx context.Context, request artifactRequest) (resolvedArtifact, error) {
	registry := ""
	repository := request.Repository
	reference := ""
	private := false
	switch request.Kind {
	case "image-tag":
		registry = platform.CanonicalRegistryAddress(request.Registry)
		reference = strings.TrimSpace(request.Tag)
		if registry == "" {
			return resolvedArtifact{}, contract.NewFault(contract.PreconditionFailed, "the node has no configured private registry endpoint", "Start the worker with its managed loopback registry endpoint before deploying a private artifact.")
		}
		private = true
	case "image-digest":
		registry = platform.CanonicalRegistryAddress(request.Registry)
		reference = strings.ToLower(strings.TrimSpace(request.Digest))
		if registry == "" {
			return resolvedArtifact{}, contract.NewFault(contract.PreconditionFailed, "the node has no configured private registry endpoint", "Start the worker with its managed loopback registry endpoint before deploying a private artifact.")
		}
		private = true
	case "image":
		parsed, err := platform.ParseFullyQualifiedImage(request.Image)
		if err != nil {
			return resolvedArtifact{}, err
		}
		registry = platform.CanonicalRegistryAddress(parsed.Registry)
		if parseErr := platform.ValidateImage(parsed.Original); parseErr != nil {
			return resolvedArtifact{}, parseErr
		}
		if canonical := platform.CanonicalRegistryAddress(request.Registry); canonical != "" && canonical == registry {
			if parsed.Repository != request.Repository {
				return resolvedArtifact{}, contract.NewFault(contract.ValidationFailed,
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
		return resolvedArtifact{}, contract.NewFault(contract.Internal, "unsupported artifact selector", "Report the command and binary version.")
	}
	if reference == "" {
		return resolvedArtifact{}, contract.NewFault(contract.ValidationFailed, "artifact selector is empty", "Name a tag or digest explicitly; there is no implicit latest.")
	}
	_, base, err := registryBase(registry, private)
	if err != nil {
		return resolvedArtifact{}, err
	}
	client := &registryMetadataClient{client: newRegistryHTTPClient(private), base: base, anonymousAuth: !private}
	manifestDigest, configDigest, mediaType, err := client.resolve(ctx, repository, reference, request.Arch)
	if err != nil {
		return resolvedArtifact{}, err
	}
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

type registryMetadataClient struct {
	client        *http.Client
	base          string
	anonymousAuth bool
}

type distributionManifest struct {
	SchemaVersion int    `json:"schemaVersion"`
	MediaType     string `json:"mediaType"`
	Config        struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Platform  struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
}

func (c *registryMetadataClient) resolve(ctx context.Context, repository, reference, arch string) (string, string, string, error) {
	body, mediaType, err := c.getManifest(ctx, repository, reference)
	if err != nil {
		return "", "", "", err
	}
	manifestDigest := "sha256:" + sha256HexBytes(body)
	if platform.IsManifestDigest(reference) && manifestDigest != strings.ToLower(strings.TrimSpace(reference)) {
		return "", "", "", contract.NewFault(contract.PreconditionFailed, "the registry returned different manifest bytes than the requested digest", "Do not admit an artifact that does not match its requested digest; retry against the registry.")
	}
	var manifest distributionManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return "", "", "", contract.NewFault(contract.PreconditionFailed, "the registry returned an unreadable manifest", "Use a schema-2 image manifest.")
	}
	if isManifestList(mediaType, manifest) {
		digest := selectManifestForArch(manifest, arch)
		if digest == "" {
			return "", "", "", contract.NewFault(contract.PreconditionFailed, "the image index has no manifest for the target node architecture", "Use a single-platform image or an index that contains the node's architecture.")
		}
		body, mediaType, err = c.getManifest(ctx, repository, digest)
		if err != nil {
			return "", "", "", err
		}
		childDigest := "sha256:" + sha256HexBytes(body)
		if childDigest != digest {
			return "", "", "", contract.NewFault(contract.PreconditionFailed, "the image index child manifest does not match its descriptor digest", "Do not admit a child manifest that differs from the index descriptor; retry against the registry.")
		}
		manifestDigest = childDigest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return "", "", "", contract.NewFault(contract.PreconditionFailed, "the registry returned an unreadable child manifest", "Use a schema-2 image manifest.")
		}
	}
	if manifest.SchemaVersion != 2 || isManifestList(mediaType, manifest) {
		return "", "", "", contract.NewFault(contract.PreconditionFailed, "the registry did not return a schema-2 image manifest", "Use a single-platform schema-2 image.")
	}
	config := strings.ToLower(strings.TrimSpace(manifest.Config.Digest))
	if !platform.IsManifestDigest(config) {
		return "", "", "", contract.NewFault(contract.PreconditionFailed, "the image manifest has no sha256 config digest", "Use a schema-2 image manifest.")
	}
	return manifestDigest, config, mediaType, nil
}

func isManifestList(mediaType string, manifest distributionManifest) bool {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if strings.HasPrefix(mediaType, "application/vnd.oci.image.index.v1+json") ||
		strings.HasPrefix(mediaType, "application/vnd.docker.distribution.manifest.list.v2+json") {
		return true
	}
	return manifest.SchemaVersion == 2 && len(manifest.Manifests) > 0 && manifest.Config.Digest == ""
}

func selectManifestForArch(manifest distributionManifest, arch string) string {
	for _, candidate := range manifest.Manifests {
		if candidate.Platform.OS == "linux" && candidate.Platform.Architecture == arch {
			digest := strings.ToLower(strings.TrimSpace(candidate.Digest))
			if platform.IsManifestDigest(digest) {
				return digest
			}
		}
	}
	return ""
}

func (c *registryMetadataClient) getManifest(ctx context.Context, repository, reference string) ([]byte, string, error) {
	manifestURL := c.base + "/v2/" + escapeRepository(repository) + "/manifests/" + url.PathEscape(reference)
	response, err := c.do(ctx, manifestURL)
	if err != nil {
		return nil, "", err
	}
	defer closeArtifactResponse(response)
	if response.StatusCode == http.StatusUnauthorized && c.anonymousAuth {
		token, tokenErr := c.requestAnonymousToken(ctx, response.Header.Get("WWW-Authenticate"))
		if tokenErr != nil {
			return nil, "", tokenErr
		}
		response, err = c.doWithToken(ctx, manifestURL, token)
		if err != nil {
			return nil, "", err
		}
		defer closeArtifactResponse(response)
	}
	switch response.StatusCode {
	case http.StatusOK:
		body, err := io.ReadAll(io.LimitReader(response.Body, artifactManifestBodyLimit+1))
		if err != nil {
			return nil, "", contract.NewFault(contract.Unavailable, "the registry manifest read failed", "Retry after the registry is reachable.")
		}
		if len(body) > artifactManifestBodyLimit {
			return nil, "", contract.NewFault(contract.PreconditionFailed, "the registry manifest is larger than the bounded read", "Use a normal image manifest under 4 MiB.")
		}
		return body, response.Header.Get("Content-Type"), nil
	case http.StatusNotFound:
		return nil, "", contract.NewFault(contract.NotFound, "the registry has no manifest for the selected artifact", "Push the image, or choose a tag or digest that exists.")
	default:
		return nil, "", contract.NewFault(contract.Unavailable, "the registry answered the manifest lookup with "+response.Status, "Retry after the registry is reachable.")
	}
}

func (c *registryMetadataClient) do(ctx context.Context, target string) (*http.Response, error) {
	return c.doWithToken(ctx, target, "")
}

func (c *registryMetadataClient) doWithToken(ctx context.Context, target, token string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the artifact reference is invalid", "Use registry/repository:tag or registry/repository@sha256:….")
	}
	request.Header.Set("Accept", artifactManifestAccept)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := c.client.Do(request)
	if err != nil {
		if errors.Is(err, errRegistryRedirectRefused) {
			return nil, contract.NewFault(contract.PreconditionFailed, "the registry returned a redirect", "Registry metadata reads never follow redirects; use the canonical registry endpoint.")
		}
		return nil, contract.NewFault(contract.Unavailable, "the registry is not reachable", "Retry after the registry endpoint is reachable; no Docker credentials are used.")
	}
	return response, nil
}

func (c *registryMetadataClient) requestAnonymousToken(ctx context.Context, challenge string) (string, error) {
	realm, params, ok := parseBearerChallenge(challenge)
	if !ok {
		return "", contract.NewFault(contract.PreconditionFailed, "the external registry requires credentials", "Only public anonymous images are supported; private external registries are not mirrored or credentialed.")
	}
	u, err := validateRegistryTokenRealm(realm)
	if err != nil {
		return "", err
	}
	query := u.Query()
	if service := params["service"]; service != "" {
		query.Set("service", service)
	}
	if scope := params["scope"]; scope != "" {
		query.Set("scope", scope)
	}
	u.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "the registry authentication request is invalid", "Retry with a public image.")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "the registry token endpoint is not reachable", "Retry after the registry is reachable.")
	}
	defer closeArtifactResponse(response)
	if response.StatusCode != http.StatusOK {
		return "", contract.NewFault(contract.PreconditionFailed, "the external registry refused anonymous access", "Only public anonymous images are supported.")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, artifactTokenBodyLimit+1))
	if err != nil || len(body) > artifactTokenBodyLimit {
		return "", contract.NewFault(contract.Unavailable, "the registry token response is unreadable", "Retry after the registry is reachable.")
	}
	var token struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &token) != nil {
		return "", contract.NewFault(contract.Unavailable, "the registry token response is unreadable", "Retry after the registry is reachable.")
	}
	value := token.Token
	if value == "" {
		value = token.AccessToken
	}
	if value == "" {
		return "", contract.NewFault(contract.PreconditionFailed, "the external registry did not issue an anonymous token", "Only public anonymous images are supported.")
	}
	return value, nil
}

func parseBearerChallenge(header string) (string, map[string]string, bool) {
	header = strings.TrimSpace(header)
	if !strings.HasPrefix(strings.ToLower(header), "bearer ") {
		return "", nil, false
	}
	values := map[string]string{}
	rest := header[len("bearer "):]
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		after = strings.TrimSpace(after)
		value := ""
		if strings.HasPrefix(after, "\"") {
			after = after[1:]
			end := strings.IndexByte(after, '"')
			if end < 0 {
				break
			}
			value = after[:end]
			rest = after[end+1:]
		} else {
			before, remaining, found := strings.Cut(after, ",")
			if !found {
				value, rest = strings.TrimSpace(before), ""
			} else {
				value, rest = strings.TrimSpace(before), remaining
			}
		}
		values[key] = value
	}
	return values["realm"], values, values["realm"] != ""
}

func escapeRepository(repository string) string {
	parts := strings.Split(repository, "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func closeArtifactResponse(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
}

func sha256HexBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// createArtifactReleaseTx rechecks the app/repository/environment placement
// facts under the admission transaction, then records the canonical immutable
// release, pending deployment and initial state in one commit. It never reads
// the registry.
func (s *Service) createArtifactReleaseTx(ctx context.Context, tx pgx.Tx, opID contract.OperationID, env platform.Environment, artifact *resolvedArtifact) (platform.ReleaseID, error) {
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
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_releases
        (id,operation_id,app_id,environment_id,image,image_id,source,status)
        VALUES($1,$2,$3,$4,$5,$6,$7,'pending')`,
		releaseID, opID, app.ID, env.ID, artifact.CanonicalImage, artifact.ConfigDigest, source); err != nil {
		return "", platformWriteError(err, "artifact release could not be recorded")
	}
	return releaseID, nil
}

func artifactSelectorLabel(artifact *resolvedArtifact) string {
	if artifact == nil {
		return ""
	}
	return fmt.Sprintf("%s %s resolved to %s (config %s)", artifact.Kind, artifact.Requested, artifact.CanonicalImage, artifact.ConfigDigest)
}

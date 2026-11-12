package platform

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// MaxCanonicalArtifactReferenceLength is the release.image column bound. A
// canonical artifact reference is <registry-host>/<repository>@<manifest
// digest>; admissions reject anything longer before a release row is written.
const MaxCanonicalArtifactReferenceLength = 255

// MaxImageRepositoryLength bounds the private workspace repository path. The
// canonical reference check remains authoritative because it also includes the
// configured registry host and digest.
const MaxImageRepositoryLength = 200

var (
	imageRepositoryComponentPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	imageTagValuePattern            = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	imageManifestDigestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// DefaultImageRepository returns the app slug used when app creation omits an
// explicit image repository. It is the app name, which the platform already
// restricts to a lower-case repository-safe slug.
func DefaultImageRepository(appName string) string {
	return strings.TrimSpace(appName)
}

// NormalizeImageRepository validates and normalizes an app's private workspace
// registry path. The path is repository-only: no registry host, port, tag,
// digest, scheme, URL, query, fragment, or credentials. It is intentionally not
// resolved against Docker Hub.
func NormalizeImageRepository(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", invalid("image repository", "Use a private repository path such as mbvlabs/andurel.")
	}
	if len(value) > MaxImageRepositoryLength {
		return "", invalid("image repository", fmt.Sprintf("Use at most %d characters; the canonical artifact reference must fit the release image column.", MaxImageRepositoryLength))
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "@:?#\\ \t\r\n\x00") {
		return "", invalid("image repository", "Use only the private repository path: no registry host or port, tag, digest, URL, scheme, query, fragment, or credentials.")
	}
	if strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") || strings.Contains(value, "//") {
		return "", invalid("image repository", "Use slash-separated repository components without an empty component.")
	}
	parts := strings.Split(value, "/")
	if len(parts) > 4 {
		return "", invalid("image repository", "Use at most four repository components.")
	}
	if len(parts) > 1 && isRegistryHostComponent(parts[0]) {
		return "", invalid("image repository", "Do not include a registry host or port; use only the private repository path.")
	}
	for _, part := range parts {
		if !imageRepositoryComponentPattern.MatchString(part) {
			return "", invalid("image repository", "Use lower-case letters, digits, dots, underscores, and hyphens; each component starts and ends with a letter or digit.")
		}
	}
	return strings.Join(parts, "/"), nil
}

// NormalizeImageTag validates a tag selector for the app's private repository.
func NormalizeImageTag(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", invalid("image tag", "Pass a tag from the app's private repository.")
	}
	if !imageTagValuePattern.MatchString(value) {
		return "", invalid("image tag", "Use 1–128 letters, digits, dots, underscores, or hyphens; start with a letter, digit, or underscore.")
	}
	return value, nil
}

// NormalizeImageDigest validates a manifest digest selector. Only the sha256
// algorithm is supported so the reference fits the release contract.
func NormalizeImageDigest(raw string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if !imageManifestDigestPattern.MatchString(value) {
		return "", invalid("image digest", "Use an immutable sha256 digest such as sha256:"+strings.Repeat("a", 64)+".")
	}
	return value, nil
}

// IsManifestDigest reports whether value is a sha256 manifest digest.
func IsManifestDigest(value string) bool {
	return imageManifestDigestPattern.MatchString(strings.ToLower(strings.TrimSpace(value)))
}

// ImageReference is a parsed fully qualified OCI image reference.
type ImageReference struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
	Original   string
}

// CanonicalRegistryAddress normalizes a registry host/port for exact
// comparison. It accepts a loopback host:port as used by the private node
// registry and lower-cases DNS names.
func CanonicalRegistryAddress(value string) string {
	value = strings.TrimSpace(value)
	if host, port, err := net.SplitHostPort(value); err == nil {
		return strings.ToLower(host) + ":" + port
	}
	return strings.ToLower(value)
}

// ParseFullyQualifiedImage parses registry/repository:tag or
// registry/repository@sha256:… without guessing a registry. A leading component
// that is not an explicit registry host is refused: the fully qualified image
// selector is not a Docker Hub shorthand.
func ParseFullyQualifiedImage(raw string) (ImageReference, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ImageReference{}, invalid("image", "Pass a fully qualified registry/repository:tag or registry/repository@sha256:… reference.")
	}
	if strings.Contains(value, "://") || strings.ContainsAny(value, "?#\\ \t\r\n\x00") {
		return ImageReference{}, invalid("image", "Use registry/repository:tag or registry/repository@sha256:… without a scheme, credentials, query, or fragment.")
	}
	at := strings.LastIndex(value, "@")
	digest := ""
	namepart := value
	if at >= 0 {
		namepart, digest = value[:at], value[at+1:]
		if !IsManifestDigest(digest) {
			return ImageReference{}, invalid("image digest", "Use an immutable sha256 digest after @.")
		}
		digest = strings.ToLower(digest)
	}
	slash := strings.IndexByte(namepart, '/')
	if slash <= 0 || slash == len(namepart)-1 {
		return ImageReference{}, invalid("image", "Use a fully qualified reference with an explicit registry host, for example registry.example.com/team/app:tag.")
	}
	registry := namepart[:slash]
	repository := namepart[slash+1:]
	if !isRegistryHostComponent(registry) {
		return ImageReference{}, invalid("image", "Use an explicit registry host such as registry.example.com; a bare repository is not a fully qualified image.")
	}
	tag := ""
	if colon := strings.LastIndex(repository, ":"); colon >= 0 {
		tag = repository[colon+1:]
		repository = repository[:colon]
		if !imageTagValuePattern.MatchString(tag) {
			return ImageReference{}, invalid("image tag", "Use 1–128 letters, digits, dots, underscores, or hyphens; start with a letter, digit, or underscore.")
		}
	}
	if digest == "" && tag == "" {
		return ImageReference{}, invalid("image", "Name a tag or digest explicitly; there is no implicit latest.")
	}
	if repository == "" || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") || strings.Contains(repository, "//") {
		return ImageReference{}, invalid("image repository", "Use slash-separated lower-case repository components.")
	}
	for _, part := range strings.Split(repository, "/") {
		if !imageRepositoryComponentPattern.MatchString(part) {
			return ImageReference{}, invalid("image repository", "Use lower-case repository components made of letters, digits, dots, underscores, and hyphens.")
		}
	}
	return ImageReference{
		Registry: registry, Repository: repository, Tag: tag, Digest: digest, Original: value,
	}, nil
}

func isRegistryHostComponent(value string) bool {
	if value == "localhost" {
		return true
	}
	if strings.ContainsAny(value, "@") {
		return false
	}
	host := value
	if strings.Contains(value, ":") {
		parsedHost, port, err := net.SplitHostPort(value)
		if err != nil || port == "" {
			return false
		}
		parsedPort, err := strconv.Atoi(port)
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return false
		}
		host = parsedHost
	}
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	return strings.Contains(host, ".")
}

// ValidateAppImageRepositoryForRegistry validates the private repository path
// and the canonical artifact reference bound implied by the actual configured
// registry endpoint. It is called before app creation so an over-long immutable
// repository cannot be persisted and only fail later during artifact admission.
func ValidateAppImageRepositoryForRegistry(repository, registryEndpoint string) error {
	normalized, err := NormalizeImageRepository(repository)
	if err != nil {
		return err
	}
	if strings.TrimSpace(registryEndpoint) == "" {
		return invalid("image repository", "The node's configured registry endpoint is required before an app image repository can be recorded.")
	}
	_, err = CanonicalArtifactReference(registryEndpoint, normalized, "sha256:"+strings.Repeat("a", 64))
	return err
}

// CanonicalArtifactReference builds and bounds the immutable reference stored
// on a release. The manifest digest is the manifest identity, never the image
// config digest.
func CanonicalArtifactReference(registryEndpoint, repository, manifestDigest string) (string, error) {
	registryEndpoint = strings.TrimSuffix(strings.TrimSpace(registryEndpoint), "/")
	repository = strings.TrimSpace(repository)
	if registryEndpoint == "" || repository == "" {
		return "", invalid("artifact reference", "The configured private registry and app repository are required.")
	}
	if !IsManifestDigest(manifestDigest) {
		return "", invalid("artifact reference", "Use an immutable sha256 manifest digest.")
	}
	reference := registryEndpoint + "/" + repository + "@" + strings.ToLower(manifestDigest)
	if len(reference) > MaxCanonicalArtifactReferenceLength {
		return "", invalid("artifact reference", fmt.Sprintf("The canonical artifact reference exceeds %d characters; shorten the app image repository.", MaxCanonicalArtifactReferenceLength))
	}
	return reference, nil
}

package retention

import "strings"

// imageRef is a normalized image reference. repository is the canonical
// repository path, with any explicit registry host and port removed; tag is the
// tag (empty for a digest reference); digest is the manifest digest (empty for
// a tag reference). A reference may carry both a tag and a digest.
type imageRef struct {
	repository string
	tag        string
	digest     string
}

// parseImageReference normalizes the image references eregion records on a
// release. It accepts Docker's repository:tag, repository@sha256:… and
// registry/repository:tag forms, with an optional http(s):// prefix. The
// repository is canonicalized by normalizeRepository so an inventory catalog
// (which normally omits the registry host) matches the release's image.
func parseImageReference(raw string) (imageRef, bool) {
	value := trimImageReference(raw)
	if value == "" {
		return imageRef{}, false
	}
	if strings.HasPrefix(value, "sha256:") || strings.HasPrefix(value, "sha512:") {
		if strings.Contains(value, "/") {
			return imageRef{}, false
		}
		digest := normalizeDigest(value)
		if !validDigest(digest) {
			return imageRef{}, false
		}
		return imageRef{digest: digest}, true
	}

	var digest string
	if at := strings.LastIndex(value, "@"); at >= 0 {
		digest = normalizeDigest(value[at+1:])
		value = value[:at]
		if !validDigest(digest) {
			return imageRef{}, false
		}
	}

	var tag string
	if colon := strings.LastIndex(value, ":"); colon > strings.LastIndex(value, "/") {
		tag = strings.TrimSpace(value[colon+1:])
		value = value[:colon]
		if tag == "" {
			return imageRef{}, false
		}
	}

	repository := normalizeRepository(value)
	if repository == "" || strings.ContainsAny(repository, " :@") {
		return imageRef{}, false
	}
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return imageRef{repository: repository, tag: tag, digest: digest}, true
}

// normalizeRepository canonicalizes a repository or a full image reference for
// comparison with a registry catalog. Docker treats the first path component as
// a registry host when it contains a dot, contains a colon, or is localhost; that
// component and its port are removed. Repository names are lower-case by Docker
// rule.
func normalizeRepository(raw string) string {
	value := trimImageReference(raw)
	if value == "" {
		return ""
	}
	if at := strings.Index(value, "@"); at >= 0 {
		value = value[:at]
	}
	// A tag can only follow the last slash. This also leaves a registry port
	// such as registry:5000/api untouched.
	if colon := strings.LastIndex(value, ":"); colon > strings.LastIndex(value, "/") {
		value = value[:colon]
	}
	value = strings.Trim(value, "/")
	if value == "" {
		return ""
	}

	parts := strings.Split(value, "/")
	for _, part := range parts {
		if part == "" {
			return ""
		}
	}
	if len(parts) > 1 && looksLikeRegistryHost(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return ""
	}
	repository := strings.ToLower(strings.Join(parts, "/"))
	if repository == "" {
		return ""
	}
	return repository
}

// normalizeDigest trims and lower-cases a digest so the same manifest compares
// equal regardless of the case an inventory chose. A non-digest value is
// returned trimmed but otherwise unchanged.
func normalizeDigest(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// validDigest reports whether a normalized value has a digest algorithm and a
// non-empty encoded value. It rejects a bare "sha256:" from an incomplete
// inventory entry.
func validDigest(value string) bool {
	algorithm, encoded, ok := strings.Cut(value, ":")
	return ok && algorithm != "" && encoded != ""
}

// artifactReference is the repository-qualified removal reference for one
// registry artifact. The repository keeps its inventory spelling so an operator
// can see the exact registry path; the digest is normalized.
func artifactReference(artifact RegistryArtifact) string {
	repository := strings.TrimSpace(artifact.Repository)
	digest := normalizeDigest(artifact.Digest)
	if repository == "" {
		return digest
	}
	return repository + "@" + digest
}

func trimImageReference(raw string) string {
	value := strings.TrimSpace(raw)
	if index := strings.Index(value, "://"); index >= 0 {
		value = value[index+3:]
	}
	value = strings.TrimRight(value, "/")
	return strings.TrimSpace(value)
}

func looksLikeRegistryHost(component string) bool {
	lower := strings.ToLower(component)
	return lower == "localhost" || strings.Contains(component, ".") || strings.Contains(component, ":")
}

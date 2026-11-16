package platform

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/registrymeta"
)

// ResolveCanonicalImageIdentity authenticates a canonical registry digest
// reference through the manifest and config bytes and returns both immutable
// identities. Docker image IDs (runtime observations) are deliberately absent.
// Loopback node registries use the bounded HTTP client; every other registry
// uses normal verified HTTPS with anonymous auth only.
func ResolveCanonicalImageIdentity(
	ctx context.Context,
	image, arch string,
) (registrymeta.Resolution, error) {
	parsed, err := ParseFullyQualifiedImage(image)
	if err != nil {
		return registrymeta.Resolution{}, fmt.Errorf("%w: %w", engine.ErrPermanent, err)
	}
	if parsed.Digest == "" {
		return registrymeta.Resolution{}, fmt.Errorf(
			"%w: a canonical release image must be an immutable digest reference",
			engine.ErrPermanent,
		)
	}
	address := parsed.Registry
	loopback := false
	host := address
	if hostPart, _, splitErr := net.SplitHostPort(address); splitErr == nil {
		host = hostPart
	}
	if addr, parseErr := netip.ParseAddr(host); parseErr == nil && addr.IsLoopback() {
		loopback = true
	}
	if strings.EqualFold(host, "localhost") {
		loopback = true
	}
	scheme := "https"
	if loopback {
		scheme = "http"
	}
	client := &registrymeta.Client{
		HTTPClient:    registrymeta.NewHTTPClient(loopback),
		Base:          scheme + "://" + address,
		AnonymousAuth: !loopback,
	}
	return client.Resolve(
		ctx,
		parsed.Repository,
		parsed.Digest,
		registrymeta.Options{Architecture: arch, AllowIndex: true},
	)
}

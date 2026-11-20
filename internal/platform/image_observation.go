package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// ImageObservation is a point-in-time, read-only local Docker observation.
type ImageObservation struct {
	Reference    string   `json:"reference"`
	ID           string   `json:"id"`
	OS           string   `json:"os"`
	Architecture string   `json:"architecture"`
	RepoDigests  []string `json:"repo_digests"`
}

func (o ImageObservation) ValidFor(reference string) bool {
	return o.Reference == reference && IsImageDigestReference(reference) &&
		imageIDPattern.MatchString(o.ID) && o.OS != "" && o.Architecture != "" && o.RepoDigests != nil
}

func ObserveLocalImage(ctx context.Context, reference string) (ImageObservation, error) {
	if !IsImageDigestReference(reference) {
		return ImageObservation{}, contract.NewFault(contract.ValidationFailed, "image observation requires a canonical digest reference", "")
	}
	env, cleanup, err := IsolatedDockerEnvironment()
	if err != nil {
		return ImageObservation{}, err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout, stderr, err := runArgvClean(ctx, []string{"docker", "image", "inspect", "--format", "{{.Id}}|{{.Os}}|{{.Architecture}}|{{json .RepoDigests}}", reference}, env, engine.Runtime{}, "image-observation", nil)
	if err != nil {
		return ImageObservation{}, contract.NewFault(contract.Unavailable, "image observation failed", firstLine(stderr))
	}
	parts := strings.SplitN(strings.TrimSpace(stdout), "|", 4)
	if len(parts) != 4 || !imageIDPattern.MatchString(parts[0]) || parts[1] == "" || parts[2] == "" {
		return ImageObservation{}, contract.NewFault(contract.Unavailable, "invalid image observation", "")
	}
	var digests []string
	if err := json.Unmarshal([]byte(parts[3]), &digests); err != nil {
		return ImageObservation{}, contract.NewFault(contract.Unavailable, "invalid image observation", fmt.Sprint(err))
	}
	observation := ImageObservation{Reference: reference, ID: parts[0], OS: parts[1], Architecture: parts[2], RepoDigests: digests}
	if !observation.ValidFor(reference) {
		return ImageObservation{}, contract.NewFault(contract.Unavailable, "invalid image observation", "")
	}
	return observation, nil
}

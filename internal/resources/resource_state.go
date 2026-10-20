package resources

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const resourceStateSchema = "eregion/resource-state/v1"

type resourceState struct {
	Schema string `json:"schema"`
	Spec   Spec   `json:"spec"`
}

func (s *Service) resourceSpecPath(name string) string {
	return filepath.Join(s.resourceDir(name), "resource.json")
}

func (s *Service) readStoredSpec(name string) (Spec, bool, error) {
	raw, err := readProtectedFile(s.resourceSpecPath(name), 32*1024)
	if err != nil {
		if os.IsNotExist(err) {
			return Spec{}, false, nil
		}
		return Spec{}, false, precondition("resource inventory record cannot be read safely", "Inspect the service state directory.")
	}
	var state resourceState
	if err := json.Unmarshal(raw, &state); err != nil || state.Schema != resourceStateSchema || state.Spec.Name != name {
		return Spec{}, false, precondition("resource inventory record is corrupt", "Inspect the service state directory; do not guess the resource identity.")
	}
	return state.Spec, true, nil
}

// ensureStoredSpec records the secret-free resource identity for inventory. It
// refuses to rewrite the durable identity of an existing resource.
func (s *Service) ensureStoredSpec(spec Spec) error {
	existing, ok, err := s.readStoredSpec(spec.Name)
	if err != nil {
		return err
	}
	if ok {
		if existing.Host != spec.Host || existing.Version != spec.Version || existing.Database != spec.Database ||
			existing.Username != spec.Username || existing.Volume != spec.Volume || existing.Engine != spec.Engine {
			return conflict("existing resource identity does not match the request", "Use the original version, volume, database, and username; create a new resource for a different identity.")
		}
	}
	encoded, err := json.MarshalIndent(resourceState{Schema: resourceStateSchema, Spec: spec}, "", "  ")
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(s.resourceDir(spec.Name)); err != nil {
		return unavailable("could not create the protected state directory", "Check permissions on the service state directory.")
	}
	if err := writeAtomic(s.resourceSpecPath(spec.Name), append(encoded, '\n'), 0o600); err != nil {
		return unavailable("could not write the resource inventory record", "Check permissions on the service state directory.")
	}
	return nil
}

// ListResources returns secret-free stored resource specifications on this
// service host. It does not contact Docker.
func (s *Service) ListResources() ([]Spec, error) {
	root := filepath.Join(s.config.StateDir, "resources")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	specs := make([]Spec, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		spec, ok, err := s.readStoredSpec(entry.Name())
		if err != nil {
			return nil, err
		}
		if ok {
			specs = append(specs, spec)
		}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs, nil
}

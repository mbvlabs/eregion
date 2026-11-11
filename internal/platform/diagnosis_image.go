package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
)

var diagnosisImageIDPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var diagnosisBuildpackVersion = regexp.MustCompile(`^[0-9]{1,5}(\.[0-9]{1,5}){0,3}$`)
var diagnosisBuildpackIDs = map[string]bool{
	"paketo-buildpacks/go-dist": true, "paketo-buildpacks/go-build": true,
	"paketo-buildpacks/go-mod-vendor": true, "paketo-buildpacks/node-engine": true,
	"paketo-buildpacks/npm-install": true, "paketo-buildpacks/npm-start": true,
	"paketo-buildpacks/procfile": true, "paketo-buildpacks/ca-certificates": true,
	"paketo-buildpacks/environment-variables": true,
}

// ObserveDiagnosisImage reads only two whitelisted Buildpacks labels from an
// immutable local image ID. Neither the full inspect object nor label values
// are returned. Declared metadata is not proof of executable availability.
func ObserveDiagnosisImage(parent context.Context, id string) (*DiagnosisImage, string) {
	if !diagnosisImageIDPattern.MatchString(id) {
		return nil, "invalid immutable image ID"
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", `{{json .Id}}{{"\n"}}{{json (index .Config.Labels "io.buildpacks.build.metadata")}}{{"\n"}}{{json (index .Config.Labels "io.buildpacks.lifecycle.metadata")}}`, id)
	buf := &limitedDiagnosticBuffer{limit: 600 * 1024}
	cmd.Stdout = buf
	if err := cmd.Run(); err != nil || buf.overflow {
		return nil, "image absent, pruned, or Docker inaccessible"
	}
	parts := strings.SplitN(buf.String(), "\n", 3)
	var got string
	if len(parts) != 3 || json.Unmarshal([]byte(parts[0]), &got) != nil || got != id {
		return nil, "image identity could not be verified"
	}
	out := &DiagnosisImage{ImageID: id, Buildpacks: []DiagnosisBuildpack{}, ProcessTypes: []string{}, Layers: []DiagnosisLayer{}, Note: "Buildpacks metadata is declared, not proof a runtime or executable is available; only allowlisted IDs, process types and layer names are shown. Commands and layer contents are omitted."}
	seenBP, seenProcess := map[string]bool{}, map[string]bool{}
	for _, part := range parts[1:] {
		var label *string
		if json.Unmarshal([]byte(strings.TrimSpace(part)), &label) != nil {
			return out, "Buildpacks metadata label malformed"
		}
		if label == nil || *label == "" {
			continue
		}
		out.MetadataLabelsPresent++
		if len(*label) > 256*1024 || !diagnosisJSONBounded([]byte(*label)) {
			return out, "Buildpacks metadata exceeds bounds or is malformed"
		}
		var metadata struct {
			Buildpacks []struct {
				ID      string                     `json:"id"`
				Version string                     `json:"version"`
				Key     string                     `json:"key"`
				Layers  map[string]json.RawMessage `json:"layers"`
			} `json:"buildpacks"`
			Processes []struct {
				Type string `json:"type"`
			} `json:"processes"`
		}
		if json.Unmarshal([]byte(*label), &metadata) != nil || len(metadata.Buildpacks) > 128 || len(metadata.Processes) > 128 {
			return out, "Buildpacks metadata shape exceeds bounds"
		}
		for _, bp := range metadata.Buildpacks {
			out.DeclaredLayerCount += len(bp.Layers)
			for known := range diagnosisBuildpackIDs {
				if bp.ID != known && bp.Key != strings.ReplaceAll(known, "/", "_") {
					continue
				}
				for _, layer := range []string{"node", "node-engine", "go", "go-dist", "npm"} {
					if _, ok := bp.Layers[layer]; ok {
						out.Layers = append(out.Layers, DiagnosisLayer{Buildpack: known, Name: layer})
					}
				}
			}
			if !diagnosisBuildpackIDs[bp.ID] {
				continue
			}
			version := bp.Version
			if version != "" && !diagnosisBuildpackVersion.MatchString(version) {
				version = ""
			}
			key := bp.ID + "@" + version
			if !seenBP[key] {
				seenBP[key] = true
				out.Buildpacks = append(out.Buildpacks, DiagnosisBuildpack{ID: bp.ID, Version: version})
			}
		}
		for _, process := range metadata.Processes {
			switch process.Type {
			case "web", "worker", "release", "task":
				if !seenProcess[process.Type] {
					seenProcess[process.Type] = true
					out.ProcessTypes = append(out.ProcessTypes, process.Type)
				}
			}
		}
	}
	if out.MetadataLabelsPresent == 0 {
		return out, "Buildpacks metadata labels are absent"
	}
	sort.Slice(out.Layers, func(i, j int) bool {
		if out.Layers[i].Buildpack != out.Layers[j].Buildpack {
			return out.Layers[i].Buildpack < out.Layers[j].Buildpack
		}
		return out.Layers[i].Name < out.Layers[j].Name
	})
	return out, ""
}

// Limit both nesting and number of JSON values before decoding any projection.
func diagnosisJSONBounded(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	depth, count := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return depth == 0 && count > 0
		}
		if err != nil {
			return false
		}
		count++
		if count > 8192 {
			return false
		}
		if d, ok := token.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
				if depth > 16 {
					return false
				}
			case '}', ']':
				depth--
				if depth < 0 {
					return false
				}
			}
		}
	}
}

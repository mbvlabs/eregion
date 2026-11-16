package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"regexp"
	"runtime"
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
	parts, failure := observeDiagnosisParts(parent, id)
	if failure != "" {
		return nil, failure
	}
	out := &DiagnosisImage{
		ImageID:      id,
		Buildpacks:   []DiagnosisBuildpack{},
		ProcessTypes: []string{},
		Layers:       []DiagnosisLayer{},
		Note:         "Buildpacks metadata is declared, not proof a runtime or executable is available; only allowlisted IDs, process types and layer names are shown. Commands and layer contents are omitted.",
	}
	seenBP, seenProcess := map[string]bool{}, map[string]bool{}
	for _, part := range parts[1:] {
		if failure := collectDiagnosisMetadata(out, part, seenBP, seenProcess); failure != "" {
			return out, failure
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

func observeDiagnosisParts(parent context.Context, id string) ([]string, string) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"docker",
		"image",
		"inspect",
		"--format",
		`{{json .Id}}{{"\n"}}{{json (index .Config.Labels "io.buildpacks.build.metadata")}}{{"\n"}}{{json (index .Config.Labels "io.buildpacks.lifecycle.metadata")}}`,
		id,
	)
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
	return parts, ""
}

type diagnosisMetadataBuildpack struct {
	ID      string                     `json:"id"`
	Version string                     `json:"version"`
	Key     string                     `json:"key"`
	Layers  map[string]json.RawMessage `json:"layers"`
}

type diagnosisMetadataProcess struct {
	Type string `json:"type"`
}

type diagnosisMetadata struct {
	Buildpacks []diagnosisMetadataBuildpack `json:"buildpacks"`
	Processes  []diagnosisMetadataProcess   `json:"processes"`
}

func collectDiagnosisMetadata(out *DiagnosisImage, part string, seenBP, seenProcess map[string]bool) string {
	var label *string
	if json.Unmarshal([]byte(strings.TrimSpace(part)), &label) != nil {
		return "Buildpacks metadata label malformed"
	}
	if label == nil || *label == "" {
		return ""
	}
	out.MetadataLabelsPresent++
	if len(*label) > 256*1024 || !diagnosisJSONBounded([]byte(*label)) {
		return "Buildpacks metadata exceeds bounds or is malformed"
	}
	var metadata diagnosisMetadata
	if json.Unmarshal([]byte(*label), &metadata) != nil || len(metadata.Buildpacks) > 128 ||
		len(metadata.Processes) > 128 {
		return "Buildpacks metadata shape exceeds bounds"
	}
	for _, buildpack := range metadata.Buildpacks {
		collectDiagnosisBuildpack(out, buildpack, seenBP)
	}
	for _, process := range metadata.Processes {
		collectDiagnosisProcess(out, process, seenProcess)
	}
	return ""
}

func collectDiagnosisBuildpack(out *DiagnosisImage, buildpack diagnosisMetadataBuildpack, seen map[string]bool) {
	out.DeclaredLayerCount += len(buildpack.Layers)
	for known := range diagnosisBuildpackIDs {
		if buildpack.ID != known && buildpack.Key != strings.ReplaceAll(known, "/", "_") {
			continue
		}
		for _, layer := range []string{"node", "node-engine", "go", "go-dist", "npm"} {
			if _, ok := buildpack.Layers[layer]; ok {
				out.Layers = append(out.Layers, DiagnosisLayer{Buildpack: known, Name: layer})
			}
		}
	}
	if !diagnosisBuildpackIDs[buildpack.ID] {
		return
	}
	version := buildpack.Version
	if version != "" && !diagnosisBuildpackVersion.MatchString(version) {
		version = ""
	}
	key := buildpack.ID + "@" + version
	if seen[key] {
		return
	}
	seen[key] = true
	out.Buildpacks = append(out.Buildpacks, DiagnosisBuildpack{ID: buildpack.ID, Version: version})
}

func collectDiagnosisProcess(out *DiagnosisImage, process diagnosisMetadataProcess, seen map[string]bool) {
	switch process.Type {
	case "web", "worker", "release", "task":
		if !seen[process.Type] {
			seen[process.Type] = true
			out.ProcessTypes = append(out.ProcessTypes, process.Type)
		}
	}
}

// Limit both nesting and number of JSON values before decoding any projection.
func diagnosisJSONBounded(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	depth, count := 0, 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return err == io.EOF && depth == 0 && count > 0
		}
		count++
		if count > 8192 {
			return false
		}
		if delimiter, ok := token.(json.Delim); ok {
			var valid bool
			depth, valid = boundedJSONDepth(delimiter, depth)
			if !valid {
				return false
			}
		}
	}
}

func boundedJSONDepth(delimiter json.Delim, depth int) (int, bool) {
	switch delimiter {
	case '{', '[':
		depth++
		return depth, depth <= 16
	case '}', ']':
		depth--
		return depth, depth >= 0
	default:
		return depth, true
	}
}

// ResolveDiagnosisImageID maps a canonical immutable manifest reference to the
// Docker runtime image ID using a bounded, read-only local inspect. It verifies
// the platform and repository digest before returning the local observation.
func ResolveDiagnosisImageID(parent context.Context, canonical string) (string, string) {
	parsed, err := ParseFullyQualifiedImage(canonical)
	if err != nil || parsed.Digest == "" {
		return "", "canonical release image reference is unavailable"
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"docker",
		"image",
		"inspect",
		"--format",
		"{{.Id}}|{{.Os}}|{{.Architecture}}|{{json .RepoDigests}}",
		canonical,
	)
	buf := &limitedDiagnosticBuffer{limit: 600 * 1024}
	cmd.Stdout = buf
	if err := cmd.Run(); err != nil || buf.overflow {
		return "", "image absent, pruned, or Docker inaccessible"
	}
	runtimeID, err := VerifyMaterializedImageInspection(buf.String(), canonical, runtime.GOARCH)
	if err != nil {
		return "", "image identity could not be verified"
	}
	return runtimeID, ""
}

package cli

import (
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/spf13/cobra"
)

// metadataCheckReport is the offline result of `commands --check-meta`. The
// registry is already validated before any command runs; this report makes the
// existing gate explicit and inspectable without changing its semantics.
type metadataCheckReport struct {
	OK       bool     `json:"ok"`
	Checked  int      `json:"checked"`
	Failures []string `json:"failures,omitempty"`
}

func (r registry) check(root *cobra.Command) metadataCheckReport {
	report := metadataCheckReport{OK: true, Checked: len(walk(root))}
	if err := r.validate(root); err != nil {
		report.OK = false
		fault := contract.Classify(err)
		if failures, ok := fault.Details["violations"].([]string); ok {
			report.Failures = append([]string(nil), failures...)
		}
		if len(report.Failures) == 0 {
			report.Failures = []string{fault.Message}
		}
	}
	return report
}

func metadataCheckFault(report metadataCheckReport) *contract.Fault {
	return &contract.Fault{
		Code:    contract.Internal,
		Message: "command metadata check failed",
		Hint:    "Fix the command registry before publishing this binary.",
		Details: map[string]any{"failures": report.Failures},
	}
}

// searchCatalog searches the visible catalog only. Every whitespace-separated
// term must appear in the path, summary, or examples; iteration follows the
// catalog's path order, so results are deterministic.
func searchCatalog(commands []Command, query string) []Command {
	terms := strings.Fields(strings.ToLower(query))
	matches := make([]Command, 0)
	if len(terms) == 0 {
		return matches
	}
	for _, command := range commands {
		haystack := strings.ToLower(strings.Join([]string{
			command.Path,
			command.Summary,
			strings.Join(command.Examples, "\n"),
		}, "\n"))
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, command)
		}
	}
	return matches
}

// suggestCommandPaths returns actionable full paths only for the visible
// operator tree. Hidden node-mode commands are never suggested, and sorting
// plus a small limit keeps the hint deterministic.
func suggestCommandPaths(parent *cobra.Command, token string) []string {
	token = strings.ToLower(strings.TrimSpace(token))
	if parent == nil || commandHidden(parent) || token == "" || strings.HasPrefix(token, "-") {
		return nil
	}
	paths := make([]string, 0)
	for _, child := range parent.Commands() {
		if commandHidden(child) || !child.IsAvailableCommand() {
			continue
		}
		name := strings.ToLower(child.Name())
		if strings.HasPrefix(name, token) || editDistance(token, name) <= 2 {
			paths = append(paths, child.CommandPath())
		}
	}
	sort.Strings(paths)
	paths = compact(paths)
	if len(paths) > 3 {
		paths = paths[:3]
	}
	return paths
}

// editDistance is the case-insensitive Levenshtein distance used by the
// deterministic suggestion hint. Command names are short, so the simple
// dynamic-programming implementation is enough.
func editDistance(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 0
			if a[i-1] != b[j-1] {
				cost = 1
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}

func compact(paths []string) []string {
	if len(paths) < 2 {
		return paths
	}
	out := paths[:1]
	for _, path := range paths[1:] {
		if path != out[len(out)-1] {
			out = append(out, path)
		}
	}
	return out
}

func unknownCommandToken(err error) string {
	if err == nil {
		return ""
	}
	const marker = `unknown command "`
	text := err.Error()
	start := strings.Index(text, marker)
	if start < 0 {
		return ""
	}
	rest := text[start+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func unknownCommandFault(message string, suggestions []string) *contract.Fault {
	fault := &contract.Fault{
		Code:    contract.Usage,
		Message: message,
		Hint:    "Discover exact paths with eregion commands --json.",
	}
	if len(suggestions) > 0 {
		fault.Hint = "Did you mean " + strings.Join(suggestions, ", ") + "? Discover exact paths with eregion commands --json."
		fault.Details = map[string]any{"suggestions": suggestions}
	}
	return fault
}

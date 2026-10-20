package host

import (
	"fmt"
	"strings"
)

// simpleDiff renders a bounded, line-oriented preview. It is intentionally
// not a full unified diff: the purpose is to show the operator which desired
// lines differ, not to be a patch format.
func simpleDiff(path string, current, desired []byte) []string {
	const limit = 80
	out := []string{fmt.Sprintf("--- %s (current)", path), fmt.Sprintf("+++ %s (desired)", path)}
	if len(current) == 0 {
		out = append(out, "@@ new file @@")
	} else {
		out = append(out, "@@ changed @@")
	}
	lines := strings.Split(strings.TrimRight(string(desired), "\n"), "\n")
	for index, line := range lines {
		if index >= limit {
			out = append(out, fmt.Sprintf("+ … %d more desired lines", len(lines)-limit))
			break
		}
		out = append(out, "+"+line)
	}
	if len(current) > 0 {
		existing := strings.Split(strings.TrimRight(string(current), "\n"), "\n")
		if len(existing) > 12 {
			existing = existing[:12]
		}
		for _, line := range existing {
			out = append(out, "-"+line)
		}
	}
	return out
}

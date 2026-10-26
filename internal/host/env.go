package host

import (
	"fmt"
	"os"
	"strings"
)

// ServiceEnvPath is the environment file the node services load. Node-mode
// commands read the same file so they use the control plane's database,
// keyring and extension configuration rather than the caller's shell.
const ServiceEnvPath = "/etc/eregion/control-plane.env"

// LoadServiceEnv reads path and applies every KEY="value" line to the current
// process. It understands the quoted, escaped form bootstrap writes. A missing
// file is an error: a node RPC must never fall back to an ambient database URL.
func LoadServiceEnv(path string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the fixed control-plane environment path.
	if err != nil {
		return fmt.Errorf("read the node service environment: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" || strings.ContainsAny(key, " \t\r\n") {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = unquoteServiceEnv(value[1 : len(value)-1])
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("apply the node service environment: %w", err)
		}
	}
	return nil
}

func unquoteServiceEnv(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			switch value[i+1] {
			case '\\', '"':
				i++
				out.WriteByte(value[i])
				continue
			}
		}
		out.WriteByte(value[i])
	}
	return out.String()
}

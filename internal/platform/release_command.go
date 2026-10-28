package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ReleaseCommandDigest is the durable identity of one release command
// specification: its argv and timeout. A changed command has a new digest and
// therefore runs again for the same release; whitespace or object key order
// cannot create a second identity.
func ReleaseCommandDigest(command CommandSpec) string {
	raw, _ := json.Marshal(struct {
		Argv           []string `json:"argv"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}{command.Argv, command.TimeoutSeconds})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

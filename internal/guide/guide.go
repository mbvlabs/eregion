// Package guide embeds the agent operating guide printed by eregion guide
// agents and installed by eregion skill install.
package guide

import _ "embed"

// Agents is the embedded agent operating guide.
//
//go:embed agents.md
var Agents string

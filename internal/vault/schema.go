package vault

import _ "embed"

// SchemaSQL is the complete domain DDL for revisioned encrypted secrets. The
// control plane applies it as one explicit migration; this package never runs
// DDL on its own.
//
//go:embed schema.sql
var SchemaSQL string

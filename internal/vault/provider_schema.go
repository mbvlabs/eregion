package vault

import _ "embed"

// ProviderSchemaSQL is the complete domain DDL for typed provider credentials.
// It is separate from the application-secret schema so provider scopes can
// never masquerade as an app/environment secret namespace. The control plane
// applies it as migration 027; this package never runs DDL on its own.
//
//go:embed provider_schema.sql
var ProviderSchemaSQL string

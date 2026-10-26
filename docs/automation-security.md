# Scoped automation credentials

API credentials contain a token hash, a server-resolved actor, transport scopes,
and optional explicit action grants. Never store the plaintext token in this
file, a request body, a River argument, an event, or Git.

```json
[
  {
    "actor": {"kind": "ci", "subject": "example/api-deployment"},
    "token_sha256": "REPLACE_WITH_LOWERCASE_SHA256_OF_RANDOM_TOKEN",
    "scopes": ["operations:submit", "operations:read"],
    "action_grants": [
      {"kind": "platform.release.create", "target": "env/api/production"},
      {"kind": "platform.deploy", "target": "env/api/production"}
    ]
  }
]
```

The example placeholder is deliberately not a valid hash. Generate a random
32–256-character token using a secret manager, calculate its SHA-256, protect
the server JSON file with mode `0600`, and supply the token to CI through
`EREGION_API_TOKEN`. Restart the API to load credential changes.

## Authorization rules

- Transport scopes do not grant platform-wide action authority to CI or webhook
  credentials. An automation principal without action grants can use the original
  diagnostic operation only.
- Each grant matches one exact action kind and target. `env/api/*` can grant all
  environments of one app; wildcards in app names or action kinds are rejected.
- Preview/dry-run requests require the same action authorization as submission.
- A scoped credential's inventory reads must specify both the authorized `app`
  and `environment`. Environment, release, deployment, and change collections
  support these reads; global fleet and extension collections fail closed.
- Operation reads, event replay, and cancellation remain scoped to the operation's
  authenticated actor and the credential's target grant. Cancellation also needs
  `operations:cancel`.
- Operator and system credentials with no action grants retain administrative
  authority for compatibility. Add grants when issuing a restricted operator
  credential. Never distribute a development operator token as a deployment token.
- An action grant does not confer shell, host-enrollment, secret-export, or
  destructive-resource authority for any other action kind.

Use a distinct actor subject per automation integration. Keep each submission's
idempotency key stable across retries, and retain its operation ID. A lost HTTP
response is not evidence that submission or a remote effect failed.

## Testing boundary

There are no local test harnesses. Unit and integration tests run with
`just check` and `just integration`; everything else is exercised by hand on a
real test host. Passing tests are not evidence of fresh-host, remote-fleet,
telemetry, or HA release readiness. See the roadmap for the acceptance gates.

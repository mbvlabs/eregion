# Security review — M7 release readiness

**Date:** 2026-10-20  
**Scope:** authentication and authorization, secret handling, terminal escape
safety, webhook authenticity/replay, private-access proxying, SSH host-key
pinning, file permissions, command construction in host/resource/telemetry step
rendering, and SSRF in health checks and release/git sources.  
**Method:** every claim below was checked against the code in this tree. Severity
uses the impact of a realistic caller who already holds the credential named in
the finding. This is a review, not a penetration test or an external audit.

## Summary

| ID | Severity | Area | Status |
| --- | --- | --- | --- |
| S1 | Medium | Git build sources are not restricted to public hosts and may follow redirects | **Accepted for v0.1**; not fixed in this tree (owner: platform/security). See scoped acceptance below. |
| S2 | Low | Private-access proxy forwards client credential headers other than `Authorization` | **Accepted for v0.1**; not fixed in this tree (owner: API/private-access). |
| S3 | Low | SSH identity file check and read are a TOCTOU pair and do not reject symlinks | **Accepted for v0.1**; not fixed in this tree (owner: transport). |
| S4 | Low | Webhook replay is bounded only by operation idempotency retention | **Fixed in this tree.** Durable `eregion_webhook_deliveries`, migration 008, unit and PostgreSQL integration tests. |
| S5 | Informational | Bearer-token resolution uses a normal map lookup | Accepted; no practical attack demonstrated. |
| S6 | Informational | Secret request body is copied into a Go string before decoding | Accepted; memory-lifetime hygiene only. |

No plaintext-secret leak, authentication bypass, host-key bypass, or command
injection was found in the reviewed paths. The terminal-safety and ASCII
rendering fixes made in this task are in `internal/tui` and have regression
tests (`internal/tui/tui_test.go`).

## Verified controls

### Route boundaries (`internal/api/mux.go`)

`NewMux` classifies by exact path prefix, not by a client-supplied route table
(`internal/api/mux.go:36-48`). `/v1/secrets` goes to the vault handler, which
authenticates itself and refuses insecure plaintext transports
(`internal/api/vault.go:92-123`, `398-408`). `/v1/webhooks/github` is the only
bearer-bypass route, and it verifies its own HMAC. `/private/` is only mounted
when the access controller is configured, and the request must pass bearer
authentication plus a subject/device grant before the proxy is reached
(`internal/api/mux.go:56-70`). The fallback never inherits an unauthenticated
route.

### Authentication (bearer tokens)

- `internal/api/auth.go:22-37` opens the credential file, checks the opened
  file's mode (`&0077 == 0`) and regular-file type, and rejects unknown JSON
  fields. Credentials are stored as lowercase SHA-256 hashes only
  (`internal/api/auth.go:60-70`); the plaintext token never appears in the file.
- `internal/api/auth.go:100-112` requires `Bearer` plus a 32–256 character
  token and resolves it by SHA-256. There is no actor field in request bodies.
- Scopes are an explicit allowlist (`internal/api/auth.go:14`) and every route
  selects one required scope in `internal/api/server.go:101-112`.
- `/healthz` is the only unauthenticated API route. `/readyz` requires
  `system:read` and returns a fixed message, never a database error or DSN.

### Authorization

- `internal/api/authorization.go:64-78` is the only action decision point. An
  operator/system credential with no grants keeps administrative authority;
  CI/webhook credentials get exactly one diagnostic compatibility action and
  otherwise need an exact kind/target grant. `env/<app>/*` is the only
  wildcard and is validated as one exact app segment.
- Previews (`dry_run`) call the same authorizer as submissions; there is no
  preview-only bypass (`internal/api/authorization.go:80-87` and
  `internal/api/server.go:139-166`).
- Inventory reads fail closed for scoped credentials unless both `app` and
  `environment` are authorized (`internal/api/authorization.go:88-111`).
- Vault mutations/read/export/archive each require a distinct
  `platform.secret.*` grant after the namespace is validated
  (`internal/api/vault.go:243-299`, `306-337`). Plaintext export is a separate
  route; values are never part of operation input.
- Webhook submissions use actor `webhook:github:<repo>`; the repository is
  resolved from the signed payload and the allowlist, never from an app field
  in the body (`internal/api/webhook.go:188-245`).

### Secret handling

- Vault ciphertext is AES-256-GCM with namespace/revision AAD, a per-secret
  revision CAS, and a master key file that must be owner-only and non-symlink
  (`internal/vault`, `docs/secrets.md`). Metadata responses contain no
  plaintext.
- River arguments contain only durable identity:
  `internal/controlplane/jobs.go:34-45` (`operation_id`, `action_kind`). Action
  input stays in PostgreSQL; secret values are not action input.
- Workers resolve secret references immediately before transport and attach them
  to `node.Request.Secrets` (`internal/controlplane/platform_worker.go:489-512`,
  `766-832`). The node registers them with a redaction registry before any
  event/log/journal write (`internal/noded/task.go:34-44`) and the transport
  registers them before writing events or errors
  (`internal/transport/ssh.go:178-183`).
- The node writes the task request over an inherited pipe (`internal/noded/serve.go:260-294`),
  not to the node journal or a file; task stdout/stderr are redacted before they
  reach the node log (`internal/noded/task.go:34-60`).
- CLI output redacts every envelope field (`internal/output/output.go:130-165`)
  and secret export creates a new `0600` file with `O_EXCL`
  (`internal/cli/secret.go:277`).

### Terminal escape safety

- `internal/output/output.go:106-116` makes C0/C1 controls and Unicode format
  characters (`Cf`, including bidi overrides and OSC terminators) visible as
  `\uXXXX`. Newline and tab remain intact for append-only output.
- The viewer cleanser is applied to event messages, stages, execution IDs, and
  progress units before they enter the reducer
  (`internal/viewer/model.go:260-270`); the TUI also cleans host/target/event
  text at render time (`internal/tui/view.go`).
- This task changed `internal/tui` separators and stage labels to use the
  ASCII separator under `--ascii`/`TERM=dumb`; regression goldens cover
  `TERM=dumb`, `xterm-256color`, `screen-256color`, `NO_COLOR`, ASCII, reduced
  motion, CJK/emoji labels, and 20×6/300×80 layouts
  (`internal/tui/tui_test.go`, `internal/tui/testdata`).

### Webhook authenticity and deduplication

- The body is bounded before/while reading (`internal/api/webhook.go:143-157`),
  exactly one `X-Hub-Signature-256` is required, and the HMAC is compared with
  `hmac.Equal` over the exact received bytes
  (`internal/api/webhook.go:158-162`, `360-371`).
- Exactly one `X-GitHub-Event` and one delivery ID are required; only `push`
  is handled, and ping is answered without submitting work
  (`internal/api/webhook.go:164-182`).
- The repository/branch allowlist is checked before any submission; a deleted
  branch or non-branch ref is ignored (`internal/api/webhook.go:188-206`).
- The idempotency key is `ghdelivery-<X-GitHub-Delivery>`
  (`internal/api/webhook.go`), so a retried or replayed delivery resolves to the
  same durable operation instead of starting a second one.
- A delivery is also recorded in `eregion_webhook_deliveries` before submission
  and completed with the accepted operation ID. The row stores SHA-256 of the
  delivery ID, SHA-256 of the exact body, actor/kind/target, and the operation
  ID; it never stores the secret, raw signature, raw delivery ID, or body
  (`internal/api/webhook_delivery.go`,
  `internal/controlplane/webhook_deliveries.go`, migration 008). A replay after
  operation/idempotency retention returns the recorded operation ID and creates
  nothing. The same delivery ID with different signed bytes is refused as
  `conflict`.
- The handler never reads `Authorization` and never enqueues directly; it
  submits through `operations.Service.Submit` (same durable operation + River
  job path as the API).

### SSH host-key pinning

- `internal/transport/ssh.go:131-136` refuses an unpinned address and refuses
  a nil pin set. There is no trust-on-first-use path.
- `internal/transport/hostkeys.go:249-269` accepts only a pinned key, refuses a
  key revoked by the `known_hosts` file even when another line pins it, and
  reports unknown hosts separately from mismatches.
- `known_hosts` hashed names and `@revoked` markers are honored; `@cert-authority`
  is deliberately skipped (`internal/transport/hostkeys.go:85-141`).
- The key algorithms offered are constrained to the pinned key types
  (`internal/transport/hostkeys.go:271-298`).
- The node command is a fixed path-command string with no shell
  metacharacters (`internal/transport/ssh.go:20-23`, `50-53`, `137-144`), and
  the request travels over stdin, not the command line.

### File permissions

- API credential file: owner-only, regular, bounded, checked on the opened file
  (`internal/api/auth.go:22-37`).
- Webhook secret: owner-only, regular, bounded, checked on the opened file
  (`internal/cli/extensions.go:196-211`).
- Vault key ring: absolute, regular, non-symlink, `0600`/`0400`, bounded
  (`internal/vault/keyring.go`; documented in `docs/secrets.md`).
- Upgrade state: directories forced to `0700`, journals/backup metadata `0600`,
  backups `0700`, staged/installed binaries `0755`; the target must be a
  regular non-symlink file not writable by group/other
  (`internal/upgrade/fsutil.go`, `internal/upgrade/install.go:60-90`,
  `internal/upgrade/archive.go:113-135`).
- Secret export: create-only `0600` with `O_EXCL`, fsync, remove-on-failure
  (`internal/cli/secret.go:270-300`).

### Command construction in host/resource/telemetry steps

- `internal/platform/step.go:753-810` validates the complete argv vector and
  rejects shell programs and unsupported subcommands; execution uses
  `exec.CommandContext` with an argument list, never a shell
  (`internal/platform/step.go:791`).
- `internal/resources/runner.go:47-80` executes Docker commands directly with a
  fixed program and argument list, never a shell.
- `internal/telemetry/remote/step.go:395-440` accepts only a validated
  `docker compose` argv and executes it directly.
- Host bootstrap uses `/bin/bash -c` for fixed scripts
  (`internal/host/exec.go:38`), but every interpolated value is either a Go
  constant (package/service names), a validated plain account name, a literal
  IP listen address, or `shellQuote`d (`internal/host/step_services.go`,
  `step_packages.go`, `step_users.go`, `step_registry.go`). No action input is
  passed through this shell. The residual risk is future code that interpolates
  new operator input without validation; the current review found none.

### SSRF

- Upgrade downloads refuse redirects, non-HTTPS URLs, credentials/fragments,
  loopback/private/special literal addresses, and re-check every DNS answer
  before dialing (`internal/upgrade/download.go:57-115`, `297-376`). An
  operator allowlist can pin hosts.
- Platform HTTP/TCP health checks are loopback-only, ignore environment
  proxies, and do not follow redirects (`internal/platform/step.go:429-499`).
- Telemetry external export permits plain HTTP only for private/loopback hosts
  (`internal/telemetry/config.go:200-244`).
- Directory build sources resolve symlinks and must remain under configured
  build roots (`internal/platform/source.go:34-66`).

## Findings

### S1 — Medium — Git build sources can reach private hosts and follow redirects

**Where:** `internal/platform/actions.go:334-347` (submission validation) and
`internal/platform/step.go:576-588`, `631-658` (worker execution).  
**Why:** the only URL policy is `https`, no credentials, no query/fragment.
There is no private/loopback/link-local/CGNAT/metadata address check, and
`git clone` is allowed to follow HTTP redirects. A CI credential with an
`platform.release.create` grant for one environment can ask the builder node to
clone `https://10.0.0.5/internal.git` or a public URL that redirects to an
internal endpoint, turning the builder into an authenticated-internal-network
probe. Directory builds are already restricted; git builds are not.  
**Fix (concrete):**
1. Resolve `parsed.Hostname()` at submission and again in `buildPackStep`, and
   reject any answer that is loopback, private, link-local, CGNAT, unspecified,
   multicast, or the cloud metadata address (`169.254.169.254`). Reuse the
   `publicIP` policy already in `internal/upgrade/download.go`.
2. Allowlist repository hosts with an operator setting such as
   `EREGION_BUILD_GIT_ALLOWED_HOSTS`; default to the public GitHub host only.
3. Run clone with redirects disabled and no ambient credentials:
   `git -c http.followRedirects=false -c protocol.file.allow=never clone ...`,
   `GIT_TERMINAL_PROMPT=0`, and the existing clean build environment.
4. Add tests for literal private IPs, a hostname resolving to a private IP,
   and a redirect response.

**v0.1 decision — accepted, not fixed (owner: platform/security).** The finding
needs a builder node with outbound network access and a caller holding a
release-create grant. The v0.1 line is documented as a single-control-plane
experimental release and does not advertise multi-tenant builder isolation. The
scoped acceptance is: do not expose the builder role to untrusted CI credentials
or place it on a network with reachable internal services; the concrete fix
above remains the release-blocking requirement before a shared or multi-tenant
builder claim. This worktree did not edit `internal/platform`.

### S2 — Low — Private-access proxy forwards client credential headers

**Where:** `internal/cli/extensions.go:120-128`.  
**Why:** the `ReverseProxy` rewrite deletes `Authorization` but leaves
`Cookie`, `Proxy-Authorization`, and client-supplied forwarding headers intact.
The private route already requires a bearer token plus a matching device grant,
so this is not an access bypass, but a browser cookie or proxy credential for
the control-plane origin is unnecessarily disclosed to the upstream service.  
**Fix:** in `Rewrite`, delete `Cookie`, `Proxy-Authorization`, `X-Forwarded-For`,
`X-Forwarded-Host`, `X-Forwarded-Proto`, and `Forwarded`; then set only the
forwarding headers the deployment deliberately wants. Reject `CONNECT` at the
mux. Add a test that the upstream never sees those headers.

**v0.1 decision — accepted, not fixed (owner: API/private-access).** The private
route still requires a bearer token and a device grant, so this is unnecessary
disclosure rather than an access bypass. Until the rewrite deletes those
headers, do not put control-plane cookies or proxy credentials in front of the
same origin. The fix is small and remains queued for the next private-access
change.

### S3 — Low — SSH identity file check/read is a TOCTOU pair

**Where:** `internal/transport/ssh.go:112-122`.  
**Why:** the code `Stat`s the identity to check mode, then calls `os.ReadFile`
on the path. A writer in the identity's parent directory could replace the file
with a symlink between those calls; the permission check would apply to the old
file. The identity is already operator-controlled, so this is defence in depth,
not an unauthenticated attack.  
**Fix:** `os.Open` once, `f.Stat` the opened file, verify regular file and
`&0077 == 0`, then read from that handle with a size bound. Reject a symlink
with `Lstat` before opening (or use `O_NOFOLLOW` on Unix). Add a test with a
symlink swapped after stat.

**v0.1 decision — accepted, not fixed (owner: transport).** The SSH identity is
operator-controlled and the attack requires write access to its parent
directory, so this is defence in depth. Keep identity directories root-owned
and not group/world-writable. The opened-handle fix remains queued for the next
transport change.

### S4 — Low — Webhook replay is bounded by operation idempotency retention

**Where:** `internal/api/webhook.go`; idempotency behavior is owned by the
operations store.
**Why:** GitHub signatures cover no timestamp, so a captured valid request can
be replayed. A replay with the same `X-GitHub-Delivery` returns the original
operation, which is the correct side effect. If operation/idempotency rows are
ever purged while the secret is still valid, an old delivery could create a new
operation. The signature itself cannot expire the request.
**Fix implemented:** migration `008_webhook_deliveries.sql` adds
`eregion_webhook_deliveries`. The handler reserves a delivery before submission
and completes it with the accepted operation ID. Only SHA-256 of the delivery ID
and exact body, the actor/kind/target, and the operation ID are stored — never
the secret, raw signature, raw delivery ID, or body. Operation retention and
delivery retention are independent. A known delivery with different body bytes
is refused with `conflict`; a replay returns the recorded operation ID even when
the operation row no longer exists. The reservation is written before
`Submit`, then completed with the accepted operation ID; it is deliberately
independent of the operation transaction so a process crash leaves a pending
reservation that the next replay completes with the same idempotency key. The
store is PostgreSQL-backed in production and shared by every control plane; an
in-memory implementation exists for unit tests only.

**Tests:** `TestWebhookReplaySurvivesOperationRetention` (API unit) and
`TestIntegrationWebhookDeliverySurvivesOperationRetention` (real PostgreSQL;
deletes the operation/idempotency row before replaying).

### S5 — Informational — Bearer-token lookup is not constant time

**Where:** `internal/api/auth.go:108`.  
**Why:** the SHA-256 digest is used as a Go map key. Map lookup timing can
depend on the key, but the attacker would need to steer a 256-bit digest to a
known bucket; no practical token-recovery path was demonstrated. Every request
still requires a 32–256 character bearer token and one exact hash match.  
**Fix (optional):** iterate the small principal set (≤128) and compare digests
with `subtle.ConstantTimeCompare`, or keep the map but document the timing
assumption. Do not replace the hash with a reversible store.

### S6 — Informational — Secret request body is copied into a Go string

**Where:** `internal/api/vault.go:348-360` (`strings.NewReader(string(raw))`).  
**Why:** the bounded plaintext body is held as both `[]byte` and `string` until
garbage collection. The decoded value is zeroed (`vault.go:258-266`), but the
base64/raw copies are not. This is memory-lifetime hygiene, not a disclosure:
no error, event, or log prints the body.  
**Fix:** decode from `bytes.NewReader(raw)` and zero `raw` in a deferred loop;
avoid the `string(raw)` conversion.

## Explicitly unproven here

- No external penetration test, fuzzing campaign, or supply-chain review was run.
- No live Cloudflare/TLS/WAF configuration was reviewed; the repository only
  contains the client-side integration and its policy checks.
- No multi-host HA/failover guarantee is made. See
  [release-criteria.md](release-criteria.md); publish only failover behavior
  that was actually drilled.
- Findings S1–S3 are accepted for the experimental v0.1 line with the owners and
  scoped mitigations above; they are not fixed. S4 is fixed and covered by unit
  and PostgreSQL integration tests. S5–S6 are informational and accepted.

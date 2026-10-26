# Security review notes

**Not an external audit.** Findings S1–S6 come from a 2026-10-20 review; the
boundaries below were re-read from source on 2026-10-26 without a penetration
test or live host inspection. Work and status live in [MASTER](MASTER.md).

## Controls to preserve

- API authentication resolves actor identity from hashed bearer credentials,
  never a request body's actor. Transport scopes and exact action/target grants
  are separate. Preview uses authorization too. CI/webhook credentials without
  explicit platform grants do not gain general platform authority.
- Operator/system principals without grants currently retain administrative
  compatibility authority. Do not distribute such tokens to automation or claim
  that current operator contexts are token-free.
- Vault values are encrypted with namespace/revision binding; master keys stay
  outside PostgreSQL. Export is separate and explicit. Plaintext is forbidden in
  ordinary action input, River arguments, events and node journals.
- Worker-node transport pins SSH host identity, checks node protocol/name and
  attested identity where applicable, and reconciles status after response loss.
  This does not prove full binary version lockstep or complete node hardening.
- Secret/terminal redaction precedes output; untrusted control sequences must not
  execute in the operator terminal. Protect handoff and clipboard material too.
- Git sources use explicit HTTPS hosts, execution-time address checks and
  redirect/protocol restrictions. Builds still run host-trusted source code:
  Docker access and main-node builds are not a tenant sandbox.
- Webhooks validate bounded raw-body HMAC, repository/ref allowlists and durable
  delivery/body hashes. Signature authenticity is not an expiry mechanism.
- Private proxy strips credentials/cookies/forwarding headers. Grants and access
  decisions fail closed if persistent authorization/audit is unavailable.
- Upgrades require a separately trusted key, signed manifest and artifact digest;
  checksum listings or a successful version command alone are insufficient.

## Historical findings retained

These preserve prior triage, not a new acceptance decision for every future
release. Re-check against the build being shipped.

| ID | Previous finding | Source-level disposition |
| --- | --- | --- |
| S1 | Git source SSRF/redirects | Allowlist, address re-resolution and git redirect/protocol restrictions exist. Residual DNS-check-to-git-dial rebinding window and explicitly allowed private forges remain. |
| S2 | Proxy leaked caller credentials/forwarding headers | Stripping exists in `internal/cli/extensions.go`, with regression test. |
| S3 | SSH identity check/read race and symlink acceptance | Still visible in `internal/transport/ssh.go`: Stat then ReadFile. Low severity with protected parent directories; open-once/bounded/no-symlink handling remains needed before v1. |
| S4 | Webhook replay after operation retention | Migration 008 and durable reservation/completion store exist independently of operation rows. |
| S5 | Hashed bearer map lookup not constant-time | Historical informational finding; no practical token-recovery attack demonstrated by that review. |
| S6 | Secret request body copied into string | Historical informational memory-lifetime concern; do not treat value zeroing as erasure of every Go allocation. |

## Boundaries requiring re-review before qualification

1. **New private access/setup.** `serverinit_access.go`, `internal/access/scripts`
   and provider firewall behavior need actual switchover/timeout/reboot testing.
   Lockdown uses firewall policy; do not assert sshd binds only wg0 without
   checking the actual config. Root/Docker administrative authority is explicit.
2. **Handoff/login.** Current handoff includes the operator's SSH/WireGuard keys
   and the external DSN, not an API token; local identity/password files
   persist. Import validates/writes files and changes context before its final
   node-RPC readiness check. Existing pin handling, overwrite/conflict safety,
   secret lifetime and JSON/no-TTY prompting need regression review. Encrypted
   transport bundles are still credentials.
3. **Destructive approval.** CLI rollback's public intent digest is not a server
   authorization token; raw authenticated action submission can bypass that CLI
   check. Define server-side exact-target/revision checks for new deletion paths.
4. **Caddy.** Admin port 2019 is administrative authority and must stay local.
   Domain rows precede reload, reload uses direct file writes, and DB/file/proxy
   state can diverge. No observed route/TLS result is persisted. Public hostname
   collision with the main control hostname and concurrent writes need coverage.
5. **Automation.** Expiry, revocation, human-factor issuance and narrow public
   deployment ingress are requirements, not implemented protections. Never expose
   all admin routes to make CI work. `--yes` is not a human factor.
6. **Cross-host/state recovery.** DB state plus file-backed secrets, node evidence,
   resource state and credentials are separate recovery dependencies. Shared
   PostgreSQL alone is not HA or a complete backup.

Record actual security/recovery qualification in MANUAL-TESTING.

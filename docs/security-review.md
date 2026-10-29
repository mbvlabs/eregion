# Security review notes

**Not an external audit.** Findings S1–S6 come from a 2026-10-20 review; the
boundaries below were reconciled with source on 2026-10-28 without a penetration
test or live host inspection. Work and status live in [MASTER](MASTER.md).

## Controls to preserve

- API authentication resolves actor identity from hashed bearer credentials,
  never a request body's actor. Transport scopes and exact action/target grants
  are separate. Preview uses authorization too. CI/webhook credentials without
  explicit platform grants do not gain general platform authority.
- Operator contexts are token-free SSH/WireGuard identities. Their OS credential
  holder is root-equivalent; the D7 factor is an application boundary, not OS
  isolation. CI tokens must remain scoped and expiring; internal administrative
  principals are not automation credentials.
- Vault values are encrypted with namespace/revision binding; master keys stay
  outside PostgreSQL. Export is separate and explicit. Plaintext is forbidden in
  ordinary action input, River arguments, events and node journals.
- Worker-node transport pins SSH host identity, checks node protocol/name and
  attested identity where applicable, and reconciles status after response loss.
  This does not prove full binary version lockstep or complete node hardening.
- Secret/terminal redaction precedes output; untrusted control sequences must not
  execute in the operator terminal. Protect handoff and clipboard material too.
- Git sources use explicit HTTPS hosts, submission- and execution-time address checks and
  redirect/protocol restrictions. Builds still run host-trusted source code:
  Docker access and main-node builds are not a tenant sandbox.
- Webhooks validate bounded raw-body HMAC, repository/ref allowlists and durable
  delivery/body hashes. Signature authenticity is not an expiry mechanism.
- Private proxy strips credentials/cookies/forwarding headers. Grants and access
  decisions fail closed if persistent authorization/audit is unavailable.
- Upgrades require a cosign keyless signature bound to the pinned publish-workflow
  identity, a verified manifest and artifact digest;
  checksum listings or a successful version command alone are insufficient. A
  published node resolves the manifest itself for an `upgrade.install` upload and
  accepts only its own `linux/<arch>` artifact; only a `dev` node keeps the
  checksum-only developer path.
- The D7 human factor (a TOTP code from `eregion protect init`) is verified by
  the node inside the request that performs token creation, either export,
  `app delete` or `env delete`; a separate CLI pre-check no longer exists, so a
  direct RPC cannot skip it. `--yes`, `--json`, stdin and pipes never satisfy
  it, dry runs never need it, and the HTTP `/v1/operations` route refuses a real
  delete submission whatever the credential's grant. The factor stops an agent
  operating through eregion from reading secrets, minting tokens or deleting
  apps; it is not a sandbox, because an OS account holding a context's files
  (SSH key, sudo password) is root-equivalent on that server.

## Pre-publication credential review — 2026-10-28

**Result: no live credential was identified in the material inspected.** This is
not a guarantee that every secret format or external GitHub surface is covered.
The repository was still private during this read-only review; visibility,
repository settings and credential values were not changed.

Scope and evidence:

- Candidate and remote `master`: `ed6e4f3`. Gitleaks **v8.24.3**, default rules,
  recursive decoding depth 3, redacted reports, no project suppressions or
  inline allow-comments honored. Scanned the complete tracked HEAD tree and
  all locally retained branch/tag history (**232 commits**, including lane and
  backup branches), not just the current files.
- Six history candidates (two present in HEAD) were manually triaged: repeating
  hexadecimal test tokens in API/CLI/webhook tests, a test assertion's secret
  variable name, an old resource-command example and WireGuard's literal
  `replace_allowed_ips` UAPI directive. These are fixtures/code, not identified
  operational credentials. No rotation or history rewrite is indicated by them.
- An additional local sweep inspected **1,993 Git blobs**, including locally
  unreachable objects, for private-key blocks, provider/GitHub tokens and
  credential-bearing URLs/assignments. Private-key markers were incomplete test
  fixtures; URL credentials were test/example or loopback fixture values. No
  live SSH/WireGuard key, production DSN or provider token was identified.
- GitHub exposed one branch (`master`), no tags/releases, no Actions artifacts,
  and no issues/PRs (including closed). Wiki and Pages were disabled. Scanned
  available logs from **81 of 82 workflow runs** with no Gitleaks findings; the
  candidate's unfinished CI run `40766668339` had no downloadable logs and was
  not covered. Future logs require a fresh check.
- Repository variables were empty; environment variables were release bucket
  and public release URL configuration. R2 credentials are referenced through
  GitHub environment **secrets**, not embedded values. Their encrypted values
  cannot be read by this audit and were not fetched. Making the repository
  public does not itself disclose those secret values, but workflow access to
  them must remain restricted.
- `scratchpad.md`, `.claude/settings.local.json` and `bin/eregion` are ignored,
  untracked local files, not publication contents. Ignore rules are not access
  control: do not force-add local credentials, handoffs, contexts or audit logs.
  Scanner reports/log copies were kept outside the repository in an owner-only
  local audit directory, not added to Git.

Recommended GitHub hardening before accepting public contributions:

1. Change the repository's default Actions token permissions from **write** to
   **read**, and disable permission for Actions to approve pull requests. Current
   workflows explicitly use `contents: read` (publish additionally needs
   `id-token: write`), so the broad defaults are unnecessary.
2. The `edge` and `stable` environments currently have no protection rules or
   deployment-ref restrictions. Restrict edge to `master` and stable to intended
   release tags; consider required approval for stable credential use. Current
   publish triggers are push-to-master and version tags, not pull requests.
3. Keep fork PR builds unprivileged. CI references no production secrets and
   there are no repository self-hosted runners. Do not introduce privileged
   `pull_request_target` execution of contributor code. The CI PostgreSQL
   password is an intentional disposable-runner fixture, not a production key.

Residual limits: pattern-based detection can miss custom/encoded secrets;
non-GitHub external storage and private machine credential stores were not
inspected. No remote history was rewritten, secrets rotated, workflow run
triggered or repository visibility changed. Recheck newly generated content
before publication if the candidate changes.

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
   persist. Import now stages the identity and host pin, verifies readiness, then
   replaces the named identity/context; replacing an existing name needs exact
   `--replace`. Isolated regressions cover failure preserving the old files.
   Selected-context local identity reads and RPC routing share one resolver;
   init readiness/doctor explicitly target the context being initialized.
   Real-host import, secret lifetime and prompting remain unqualified. Encrypted
   transport bundles are still credentials.
3. **Destructive approval.** The node validates rollback's exact-target
   confirmation; real app/environment deletes additionally verify the D7 factor
   inside their submission request. Agent-usable delete previews need no factor.
   These source-level controls still need real-host qualification.
4. **Caddy.** Admin port 2019 is administrative authority and must stay local.
   Domain rows precede reload; serialized writers adapt, fsync/rename and load
   the fragment, restoring the old file on rejection. Desired/applied route,
   DNS, certificate and public-response observations are persisted separately;
   DB/file/proxy state can still diverge until reconciliation. Concurrent writes,
   failed reload recovery and hostname collision handling need host evidence.
5. **Automation.** Expiry, revocation, same-request human-factor issuance and
   narrow CI/webhook ingress are implemented at source level, not host-qualified.
   Never expose all admin routes to make CI work. `--yes` is not a human factor.
   Release-command recovery requires matching nonempty effect inputs before
   accepting terminal node evidence; missing or mismatched evidence preserves
   ownership and cannot authorize another migration run.
6. **Cross-host/state recovery.** DB state plus file-backed secrets, node evidence,
   resource state and credentials are separate recovery dependencies. Shared
   PostgreSQL alone is not HA or a complete backup.

Record actual security/recovery qualification in MANUAL-TESTING.

# eregion alignment spec: deploycrate-ce, shaped like vaire

**Status:** draft for review · 2026-10-26

## 1. What eregion is

The functionality of **deploycrate-ce** (deploy and operate applications on your
own servers), delivered the way **vaire** delivers PostgreSQL: **one glorious
CLI**, agent first, human friendly. No web UI. The terminal UI is a rendering of
the same commands, not a separate product.

- **Functional requirements:** deploycrate-ce.
- **Shape, flows, conventions:** vaire (`docs/MASTER.md`, `AGENTS.md`).
- **Hard constraints already given:** Go and bash only. One `eregion` binary for
  every role. External PostgreSQL for control-plane state (never Postgres on the
  application servers). Debian 13 and Ubuntu 26.04 only. No local test harnesses;
  you test by hand on a real server, I implement.

## 2. Why it drifted

eregion was built milestone by milestone from `docs/initial_research.md` as an
**HTTP API platform**: a control plane (API + River workers) that clients reach
with bearer tokens. That is deploycrate's internal architecture with the web
UI removed, not vaire's operator model. Every fix today (server init, handoff,
login) bolted vaire pieces onto that shape. This spec fixes the shape first.

## 3. The control model (D1, decided)

**The CLI always talks to a node through SSH over WireGuard,** like vaire. Every
operator command (`eregion app …`, `eregion deploy …`) resolves the context, opens
SSH inside WireGuard to the node, and runs the same command there in node mode
(`eregion node <cmd> --json`) against the API on loopback. Contexts hold a
WireGuard identity and an SSH key from the handoff bundle, not a bearer token.

**CI and other automation get one public endpoint with a scoped token.** GitHub
Actions and similar trigger deployments over HTTPS at `https://<node>.<domain>`
(Caddy, ACME certificate) with a deploy-scoped token: submit a deploy, read its
status. Nothing else of the API is public. Tokens are created with
`eregion token create --scope deploy` and shown once.

**`server init` asks for a domain, like vaire.** Each node is `<node>.<domain>`;
init shows the DNS record, waits, checks it against public resolvers, and Caddy
gets the certificate. A load balancer for HA later gets its own name in front of
the main node; nothing else changes.

```
 operator / agent                                  VPS: main node <node>.<domain>
┌──────────────────────────┐   WireGuard (UDP)   ┌──────────────────────────────────┐
│ eregion (operator mode)  │═════════════════════▶│ sshd on wg0 only                 │
│  context from handoff    │   SSH inside WG      │ eregion node <cmd> --json        │
│  userspace WireGuard     │─────────────────────▶│   └ calls the API on 127.0.0.1   │
│  in-process SSH          │                      │ eregion API + River worker       │
└──────────────────────────┘                      │ Docker · buildpacks · registry   │
 CI (GitHub Actions) ── HTTPS + deploy token ────▶│ Caddy: CI endpoint + app domains │
                                                  │ telemetry collector              │
                                                  └───────────────┬──────────────────┘
                                                                  │ TLS
                                                    external PostgreSQL (control state)
```

## 4. Operator lifecycle, flow by flow

| # | vaire flow | eregion today | Work |
|---|---|---|---|
| 1 | Local preflight: target reachable, SSH credential, missing tools with install offer, sudo escalation | Linux build check only | Port vaire preflight |
| 2 | `server init` interactive, bare or `--host` | **Done today** | — |
| 3 | `--answers answers.toml`, every question also a flag | Flags only | Add answers file |
| 4 | First-contact auth: `--identity`, ssh-agent, `~/.ssh` keys, encrypted-key prompt, `--password-stdin` | All but password | Add `--password-stdin` |
| 5 | Host key TOFU with fingerprint confirm, `--host-key-fingerprint`, `--accept-new-host-key` | **Done today** | — |
| 6 | Push binary; **version lockstep** check on every command; `--auto-upgrade-nodes` | Uploads each run; no lockstep | Install once, handshake + lockstep |
| 7 | Hardening (vaire): admin user + sudo password, root login and passwords off, firewall | None | Port from vaire (D2) |
| 8 | WireGuard hub `10.x.0.1`, operator peer, embedded userspace tunnel | Node WireGuard transport exists; no operator peer | Add operator peer + userspace tunnel |
| 9 | **Commit-confirm switchover**: SSH moves to wg0, public 22 closes, server timer reverts unless confirmed | Engine supports commit-confirm; not wired | Wire it |
| 10 | **Domain question** (`<node>.<domain>`), show the DNS record, wait, check against public resolvers, ACME certificate | None | Add: it gives the API its URL (§3) and ingress its hostnames (§5) |
| 11 | Secrets handoff: plain record + **age transfer bundle** + generated passphrase, copy/save/show, type `stored`, clear screen | Plain record only (today) | Add bundle + passphrase |
| 12 | `login`: paste bundle → context | None | Add |
| 13 | `context list/use/remove`; current context | Token-based, SSH tunnel (stopgap) | WireGuard + SSH identity (D1) |
| 14 | Pending init: `--resume`, `--discard`, refuse when a context or pending init exists | Server journal resumes; no local pending state | Add |
| 15 | `secrets export [--rotate-operator]`, `operator add/revoke` | None | Later |
| 16 | `doctor`: drift, security, backups, disk, certs, versions | None | Add |
| 17 | `ssh [node]` over WireGuard; `node unlock-ssh` break-glass | None | Add |
| 18 | `upgrade` (CLI) + node upgrade from operator, signed | Signed upgrades exist | Align commands |
| 19 | `commands --json`, `guide agents`, `skill install` | First two exist | Add `skill install` |
| 20 | Live progress UI, cards, error boxes (vaire `ui` package) | **Ported today** | — |

## 5. deploycrate-ce functionality

| Area | deploycrate-ce | eregion today | Work |
|---|---|---|---|
| Apps, environments | Apps with environments, slugs, delete cascades | API + CLI exist | Real-host test |
| Sources | GitHub App, git, local directory, webhooks | HTTPS git + local dir; webhook code exists | GitHub App later |
| Builds | Buildpacks (pinned pack + Paketo builder) | Exists | Real-host test |
| Releases | Immutable, image or build | Exists | — |
| Deploys | Blue/green slots, health-first switch, rollback, release commands | Health-first candidate switch, rollback | Real-host test |
| **Ingress, domains, TLS** | **Caddy routes, custom domains, ACME, DNS records** | **Missing** | **Add Caddy on main node + `domain` commands** |
| DNS providers | Cloudflare connections | Cloudflare DNS plan/reconcile exists | Real-host test |
| Env secrets | Per-environment secrets | Vault + `secret` exists | — |
| Resources | Postgres (and catalog) per environment | Postgres resources, backups, recovery | Real-host test |
| Backups | S3/R2 destinations, restic server snapshots, logical DB backups, restore | Resource backups exist | Out of scope now; server snapshots later (D4) |
| Nodes | WireGuard private network, enrollment, roles | Enroll over pinned SSH, WireGuard reconcile | Real-host test |
| Networks | Private networks, access rules | `access` commands exist | — |
| Telemetry | Collector + Prometheus (no ClickHouse here) | Telemetry node role; nothing on other hosts | Collector on every server; storage + UI on telemetry node (D5) |
| Self-update | Signed channels | Signed upgrades | Align |
| Registry | Managed registry per server | Local OCI registry on main | — |

## 6. Command tree (target)

```
eregion server init | status | doctor          # vaire flows (§4)
eregion login | context list|use|remove
eregion app create|list|delete
eregion env create|list
eregion deploy [--release …] | rollback        # plan/confirm/progress like server init
eregion release create|list
eregion domain add|list|remove                 # new, with DNS check
eregion db create|list|backup|restore          # resources renamed for humans
eregion secret set|get|list
eregion node add|list|status|remove            # vaire's `node add` shape
eregion logs | metrics
eregion ssh [node]
eregion upgrade
eregion ui
eregion commands | guide | skill install
```

Each mutating command in a terminal is guided like `server init` (questions with
defaults, plan, confirm, live progress, card); with `--json` or no TTY it takes
flags only and never prompts.

**Removed from the operator surface:** `bootstrap` (becomes node mode, used by
`server init` and `node add`), `bootstrap --interactive` wizard, `serve` /
`worker` (node mode), `platform submit` and other raw-API verbs, bearer-token
`context login`.

## 7. Decisions (2026-10-26)

- **D1** CLI → SSH over WireGuard to each node, always. A public HTTPS endpoint
  with a deploy-scoped token exists only for CI to trigger deployments (§3).
- **D2** Hardening follows vaire, not deploycrate.
- **D3** Caddy ingress on the main node.
- **D4** No backups for now; server snapshots later.
- **D5** Every server runs the telemetry collector for its own host data; storage
  and the telemetry UI run on a separate telemetry node. **No ClickHouse.**
- **D6** Command tree as §6; `platform …` and `resources …` are replaced.
- **D7** API tokens are per use and human-approved, never one long-lived
  token. No token is minted at setup or handed off. Each token (GitHub
  Actions, a script, an agent session) is created for one purpose with a
  scope and an expiry date, and creating one needs a human factor like vaire's
  `protect init`: a TOTP code from an authenticator (or a master password),
  read without echo at a terminal, so an agent cannot mint its own. Tokens are
  listed and revocable; the secret is shown once. Operator CLI access does not
  need a token: it goes over SSH inside WireGuard (D1).

## 8. Order of work

1. `server init` transport, the vaire way: hardening (D2), WireGuard hub +
   operator peer + embedded userspace tunnel, commit-confirm switch of SSH to
   wg0, version lockstep, age transfer bundle + `login`, contexts on
   WireGuard/SSH. Operator commands run in node mode over that channel.
2. Domain question + DNS check + Caddy with ACME for `<node>.<domain>` (done);
   the CI endpoint with per-use, expiring tokens (D7, later: TOTP setup,
   `token create --scope --expires`, list, revoke; drop the setup token).
3. Guided `app create` → `deploy` on the main node; app domains through Caddy.
   First end-to-end test on your server.
4. Telemetry collector on every server (D5); `doctor`, `ssh`, `node add`.
5. Command-tree rename (D6) and removal of the raw surfaces.

# Eregion: where we stand

**Bottom line:** Eregion has most of the core Deploycrate functionality and broadly follows Vaire’s CLI architecture. Your fresh-VPS deployment proves a useful happy path works. What remains is **correctness, safety, a smoother guided experience, and failure-path testing**—not a large new feature build-out.

| Question | Assessment |
|---|---|
| **Deploycrate feature compatibility?** | **Broad coverage, incomplete parity.** Apps/environments, Git builds, images, processes, deploy/rollback, domains, secrets, logs and CI/webhooks have implementations. Important edge cases remain. |
| **Vaire CLI design compatibility?** | **Strong structure, uneven experience.** Single binary, SSH/WireGuard, guided init and confirmations align. App creation stops before deployment completes; automation and output contracts have inconsistencies. |
| **Roadmap position?** | **Implementation-heavy, qualification-light.** Rows 1–10 have substantial code, but should not be treated as closed. The alpha acceptance run is not recorded. |

## The gaps that matter most

### 1. Safety and agent reliability — address first
- **Secret export’s human-factor check can be bypassed through authenticated node RPC.** Privileged binary replacement also conflicts with the promise that an operator-capable agent cannot bypass protection. This is an operator trust-boundary issue, **not evidence of public internet exposure**.
- **Dry-run can upgrade/restart a mismatched node** when auto-upgrade is enabled.
- **Failed login imports can overwrite existing local credentials.** Same-key app creation retries and machine log output also need contract fixes.

### 2. Deploycrate behavior — close the concrete gaps
- **Git subdirectory/monorepo builds ignore the configured context.** Failed branch-build retries may use a newer commit.
- **Cleanup can hide container-removal failures.** Equivalent repair of missing serving containers was not found and needs confirmation.
- **Release commands run per deployment**, unlike Deploycrate’s successful-once-per-release behavior. Decide the intended semantics before relying on restart/rollback for non-idempotent commands.

### 3. Vaire-style finish — complete the operator journey
- Guided app creation should **follow build/deploy to a serving or failed result**, rather than stop at acceptance.
- Init should enforce its promised **final doctor gate**. Align flags/defaults, log JSON/stream behavior and explicit context targeting.

## What should happen next?

**First:** fix the safety boundaries and deterministic build/cleanup behavior.  
**Then:** finish guided create and reconcile the existing roadmap/docs with the actual gaps.  
**Finally:** record a real-server acceptance run: interrupted init/recovery, failed deployment, traffic switch, rollback, CI/webhook denial paths, and deletion isolation. Verify HTTPS externally—local Caddy checks alone do not prove public reachability.

**Do not expand scope:** multi-node, HA, telemetry node, Cloudflare automation and GitHub App integration remain deferred, not missing alpha work.

*Confidence: three source audits, not reproduced failures or a test pass. Eregion snapshot `a7478c4`; no code or live infrastructure changed. Vaire has uncommitted work, so that comparison is a working-tree snapshot. Your successful deployment is real user-reported evidence, but not a complete alpha qualification.*

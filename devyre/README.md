# Devyre CLIProxyAPI

Devyre's fork of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA). It pools several Claude subscriptions, plus optional Codex accounts, behind one endpoint on this PC. The endpoint is published only on the tailnet and serves a management panel with a quota ledger modeled on Theo's setup. T3 Code and Claude Code send their Claude traffic through it.

The design, the verified facts and the decisions behind this fork are in [PLAN.md](PLAN.md). Upstream's own documentation still applies to everything not listed here.

## What differs from upstream

- **Panel source.** The server downloads `management.html` from the releases of [Devyre/Cli-Proxy-API-Management-Center](https://github.com/Devyre/Cli-Proxy-API-Management-Center): upstream's panel plus chhoumann's quota ledger, a 7-day Claude headline with Fable hidden in the ledger, and a dark default theme. When the panel file is missing and the release lookup fails, the fallback also downloads from that fork's latest release. It never falls back to the stock panel.
- **T3 Code hub contract.** `TestT3Hub_*` pin the deprecated `/v0/management` endpoints that T3 Code's "CLIProxyAPI hub" decodes (`auth-files`, `api-call`, `reset-quota`), plus `/v8/management/requests/api-call`, which the panel uses.
- **Quota readings and usage cache.** One in-memory store of quota readings, fed by response headers, by usage bodies passing through `api-call`, and by an idle-credential poller. A usage cache inside `APICall` keeps the panel, the T3 hub and the poller from tripping Claude's rate-limited usage endpoint.
- **`expiring-first` routing.** A built-in strategy that prefers the credential whose quota would be lost soonest. Session affinity still keeps a thread on its credential. `GET /v8/management/routing/quota-readings` shows the ranking.
- **Deploy kit.** Docker Compose on loopback only, `tailscale serve` on port 8318, PowerShell scripts for secrets, start-up, backup and upstream syncs, and client snippets.

### Fork-only files

| Path | Purpose |
|---|---|
| `devyre/PLAN.md` | Implementation plan and decision log |
| `devyre/README.md` | This file |
| `devyre/deploy/docker-compose.yml` | Builds `cpa-devyre:current`; publishes 8317, 54545 and 1455 on 127.0.0.1 only; mounts `CPA_HOME` at `/data` |
| `devyre/deploy/config.template.yaml` | Runtime config template (v8 layout) that `new-secrets.ps1` renders |
| `devyre/deploy/.env.example` | Compose variables; copy it to the gitignored `.env` |
| `devyre/scripts/new-secrets.ps1` | Creates the runtime folders and secrets, renders `config.yaml`, saves the public URL |
| `devyre/scripts/up.ps1` | Builds and tags the image, starts the container, waits for `/healthz` and the panel |
| `devyre/scripts/tailscale-serve.ps1` | Publishes CPA at `https://<machine>.<tailnet>.ts.net:8318` |
| `devyre/scripts/backup.ps1` | Archives `config.yaml`, `auths\` and `secrets\` |
| `devyre/scripts/sync-upstream.ps1` | Merges `upstream/main` into a sync branch and runs the checks |
| `devyre/clients/powershell-profile.ps1` | Claude Code CLI wiring: `Enable-CpaPool`, `Disable-CpaPool`, `claude-direct`, `claudex` |
| `devyre/clients/t3-code.md` | Click-by-click T3 Code setup: hub, pooled Claude instance, Claude Direct |
| `devyre/clients/codex-config.toml` | Optional Codex CLI provider |
| `internal/managementasset/devyre_updater_test.go` | Pins the panel source to the fork |
| `internal/api/devyre_t3_hub_contract_test.go` | `TestT3Hub_*`, the T3 Code hub contract |
| `internal/config/devyre_deploy_template_test.go` | Loads the deploy template through the real config loader |
| `internal/quotareading/` | Readings model, parsers and store |
| `internal/config/devyre_routing.go` | `routing.expiring-first` and `routing.quota-observation` config |
| `internal/api/handlers/management/devyre_usage_cache.go` | Usage cache inside `APICall` |
| `internal/api/handlers/management/devyre_quota_observer.go` | Idle-credential usage poller |
| `internal/api/handlers/management/devyre_routing_readings.go` | `GET /v8/management/routing/quota-readings` |
| `sdk/cliproxy/auth/selector_expiring_first.go` | The `expiring-first` selector |

Go files listed above have `_test.go` companions. To list the real fork diff, including any file added since this table was written, run:

```powershell
git diff --stat upstream/main...main
```

### Conflict hotspots

These are the upstream files the fork edits. Each edit is a small hunk, marked with a `// devyre:` comment where the reason is not obvious. When a sync conflicts here, keep upstream's new code and re-apply our hunk.

| File | Our change |
|---|---|
| `internal/managementasset/updater.go` | `defaultManagementReleaseURL` and `defaultManagementFallbackURL` point at the Devyre panel releases. Never restore `cpamc.router-for.me`. |
| `internal/config/config_defaults.go` | `DefaultPanelGitHubRepository` points at the Devyre panel fork. |
| `config.example.yaml` | `management.panel-github-repository` is the fork; `routing:` documents `expiring-first` and the `expiring-first` and `quota-observation` blocks. |
| `internal/api/handlers/management/api_tools.go` | Three hunks hook the usage cache into `APICall`: a lookup after `auth_index` and `url` are parsed, complete or fail after the upstream call, and a record on success. `/v0` and `/v8` share this handler. |
| `internal/api/server.go` | Starts the quota observer once, right after the management handler is created. |
| `internal/api/server_management_v8.go` | One line registers `GET /v8/management/routing/quota-readings`. |
| `sdk/cliproxy/service_config.go` | `expiring-first` aliases in `normalizedRoutingRuntimeState`, construction in `newRoutingSelector`, and the gate and log settings in `routingRuntimeState`. That struct must stay comparable: no pointers, maps or slices. |
| `internal/api/handlers/management/config_basic.go` | `normalizeRoutingStrategy` accepts `expiring-first`, so `PUT /routing/strategy` does too. |
| `internal/config/config_types.go` | The `Strategy` comment, plus the `ExpiringFirst` and `QuotaObservation` fields on `RoutingConfig`. |

If upstream deletes the `/v0/management` routes, `TestT3Hub_*` fails first. Restore the three routes T3 uses (`GET auth-files`, `POST api-call`, `POST reset-quota`) in `internal/api/devyre_v0_shim.go`.

## How to run

This runs on this PC with Docker Desktop, and Tailscale fronts it. Prerequisites:

- Docker Desktop running, with "Start Docker Desktop when you sign in" enabled.
- Tailscale logged in, with MagicDNS and HTTPS certificates enabled for the tailnet (admin console, DNS page).
- A published panel release on the Devyre panel fork that includes `management.html`.

Steps, from the repository root in PowerShell:

1. **Secrets and config.** Run `devyre\scripts\new-secrets.ps1`. It creates `%USERPROFILE%\.cli-proxy-api\{auths,logs,static,plugins,secrets}`, restricts `secrets\` to your user, generates the management key and one key per client (`t3-code`, `claude-code-cli`, `codex-cli`, `other-devices`), and renders `config.yaml`. It prints the management key once: save it in your password manager. Client keys stay in `secrets\client-*.txt` and are never printed.
2. **Compose variables.** Copy `devyre\deploy\.env.example` to `devyre\deploy\.env`, then set `CPA_HOME`, `CPA_TZ` and, after step 4, `CPA_PUBLIC_URL`.
3. **Build and start.** Run `devyre\scripts\up.ps1`. It builds `cpa-devyre:current` and tags the same image `cpa-devyre:<git sha>` for rollback (`<git sha>-dirty` when tracked files have local changes). Then it runs `docker compose up -d` and waits for `http://127.0.0.1:8317/healthz` and `/management.html`.
4. **Publish on the tailnet.** Run `devyre\scripts\tailscale-serve.ps1`, which serves `https://<machine>.<tailnet>.ts.net:8318`. Then save that URL for the PowerShell profile with `devyre\scripts\new-secrets.ps1 -PublicUrl https://<machine>.<tailnet>.ts.net:8318`.
5. **Log in accounts.** Open `https://<machine>.<tailnet>.ts.net:8318/management.html` and log in with the management key. Under OAuth Login, add each Claude account, using a separate browser profile or private window per account.
6. **Wire the clients.**
   - T3 Code: follow `devyre\clients\t3-code.md`.
   - Claude Code CLI: append `devyre\clients\powershell-profile.ps1` to `$PROFILE`. In a new shell, once the accounts are logged in, run `Enable-CpaPool`.
   - Codex CLI (optional): see `devyre\clients\codex-config.toml`.

Day to day:

- After pulling changes, run `up.ps1`. To restart without building, run `up.ps1 -NoBuild`.
- Logs are in the panel's Logs Viewer, or run `docker logs -f cpa`.
- To rotate secrets, run `new-secrets.ps1 -Rotate`, then `up.ps1 -NoBuild`. Then update the password manager, the T3 hub key and T3's `ANTHROPIC_AUTH_TOKEN`. The previous `config.yaml` is kept as `config.yaml.bak-<timestamp>`.
- To bypass the pool, use `claude-direct` for one run, or `Disable-CpaPool`. In T3, use the "Claude Direct" instance.

## Sync with upstream

1. Run `devyre\scripts\sync-upstream.ps1`. It requires a clean tree, fast-forwards `main` from `origin`, creates `sync/upstream-YYYYMMDD` and merges `upstream/main`. It then runs `gofmt -l`, `go build ./cmd/server` and `go test ./...`. It never pushes.
2. If the merge conflicts, resolve the conflicts at the hotspots above, keeping upstream's behavior plus our hunks. Commit the merge, then run the checks by hand. `go test ./internal/api/ -run T3Hub` must stay green.
3. Push the branch and open the PR against the fork, never against upstream:
   `gh pr create --repo Devyre/CLIProxyAPI --base main --head sync/upstream-YYYYMMDD`.
4. Merge the PR, tag `v<upstream version>-devyre.<n>`, and run `up.ps1`.

The panel fork syncs separately in its own repository: merge `upstream/main`, check `chhoumann/dev` for ledger updates, run `bun run verify`, then tag `v<upstream version>-devyre.<n>`. The server picks up a new panel release at its next start, or within 3 hours.

## Rollback

- **Server image.** `docker images cpa-devyre` lists the builds. Run `docker tag cpa-devyre:<previous-sha> cpa-devyre:current`, then `devyre\scripts\up.ps1 -NoBuild`.
- **Routing only.** In the panel, set `routing.strategy` back to `round-robin`. It hot-reloads.
- **Panel.** Run `gh release edit <previous-tag> --repo Devyre/Cli-Proxy-API-Management-Center --latest`, delete `%USERPROFILE%\.cli-proxy-api\static\management.html`, then run `docker restart cpa`.
- **Clients.** Run `Disable-CpaPool`. In T3, use the "Claude Direct" instance or remove the proxy variables from the Claude instance.

## Backup

Run `devyre\scripts\backup.ps1 -Destination <folder>`. It writes `config.yaml`, `auths\` and `secrets\` to a timestamped zip, and refuses destinations inside this checkout or the CPA home. The archive holds live OAuth tokens and keys: keep it on BitLocker-protected storage or encrypt it, for example with 7-Zip AES.

## Fork tests

```powershell
go test ./internal/api/ -run T3Hub
go test ./internal/managementasset/ -run DevyrePanelSource
go test ./internal/config/ -run DevyreDeployTemplate
```

## Safety

- Personal use only. Never share client keys, serve other people, or expose CPA publicly: Tailscale Funnel stays off and every port binds to 127.0.0.1.
- Keep session affinity on, so threads don't hop between accounts.
- Never commit secrets, auth files, `config.yaml`, `.env`, real emails or the tailnet hostname.

## Credits

- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (MIT): the upstream server. This fork keeps its license.
- [router-for-me/Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) (MIT): the upstream management panel.
- [chhoumann/Cli-Proxy-API-Management-Center](https://github.com/chhoumann/Cli-Proxy-API-Management-Center) (MIT): the quota ledger (PR #2 and PR #3) that the panel fork merges.
- [bandoyer/CLIProxyAPI](https://github.com/bandoyer/CLIProxyAPI) issues #2 to #18: the research and decision log behind expiring-first routing, quota signals, poll intervals and client wiring.
- [pingdotgg/t3code](https://github.com/pingdotgg/t3code): the CLIProxyAPI hub client that `TestT3Hub_*` pins.

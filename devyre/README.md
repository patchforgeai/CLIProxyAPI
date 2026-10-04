# Devyre CLIProxyAPI

Devyre's fork of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA). It pools several Claude subscriptions, plus optional Codex accounts, behind one endpoint on this PC. The endpoint is published only on the tailnet and serves a management panel with a quota ledger modeled on Theo's setup. T3 Code and Claude Code send their Claude traffic through it.

The design, the verified facts and the decisions behind this fork are in [PLAN.md](PLAN.md). Upstream's own documentation still applies to everything not listed here.

## What differs from upstream

- **Panel source.** The server downloads `management.html` from the releases of [Devyre/Cli-Proxy-API-Management-Center](https://github.com/Devyre/Cli-Proxy-API-Management-Center): upstream's panel plus chhoumann's quota ledger, a 7-day Claude headline with Fable hidden in the ledger, and a dark default theme. When the panel file is missing and the release lookup fails, the fallback also downloads from that fork's latest release. It never falls back to the stock panel.
- **T3 Code hub contract.** `TestT3Hub_*` pin the deprecated `/v0/management` endpoints that T3 Code's "CLIProxyAPI hub" decodes (`auth-files`, `api-call`, `reset-quota`), plus `/v8/management/requests/api-call`, which the panel uses. The auth-files listing also reports a Codex plan as `id_token.chatgpt_plan_type`, the key T3 reads, next to upstream's `id_token.plan_type`.
- **Quota readings and usage cache.** One in-memory store of quota readings, fed by response headers, by usage bodies passing through `api-call`, and by an idle-credential poller. A usage cache inside `APICall` keeps the panel, the T3 hub and the poller from tripping Claude's rate-limited usage endpoint. See [Routing and quota readings](#routing-and-quota-readings).
- **`expiring-first` routing.** A built-in strategy that prefers the credential whose quota would be lost soonest. Session affinity still keeps a thread on its credential. `GET /v8/management/routing/quota-readings` shows the ranking.
- **Deploy kit.** Docker Compose on loopback only, `tailscale serve` on port 8318, PowerShell scripts for secrets, start-up, backup and upstream syncs, and client snippets.

### Fork-only files

Every file the fork adds. Together with the hotspots below, this is exactly the list that `git diff --stat upstream/main...main` prints.

| Path | Purpose |
|---|---|
| `devyre/PLAN.md` | Implementation plan and decision log |
| `devyre/README.md` | This file |
| `devyre/clients/codex-config.toml` | Optional Codex CLI provider |
| `devyre/clients/powershell-profile.ps1` | Claude Code CLI wiring: `Enable-CpaPool`, `Disable-CpaPool`, `claude-direct`, `claudex` |
| `devyre/clients/t3-code.md` | Click-by-click T3 Code setup: hub, pooled Claude instance, Claude Direct |
| `devyre/deploy/.env.example` | Compose variables; copy it to the gitignored `.env` |
| `devyre/deploy/config.template.yaml` | Runtime config template (v8 layout) that `new-secrets.ps1` renders |
| `devyre/deploy/docker-compose.yml` | Builds `cpa-devyre:current`; publishes 8317, 54545 and 1455 on 127.0.0.1 only; mounts `CPA_HOME` at `/data` |
| `devyre/scripts/backup.ps1` | Archives `config.yaml`, `auths\` and `secrets\` |
| `devyre/scripts/new-secrets.ps1` | Creates the runtime folders and secrets, renders `config.yaml`, restarts the container on `-Rotate`, saves the public URL |
| `devyre/scripts/sync-upstream.ps1` | Merges `upstream/main` into a sync branch and runs the checks |
| `devyre/scripts/tailscale-serve.ps1` | Publishes CPA on the tailnet at `<machine>.<tailnet>.ts.net:8318`: HTTPS when Serve HTTPS is enabled, otherwise tailnet-only HTTP (force it with `-Http`) |
| `devyre/scripts/up.ps1` | Builds and tags the image, recreates the container, waits for `/healthz` and the panel |
| `internal/api/devyre_routing_readings_route_test.go` | The readings endpoint is on `/v8` only, behind management auth |
| `internal/api/devyre_t3_hub_contract_test.go` | `TestT3Hub_*`, the T3 Code hub contract |
| `internal/api/handlers/management/devyre_quota_observer.go` | Idle-credential usage poller; also forgets the readings of removed credentials |
| `internal/api/handlers/management/devyre_quota_observer_test.go` | Poller scheduling, min-gap, backoff and pruning tests |
| `internal/api/handlers/management/devyre_routing_config_test.go` | The routing blocks survive `/v8/management` config writes |
| `internal/api/handlers/management/devyre_routing_readings.go` | `GET /v8/management/routing/quota-readings` |
| `internal/api/handlers/management/devyre_routing_readings_test.go` | Readings endpoint golden JSON |
| `internal/api/handlers/management/devyre_routing_strategy_test.go` | `PUT /routing/strategy` accepts `expiring-first` and its aliases |
| `internal/api/handlers/management/devyre_usage_cache.go` | Usage cache inside `APICall` |
| `internal/api/handlers/management/devyre_usage_cache_test.go` | Usage cache tests: TTLs, single-flight, stale, backoff, bypass, query variants, writes |
| `internal/config/devyre_deploy_template_test.go` | Loads the deploy template through the real config loader and checks the typed routing values |
| `internal/config/devyre_routing.go` | `routing.expiring-first` and `routing.quota-observation` config |
| `internal/config/devyre_routing_test.go` | Routing config defaults, aliases and save round trips |
| `internal/managementasset/devyre_updater_test.go` | Pins the panel source to the fork |
| `internal/quotareading/claude.go` | Claude header and usage-body parsers |
| `internal/quotareading/claude_test.go` | Claude parser tests |
| `internal/quotareading/codex.go` | Codex header and usage-body parsers |
| `internal/quotareading/codex_test.go` | Codex parser tests |
| `internal/quotareading/contract_test.go` | Pins the package API the selector, cache and poller use |
| `internal/quotareading/evaluate.go` | Gating and urgency (`Evaluate`) |
| `internal/quotareading/evaluate_test.go` | Gating and ranking tests |
| `internal/quotareading/helpers_test.go` | Shared test helpers |
| `internal/quotareading/parse.go` | Shared parsing, `FromUsageBody` and `UsageSnapshot` |
| `internal/quotareading/parse_test.go` | Parsing tests |
| `internal/quotareading/reading.go` | Readings model: `Window`, `Kind`, `Reading` |
| `internal/quotareading/store.go` | Readings store: `Put`, `Replace`, `Effective` |
| `internal/quotareading/store_test.go` | Store tests |
| `sdk/cliproxy/auth/devyre_priority.go` | Exports the selectors' priority and availability rules for the readings endpoint |
| `sdk/cliproxy/auth/selector_expiring_first.go` | The `expiring-first` selector |
| `sdk/cliproxy/auth/selector_expiring_first_test.go` | Selector tests, including session affinity |
| `sdk/cliproxy/devyre_service_config_test.go` | Strategy aliases, settings and hot reload through the service config |

### Conflict hotspots

These are all the upstream files the fork edits. Each edit is a small hunk, marked with a `// devyre:` comment where the reason is not obvious. When a sync conflicts here, keep upstream's new code and re-apply our hunk.

| File | Our change |
|---|---|
| `config.example.yaml` | `management.panel-github-repository` is the fork; `routing:` documents `expiring-first` and the `expiring-first` and `quota-observation` blocks. |
| `internal/api/handlers/management/api_tools.go` | Three hunks hook the usage cache into `APICall`: a lookup after `auth_index` and `url` are parsed, complete or fail after the upstream call, and a record on success. `/v0` and `/v8` share this handler. |
| `internal/api/handlers/management/auth_files.go` | `extractCodexIDTokenClaims` also emits `chatgpt_plan_type`, the key T3 Code reads. |
| `internal/api/handlers/management/config_basic.go` | `normalizeRoutingStrategy` accepts `expiring-first`, so `PUT /routing/strategy` does too. |
| `internal/api/server.go` | Starts the quota observer once, right after the management handler is created. |
| `internal/api/server_management_v8.go` | One line registers `GET /v8/management/routing/quota-readings`. |
| `internal/config/config_defaults.go` | `DefaultPanelGitHubRepository` points at the Devyre panel fork. |
| `internal/config/config_types.go` | The `Strategy` comment, plus the `ExpiringFirst` and `QuotaObservation` fields on `RoutingConfig`. |
| `internal/config/config_yaml.go` | `isKnownDefaultValue` keeps explicit zero values of the pointer-backed routing settings (`devyreKeepsExplicitZero`), so a save does not drop them. |
| `internal/managementasset/updater.go` | `defaultManagementReleaseURL` and `defaultManagementFallbackURL` point at the Devyre panel releases. Never restore `cpamc.router-for.me`. |
| `sdk/cliproxy/service_config.go` | `expiring-first` aliases in `normalizedRoutingRuntimeState`, construction in `newRoutingSelector`, and the gate and log settings in `routingRuntimeState`. That struct must stay comparable: no pointers, maps or slices. |

If upstream deletes the `/v0/management` routes, `TestT3Hub_*` fails first. Restore the three routes T3 uses (`GET auth-files`, `POST api-call`, `POST reset-quota`) in `internal/api/devyre_v0_shim.go`.

## Routing and quota readings

With `routing.strategy: expiring-first` (the deploy template's setting), CPA picks the credential whose quota would be lost soonest:

- **Urgency** is the remaining percent of a credential's longest window, Claude's 7-day limit, divided by the hours until that window resets. The highest urgency is picked; equal urgencies take turns.
- **Gates.** A credential is skipped while a window that applies to the request has at most `gate-remaining-percent` (2) left and has not reset: the 5-hour window, the 7-day window, or a window scoped to the requested model family. When every credential is gated, the one whose gates reset first is picked, and upstream 429s and cooldowns take over.
- **Unknown credentials rank last.** A credential without readings is used only when no credential with readings is usable, taking turns with the other unknowns. It gets readings from its own traffic (response headers), from the panel or the T3 hub reading its usage, or from the idle poller (on by default with `expiring-first`). The poller checks every minute. It polls one Claude credential at a time, at least 10 minutes after any Claude usage call, so three new accounts all have readings about 20 minutes after a start; after that it re-polls a Claude credential once its weekly reading is 30 minutes old. Codex credentials are polled once their reading is 5 minutes old.
- **Session affinity.** A thread stays on the credential it started on, because the prompt cache is per account. It moves only when that credential becomes unavailable (cooldown, disabled) or the binding expires after an hour, never for urgency. Expiring-first only places new threads and failed-over ones.
- **Priority tiers** keep their meaning: only the highest available tier is considered.

`GET /v8/management/routing/quota-readings` (management key required; not on `/v0`) shows what the selector sees: per credential, its effective windows with `remaining_percent`, `resets_at`, `observed_at` and `source` (`header`, `usage` or `poll`), whether it is `usable` and why not (`gate_reason`, such as `"5h exhausted"` or `"7d exhausted"`), its `urgency_per_hour`, and its `rank` within its provider (1 is picked next; 0 means gated, unavailable or in a lower tier). Model-scoped windows are ignored there, so the view matches a request for an unscoped model.

**Readings.** Response headers and usage bodies feed one in-memory store, and the newest observation of each window wins. A usage body lists every window the provider reports, so a window it no longer reports is dropped. The store is cleared on restart and refills as described above.

**Usage cache.** `api-call` GETs of `https://api.anthropic.com/api/oauth/usage`, `https://api.anthropic.com/api/oauth/profile` and `https://chatgpt.com/backend-api/wham/usage` are cached per credential: Claude usage for 5 minutes, Codex usage for 60 seconds, the Claude profile for an hour. Concurrent requests share one upstream call, an upstream failure is answered with the last good body, and a Claude 429 backs that credential off for 5 minutes, doubling up to an hour. Every response the cache handles carries an `X-CPA-Usage-Cache` entry in the returned `header` map:

| Value | Meaning |
|---|---|
| `hit` | A fresh cached body; upstream was not called. |
| `miss` | Upstream was called because nothing fresh was cached. |
| `bypass` | Upstream was called because the request asked for a refresh. |
| `stale` | An expired cached body, served because upstream failed or the credential is backed off. |
| `backoff` | The stored Claude 429, served during a backoff when no body is cached. |

- Send the request header `X-CPA-Usage-Cache: refresh` to skip a fresh cached body. It is honored once the last upstream call for that URL is at least 30 seconds old; inside that floor the cache answers.
- The Claude usage URL with a query string, such as the panel's reset-grant check `?cedar_ember=1&skip_spend=1`, is cached under its full URL for 5 minutes, for up to 4 distinct query strings per credential. Further query strings are not cached: each request for one goes upstream (`miss`). Either way the call counts toward the poller's 10-minute Claude gap and shares the credential's 429 backoff, because Anthropic limits the endpoint per account whatever the query.
- A successful `POST`, `PUT`, `PATCH` or `DELETE` through `api-call` for a credential, such as a redeemed reset, makes that credential's cached responses stale, so the next read shows the new state. A Claude backoff still holds.

## How to run

This runs on this PC with Docker Desktop, and Tailscale fronts it. Prerequisites:

- Docker Desktop running, with "Start Docker Desktop when you sign in" enabled.
- Tailscale logged in, with MagicDNS and HTTPS certificates enabled for the tailnet (admin console, DNS page).
- A published panel release on the Devyre panel fork that includes `management.html`.

Steps, from the repository root in PowerShell:

1. **Secrets and config.** Run `devyre\scripts\new-secrets.ps1`. It creates `%USERPROFILE%\.cli-proxy-api\{auths,logs,static,plugins,secrets}`, restricts `secrets\` to your user, generates the management key and one key per client (`t3-code`, `claude-code-cli`, `codex-cli`, `other-devices`), and renders `config.yaml`. It prints the management key once: save it in your password manager. Client keys stay in `secrets\client-*.txt` and are never printed.
2. **Compose variables.** Copy `devyre\deploy\.env.example` to `devyre\deploy\.env`, then set `CPA_HOME`, `CPA_TZ` and, after step 4, `CPA_PUBLIC_URL`.
3. **Build and start.** Run `devyre\scripts\up.ps1`. It builds `cpa-devyre:current` and tags the same image `cpa-devyre:<git sha>` for rollback (`<git sha>-dirty` when tracked files have local changes). Then it recreates the container with `docker compose up -d --force-recreate` and waits for `http://127.0.0.1:8317/healthz` and `/management.html`.
4. **Publish on the tailnet.** Run `devyre\scripts\tailscale-serve.ps1`.
   - When Tailscale Serve HTTPS is enabled for the tailnet, it serves `https://<machine>.<tailnet>.ts.net:8318`.
   - Otherwise it prints the one-time enable link and falls back to `http://<machine>.<tailnet>.ts.net:8318`. That URL is tailnet-only and WireGuard-encrypted between devices, so the panel opens from your phone either way.
   - The serve config persists across reboots.

   Other devices use that URL; on the CPA host, `http://127.0.0.1:8317` keeps working. To point this PC's PowerShell profile at the tailnet URL instead, run `devyre\scripts\new-secrets.ps1 -PublicUrl <url>`. It writes `%USERPROFILE%\.cli-proxy-api-client\public-url.txt`, outside the folder the container mounts, so nothing in the container can change where your clients send requests.
5. **Log in accounts.** Open the panel (`http://127.0.0.1:8317/management.html` on the host, or `<tailnet URL>/management.html` from another device) and log in with the management key. Under OAuth Login, add each Claude account, using a separate browser profile or private window per account.
6. **Wire the clients.**
   - T3 Code: follow `devyre\clients\t3-code.md`.
   - Claude Code CLI: append `devyre\clients\powershell-profile.ps1` to `$PROFILE`. In a new shell, once the accounts are logged in, run `Enable-CpaPool`.
   - Codex CLI (optional): see `devyre\clients\codex-config.toml`.

Day to day:

- After pulling changes, run `up.ps1`. To restart without building, run `up.ps1 -NoBuild`; it always recreates the container.
- **Change settings through the panel or the management API.** Editing `config.yaml` on the Windows host does not reach the running server: CPA hot-reloads on file events, and Docker Desktop does not deliver them for host-side writes to the mounted folder. After a host-side edit to `config.yaml` or `auths\`, restart the container right away with `up.ps1 -NoBuild` or `docker restart cpa`. Until then the server keeps its old settings, and some management writes (deprecated `/v0` writes, plugin installs and deletes) save its in-memory config over your edit.
- Logs are in the panel's Logs Viewer, or run `docker logs -f cpa`.
- To rotate secrets, run `new-secrets.ps1 -Rotate`. It keeps the previous `config.yaml` as `config.yaml.bak-<timestamp>` and restarts the running container, so the old keys stop working at once. Then update the password manager, the T3 hub key and T3's `ANTHROPIC_AUTH_TOKEN`.
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
go test ./internal/api/ -run 'T3Hub|QuotaReadings'
go test ./internal/managementasset/ -run DevyrePanelSource
go test ./internal/config/ -run 'DevyreDeployTemplate|ExpiringFirst'
go test ./internal/quotareading/
go test ./internal/api/handlers/management/ -run 'UsageCache|QuotaObserver|RoutingQuota|ExpiringFirst|RoutingStrategy'
go test ./sdk/cliproxy/auth/ -run ExpiringFirst
go test ./sdk/cliproxy/ -run ExpiringFirst
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

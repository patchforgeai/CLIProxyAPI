# Devyre CLIProxyAPI

Devyre's fork of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA). It pools several Claude subscriptions, plus optional Codex accounts, behind one endpoint on this PC. The endpoint is published only on the tailnet and serves a management panel with a quota ledger modeled on Theo's setup. T3 Code and Claude Code send their Claude traffic through it.

The design, the verified facts and the decisions behind this fork are in [PLAN.md](PLAN.md). Upstream's own documentation still applies to everything not listed here.

## What differs from upstream

- **Panel source.** The server downloads `management.html` from the releases of [Devyre/Cli-Proxy-API-Management-Center](https://github.com/Devyre/Cli-Proxy-API-Management-Center): upstream's panel plus chhoumann's quota ledger, a 7-day Claude headline with Fable hidden in the ledger, and a dark default theme. When the panel file is missing and the release lookup fails, the fallback also downloads from that fork's latest release. It never falls back to the stock panel.
- **T3 Code hub contract.** `TestT3Hub_*` pin the deprecated `/v0/management` endpoints that T3 Code's "CLIProxyAPI hub" decodes (`auth-files`, `api-call`, `reset-quota`), plus `/v8/management/requests/api-call`, which the panel uses. The auth-files listing also reports a Codex plan as `id_token.chatgpt_plan_type`, the key T3 reads, next to upstream's `id_token.plan_type`.
- **Quota readings and usage cache.** One in-memory store of quota readings, fed by response headers, by usage bodies passing through `api-call`, and by an idle-credential poller. A usage cache inside `APICall` keeps the panel, the T3 hub and the poller from tripping Claude's rate-limited usage endpoint. See [Routing and quota readings](#routing-and-quota-readings).
- **`expiring-first` routing.** A built-in strategy that prefers the credential whose quota would be lost soonest. Session affinity still keeps a thread on its credential. `GET /v8/management/routing/quota-readings` shows the ranking.
- **Deploy kit.** Docker Compose on loopback only, `tailscale serve` on port 8318, PowerShell scripts for secrets, start-up, backup and upstream syncs, and client snippets.
- **Tailnet-only and passwordless.** CPA is reachable only from this PC and the tailnet, and chosen tailnet devices plus this PC use it without a key (`management.tailnet-auth`). `exposure-check.ps1` verifies both. See [Tailnet-only and passwordless](#tailnet-only-and-passwordless).

### Fork-only files

Every file the fork adds. Together with the hotspots below, this is exactly the list that `git diff --stat upstream/main...main` prints.

| Path | Purpose |
|---|---|
| `devyre/PLAN.md` | Implementation plan and decision log |
| `devyre/README.md` | This file |
| `devyre/clients/codex-config.toml` | Optional Codex CLI provider |
| `devyre/clients/powershell-profile.ps1` | Claude Code CLI wiring: `Enable-CpaPool`, `Disable-CpaPool`, `claude-direct`, `claudex` |
| `devyre/clients/claude-pool.cmd` | `claude-pool`: one Claude Code run through the pool. Works in any shell and needs no PowerShell execution policy change |
| `devyre/.gitattributes` | Keeps `*.cmd` files in CRLF for cmd.exe |
| `devyre/clients/t3-code.md` | Click-by-click T3 Code setup: hub, pooled Claude instance, Claude Direct |
| `devyre/deploy/.env.example` | Compose variables; copy it to the gitignored `.env` |
| `devyre/deploy/config.template.yaml` | Runtime config template (v8 layout) that `new-secrets.ps1` renders; `management.tailnet-auth` is on with empty, fail-closed lists |
| `devyre/deploy/docker-compose.yml` | Builds `cpa-devyre:current`; publishes 8317, 54545 and 1455 on 127.0.0.1 only; mounts `CPA_HOME` at `/data` |
| `devyre/scripts/backup.ps1` | Archives `config.yaml`, `auths\` and `secrets\` |
| `devyre/scripts/cpa-common.ps1` | Helpers the scripts dot-source: native-command runner, tailscale and docker readers, HTTP and management API clients |
| `devyre/scripts/exposure-check.ps1` | PASS/FAIL report: loopback-only publishing, no LAN or Funnel exposure, the serve mapping and the passwordless policy; exits 1 on any FAIL |
| `devyre/scripts/new-secrets.ps1` | Creates the runtime folders and secrets, renders `config.yaml`, restarts the container on `-Rotate`, saves the public URL |
| `devyre/scripts/sync-upstream.ps1` | Merges `upstream/main` into a sync branch and runs the checks |
| `devyre/scripts/tailnet-trust.ps1` | Writes `management.tailnet-auth` (allowed logins, devices and hosts) from `tailscale status` into the live config |
| `devyre/scripts/tailscale-serve.ps1` | Publishes CPA on the tailnet at `<machine>.<tailnet>.ts.net:8318` (HTTPS when Serve HTTPS is enabled, otherwise tailnet-only HTTP), verifies `/healthz` and repairs a serve entry left on an old tailnet name |
| `devyre/scripts/up.ps1` | Refuses to publish beyond 127.0.0.1, builds and tags the image, recreates the container, waits for `/healthz` and the panel |
| `internal/api/devyre_routing_readings_route_test.go` | The readings endpoint is on `/v8` only, behind management auth |
| `internal/api/devyre_t3_hub_contract_test.go` | `TestT3Hub_*`, the T3 Code hub contract |
| `internal/api/devyre_tailnet_auth.go` | Keyless proxy API for trusted requests (`proxy-api`), the key-only `/v1/ws` wrapper and the anti-framing headers of the panel pages |
| `internal/api/devyre_tailnet_auth_test.go` | Full-stack `tailnet-auth` tests through the real server and config loader: session endpoint, token-bearing routes, CORS, proxy principal, `/v1/ws`, framing |
| `internal/api/handlers/management/devyre_quota_observer.go` | Idle-credential usage poller; also forgets the readings of removed credentials |
| `internal/api/handlers/management/devyre_quota_observer_test.go` | Poller scheduling, min-gap, backoff and pruning tests |
| `internal/api/handlers/management/devyre_routing_config_test.go` | The routing blocks survive `/v8/management` config writes |
| `internal/api/handlers/management/devyre_routing_readings.go` | `GET /v8/management/routing/quota-readings` |
| `internal/api/handlers/management/devyre_routing_readings_test.go` | Readings endpoint golden JSON |
| `internal/api/handlers/management/devyre_routing_strategy_test.go` | `PUT /routing/strategy` accepts `expiring-first` and its aliases |
| `internal/api/handlers/management/devyre_tailnet_auth.go` | Keyless management access (`devyreTailnetAuthorize`, which `Middleware()` asks first) and `GET /v8/management/auth/session` |
| `internal/api/handlers/management/devyre_tailnet_auth_test.go` | Middleware, ban, `allow-remote` and session tests, plus a `/v8` config write round trip |
| `internal/api/handlers/management/devyre_usage_cache.go` | Usage cache inside `APICall` |
| `internal/api/handlers/management/devyre_usage_cache_test.go` | Usage cache tests: TTLs, single-flight, stale, backoff, bypass, query variants, writes |
| `internal/config/devyre_deploy_template_test.go` | Loads the deploy template through the real config loader and checks the typed routing values; pins the `tailnet-auth` block and the keys `tailnet-trust.ps1` writes |
| `internal/config/devyre_routing.go` | `routing.expiring-first` and `routing.quota-observation` config |
| `internal/config/devyre_routing_test.go` | Routing config defaults, aliases and save round trips |
| `internal/config/devyre_tailnet_auth.go` | `management.tailnet-auth` settings (`TailnetAuthConfig`); every setting defaults to off |
| `internal/config/devyre_tailnet_auth_test.go` | `tailnet-auth` defaults, both layouts, v8 validation, save round trips, the `config.example.yaml` block and the typed deploy-template check |
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
| `internal/tailnetauth/decide.go` | The trust decision: `Decide`, a pure function of the policy and the request |
| `internal/tailnetauth/decide_test.go` | Table tests for every rule of the decision |
| `sdk/cliproxy/auth/devyre_priority.go` | Exports the selectors' priority and availability rules for the readings endpoint |
| `sdk/cliproxy/auth/selector_expiring_first.go` | The `expiring-first` selector |
| `sdk/cliproxy/auth/selector_expiring_first_test.go` | Selector tests, including session affinity |
| `sdk/cliproxy/devyre_service_config_test.go` | Strategy aliases, settings and hot reload through the service config |
| `test/devyre_scripts_test.go` | Runs the PowerShell scripts with `powershell.exe` against a fake `tailscale status` and a fake management API (skipped without Windows PowerShell): the `allowed-hosts` that `tailnet-trust.ps1` writes, and its device selection (new devices only by exact MagicDNS name, renamed automation hosts, confirmation, the automation guard) |

### Conflict hotspots

These are all the upstream files the fork edits. Each edit is a small hunk, marked with a `// devyre:` comment where the reason is not obvious. When a sync conflicts here, keep upstream's new code and re-apply our hunk.

| File | Our change |
|---|---|
| `config.example.yaml` | `management.panel-github-repository` is the fork; a commented `management.tailnet-auth` block with every setting off; `routing:` documents `expiring-first` and the `expiring-first` and `quota-observation` blocks. |
| `internal/api/handlers/management/api_tools.go` | Three hunks hook the usage cache into `APICall`: a lookup after `auth_index` and `url` are parsed, complete or fail after the upstream call, and a record on success. `/v0` and `/v8` share this handler. |
| `internal/api/handlers/management/auth_files.go` | `extractCodexIDTokenClaims` also emits `chatgpt_plan_type`, the key T3 Code reads. |
| `internal/api/handlers/management/config_basic.go` | `normalizeRoutingStrategy` accepts `expiring-first`, so `PUT /routing/strategy` does too. |
| `internal/api/handlers/management/handler.go` | Two hunks: `Middleware()` asks `devyreTailnetAuthorize` first and skips the key for a trusted request, and the missing-key branch of `AuthenticateManagementKey` no longer calls `fail()`, so only a wrong key counts toward the ban. |
| `internal/api/server.go` | Starts the quota observer once, right after the management handler is created. One line adds `devyreTailnetContext` to the engine, so the proxy API's keyless check reads the live config. |
| `internal/api/server_management.go` | `serveManagementControlPanel` calls `devyreDenyFraming`. |
| `internal/api/server_management_v8.go` | Two lines register `GET /v8/management/routing/quota-readings` and `GET /v8/management/auth/session`. |
| `internal/api/server_middleware.go` | Two hunks: `accessAuthMiddleware` falls back to `devyreKeylessProxyAccess` after the API keys fail with a 401, and `serveExampleAPIKeyWarningPage` calls `devyreDenyFraming`. |
| `internal/api/server_routes.go` | `AttachWebsocketRoute` uses `devyreKeyOnlyAuthMiddleware`, so `/v1/ws` is never keyless. |
| `internal/config/config_defaults.go` | `DefaultPanelGitHubRepository` points at the Devyre panel fork. |
| `internal/config/config_types.go` | The `Strategy` comment, the `ExpiringFirst` and `QuotaObservation` fields on `RoutingConfig`, and `RemoteManagement.TailnetAuth` (`management.tailnet-auth`). |
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
5. **Passwordless access and the exposure check.** Run `devyre\scripts\tailnet-trust.ps1 -Include <this PC>,<phone>,<laptop> -ShowOnly` with the exact MagicDNS names, check the device table, then run it again without `-ShowOnly` and confirm the new devices. Then run `devyre\scripts\exposure-check.ps1`; it must end with 0 FAIL. Skip the first command to keep every request keyed. See [Tailnet-only and passwordless](#tailnet-only-and-passwordless).
6. **Log in accounts.** Open the panel (`http://127.0.0.1:8317/management.html` on the host, or `<tailnet URL>/management.html` from another device). On an allowed device it opens straight into the dashboard; elsewhere, log in with the management key. Under OAuth Login, add each Claude account, using a separate browser profile or private window per account.
7. **Wire the clients.**
   - T3 Code: follow `devyre\clients\t3-code.md`.
   - Claude Code CLI, either way:
     - **`claude-pool`.** Copy `devyre\clients\claude-pool.cmd` to a folder on PATH, for example `%USERPROFILE%\.local\bin`, next to `claude.exe`. Then `claude-pool` runs Claude Code through the pool and plain `claude` stays on your own login. It works in cmd, PowerShell and Git Bash with no execution-policy change.
     - **Profile functions.** Dot-source `devyre\clients\powershell-profile.ps1` from `$PROFILE`. In a new shell, once the accounts are logged in, run `Enable-CpaPool`, which makes plain `claude` pooled. Windows PowerShell only loads `$PROFILE` when the execution policy allows scripts; the default Restricted policy skips it.
   - Codex CLI (optional): see `devyre\clients\codex-config.toml`.

Day to day:

- After pulling changes, run `up.ps1`. To restart without building, run `up.ps1 -NoBuild`; it always recreates the container.
- **Change settings through the panel or the management API.** Editing `config.yaml` on the Windows host does not reach the running server: CPA hot-reloads on file events, and Docker Desktop does not deliver them for host-side writes to the mounted folder. After a host-side edit to `config.yaml` or `auths\`, restart the container right away with `up.ps1 -NoBuild` or `docker restart cpa`. Until then the server keeps its old settings, and some management writes (deprecated `/v0` writes, plugin installs and deletes) save its in-memory config over your edit.
- Logs are in the panel's Logs Viewer, or run `docker logs -f cpa`.
- To rotate secrets, run `new-secrets.ps1 -Rotate`. It keeps the previous `config.yaml` as `config.yaml.bak-<timestamp>` and restarts the running container, so the old keys stop working at once. Then update the password manager, the T3 hub key and T3's `ANTHROPIC_AUTH_TOKEN`.
- To bypass the pool, use `claude-direct` for one run, or `Disable-CpaPool`. In T3, use the "Claude Direct" instance.
- After a tailnet rename, run `tailscale-serve.ps1`, then `tailnet-trust.ps1` with no arguments, then `exposure-check.ps1`.

## Tailnet-only and passwordless

CPA is reachable only from this PC and from your tailnet, never from the LAN or the internet:

- The container publishes 8317, 54545 and 1455 on 127.0.0.1 only. `up.ps1` refuses to start a compose file that publishes a port anywhere else, and stops a running container that does.
- `tailscale serve` publishes `http(s)://<machine>.<tailnet>.ts.net:8318` to the tailnet only. Funnel stays off.

On top of that, `management.tailnet-auth` lets chosen tailnet devices, and this PC, use CPA without a key. The panel opens straight into the dashboard, the management API needs no key, and with `proxy-api: true` the proxy API needs no API key either. Everything else still needs the key, exactly as before.

### Set it up

1. Preview: `devyre\scripts\tailnet-trust.ps1 -Include <this PC>,<phone>,<laptop> -ShowOnly`. Names are MagicDNS names: the machine names in the admin console and in `tailscale status`, which the tailnet keeps unique, matched case-insensitively. Host names are never matched, because each node reports its own and several nodes can share one (iPhones all report `localhost`). A device that is not allowed yet must be named exactly. A wildcard such as `iphone*` or `*` only keeps devices that are already allowed, plus this PC, so on a first run `-Include *` selects only this PC. The table lists every device with its decision and the reason. Each new device is listed with its node ID, OS and registration date: a phone you just set up shows a recent date and the OS you expect.
2. Write: the same command without `-ShowOnly`. When it adds new devices, the script asks first; `-Force` skips the question once you have checked the preview, and a session that cannot ask writes nothing. The script writes only `management.tailnet-auth`, with `PUT /v8/management/config/management/tailnet-auth`, then re-reads the management section and fails unless nothing else changed. The running server applies it at once. A server built before this feature has no `/v8/management/auth/session` and would refuse the block, so the script writes nothing there and asks you to run `up.ps1` first.
3. Check: `devyre\scripts\exposure-check.ps1`. It must end with 0 FAIL.

The block: `allowed-logins` holds the login that owns this PC; `allowed-devices` holds every Tailscale IP, IPv4 and IPv6, of the selected devices; `allowed-hosts` holds this PC's MagicDNS FQDN and short name, each with tailscale serve's port (`<machine>.<tailnet>.ts.net:8318` and `<machine>:8318`), plus `localhost` and `127.0.0.1`. The server trusts a tailnet name only together with that port. An `allowed-hosts` written before this rule had bare names, which grant no tailnet trust any more: after updating the server, run `tailnet-trust.ps1` with no arguments once (`exposure-check.ps1` reports the bare names). When the block is new, `enabled`, `allow-local` and `proxy-api` start as `true`, like the deploy template; afterwards the script keeps their live values. Each list fails closed: an empty list means no keyless access through it.

- **Add a device:** re-run with `-Include` naming every device to keep and the new one by its exact MagicDNS name (the selection replaces the list), `-ShowOnly` first. Check the new device's node ID, OS and registration date, and the `allowed-devices +` lines under "Changes", before you confirm. New devices of your login need the key until you do.
- **Remove a device:** re-run without it in `-Include`, or add `-Exclude <name>`.
- **After a tailnet rename:** run `tailscale-serve.ps1`, then `tailnet-trust.ps1` with no arguments. Without `-Include` it keeps the devices that are already allowed and refreshes the names: the FQDN in `allowed-hosts` goes stale on a rename while the short name keeps working.
- **Turn it off:** `tailnet-trust.ps1 -Disable`. Every request needs a key again at once; `-Enable` turns it back on. `allow-local` and `proxy-api` are in the panel's config editor under `management.tailnet-auth`, or set one from this PC, for example: `Invoke-RestMethod -Method Put -Uri http://127.0.0.1:8317/v8/management/config/management/tailnet-auth/allow-local -Headers @{ 'X-CPA-Keyless' = '1' } -ContentType application/json -Body 'false'` (that request itself relies on `allow-local`; send `Authorization = "Bearer <management key>"` instead when it is off).

Scripts and `curl` send `X-CPA-Keyless: 1`, or any `Authorization: Bearer` value, and no key: `curl.exe -H "X-CPA-Keyless: 1" http://127.0.0.1:8317/v8/management/auth/session` (a fork-only endpoint, not in upstream's API docs) answers `{"authenticated":true,"method":"local","login":"","device":""}`. Over the tailnet `method` is `"tailnet"`, `device` is the caller's tailnet IP and `login` its login (empty for a listed tagged node). Trust is checked before the key, so `"key"` appears only when a valid key authenticated an untrusted request; otherwise the answer is 401. The panel probes that endpoint at start-up. The header is a constant, not a secret.

### Who is trusted, and why

A request skips the key only when all of these hold:

- `tailnet-auth.enabled` is true, and the container's direct TCP peer is loopback or inside `server.trusted-proxies` (the Docker gateway).
- It is not a Funnel request.
- Its `Host` is in `allowed-hosts`, and an `X-Forwarded-Host` equals it. An entry with a port matches only that port.
- It passes the browser guard. A present `Origin` must be this same origin, and a present `Sec-Fetch-Site` must be `same-origin` or `none`. Without `Origin`, the request must carry a non-browser signal: `X-CPA-Keyless: 1`, `Authorization: Bearer <non-empty token>`, or a non-empty `X-Management-Key`, `X-Api-Key` or `X-Goog-Api-Key`. Browsers never add these on their own; a page from another site can add one only in CORS mode, which always sends its `Origin`, and the guard rejects that `Origin`. Query keys, Basic and other schemes do not count.
- Then exactly one path applies, chosen by the `Host`:
  - **Tailnet** (this PC's MagicDNS name, through tailscale serve): the `Host` names tailscale serve's port and matches an `allowed-hosts` entry with that same port (`<machine>.<tailnet>.ts.net:8318`); a bare name, a `Host` without a port and `server.port` (8317) never count. `X-Forwarded-For` is exactly one tailnet IP that is listed in `allowed-devices`, and the `Tailscale-User-Login` that serve set is in `allowed-logins`. A listed tagged node, which serve stamps with no identity, needs only its IP, and only while `allowed-logins` is not empty. tailscale serve overwrites `X-Forwarded-For` and the `Tailscale-User-*` headers, so tailnet devices cannot forge them, and it keeps the `Host` the browser sent, which always names port 8318.
  - **Local** (a loopback `Host` such as `localhost` or `127.0.0.1`, with `allow-local: true`): a direct request on this PC. Proxy headers (`X-Forwarded-For`, `X-Real-IP`, `Forwarded`, `X-Forwarded-Host`, `Tailscale-*`) never arrive through serve on a loopback name, so with one the request is untrusted.
- None of the headers the decision reads (`X-Forwarded-For`, `X-Forwarded-Host`, `Origin`, `Sec-Fetch-Site`, `X-CPA-Keyless`, `Tailscale-User-Login`, `Tailscale-User-Name`) appears twice.
- For the management API, trust never opens more than the key would: a management key must be configured, and `management.allow-remote` must be true unless the client is loopback. Inside the container no client is loopback, this PC and tailscale serve's hop included, so the deploy template keeps `allow-remote: true`.

Trusted management responses carry no `Access-Control-*` headers and send `Cache-Control: no-store`, so another origin can never read them. `/management.html` and the safe-mode page at `/` refuse framing (`Content-Security-Policy: frame-ancestors 'none'`, `X-Frame-Options: DENY`).

**Keep your own automation out: tag it.** Every node on this tailnet, the CI runner and the bot VM included, is untagged and owned by the same login, so tailscale serve stamps them all with the allowed login. Only `allowed-devices` keeps them out, and an allowed CI runner or bot could read the Claude OAuth tokens through the management API. The barrier that holds is tagging those hosts in the tailnet policy (for example `tag:ci` and `tag:bot`): tagged nodes are never candidates and get no identity headers. Until they are tagged, keep in mind:

- The device list is made of Tailscale IPs, which the control plane assigns, so a device that is already allowed stays put whatever it calls itself.
- Names are a weaker signal. A node reports its own host name and OS, and its MagicDNS name follows the host name unless you pin the machine name in the admin console. A compromised automation host can therefore rename itself to look like a phone. This is why `tailnet-trust.ps1` adds a new device only when you name it exactly, never through a wildcard, and why it shows the node ID and registration date, which the node cannot change.
- The script also leaves out any device whose MagicDNS name or host name holds the token `ci`, `runner`, `bot`, `build` or `agent`, unless `-AllowAutomationName` gives its exact MagicDNS name. That guard only catches mistakes, not a machine that picked its name to slip past it.

**The trust boundary is this PC's loopback.** Inside the container, tailscale serve's hop, browsers and programs on this PC, and other containers reaching the host through `host.docker.internal` all arrive from the same Docker gateway, so the server cannot tell them apart. Software that can reach `127.0.0.1:8317` can also send forged `X-Forwarded-For` and `Tailscale-*` headers with any `Host`, the tailnet name on port 8318 included. A web page cannot: browsers never let a page set `Host`, so a page that gets the tailnet name resolved to 127.0.0.1 (DNS rebinding) reaches the loopback publish with `Host: <machine>...:8317`, which the tailnet path refuses. Nothing else may listen on port 8318 on loopback; `exposure-check.ps1` fails if something does, and it sends that rebinding request itself and expects a 401. So:

- The trusted set is the allowed tailnet devices plus anything that can reach this PC's loopback, containers included. `exposure-check.ps1` warns when a throwaway container is trusted as local.
- `allow-local: false` is no barrier against software on this PC, which can forge the tailnet headers.
- Processes running as your user can read `secrets\` and `auths\` anyway.
- Passwordless access suits only a single-user PC whose containers you trust.
- It depends on loopback-only publishing: published on the LAN, any device there could forge the headers. `up.ps1` refuses that and `exposure-check.ps1` checks it. `server.trusted-proxies` must also contain the Docker gateway. Passwordless access reads that list live, but bans and logs read it only at start, so restart the container after changing it.

### What still needs the key

- Every device not in `allowed-devices`: the CI runner, the bot VM, devices of other users, tagged nodes you did not list, and new devices.
- Funnel requests, cross-site browser requests, and requests without `Origin` that carry no keyless signal.
- RESP on port 8317 and `/keep-alive` take only the key or the local password. `/v1/ws`, the AI Studio relay socket, is never keyless.
- Unchanged and unauthenticated, as before: the OAuth callbacks (state-matched), `/v0/resource/plugins/*`, `/healthz`, `/` and `/management.html`.

A request without a key no longer counts toward the ban; only a wrong non-empty key does, and five of them ban the client IP for 30 minutes. Trusted requests never touch the counter, and a wrong key on a trusted request is ignored, so T3 Code can keep any key.

**Proxy API (`proxy-api: true`).** A valid API key always wins, so configured clients keep their own usage rows and isolation. A trusted request without a valid API key is attributed to `tailnet:<login>@<device IP>` (the login is empty for a listed tagged node) or to `local`; like every keyless request it needs a non-browser signal, and any non-empty `Authorization: Bearer` or `X-Api-Key` value counts. That principal also seeds caller-scope session isolation, the Claude MCP alias secret, the Codex prompt-cache key and the xAI reasoning-replay namespace, which is why it is per device. It need not be secret: the server derives it from verified identity, and only software inside the trust boundary could forge it.

### Residual risks

- **Plain HTTP.** Over plain HTTP, only DNS authenticates the tailnet URL. On a device whose tailnet names are answered by someone else's DNS, a hostile network can serve its own page under `http://<machine>.<tailnet>.ts.net:8318`. Two cases:
  - Tailscale is off or not answering DNS on this PC, and the name is resolved to 127.0.0.1. The page then reaches only the loopback publish on 8317, where the tailnet path never applies because it is bound to port 8318. That holds over plain HTTP and HTTPS alike.
  - A device is connected to the tailnet but uses another DNS server, with "Use Tailscale DNS" off. The page can then be rebound to this PC's tailnet IP, so it talks to CPA through tailscale serve as that device, with its keyless rights. Keep Tailscale DNS on for every allowed device. Never open the tailnet URL while Tailscale is off, and if you did on an untrusted network, clear that site's data on the device.

  Enabling Serve HTTPS, with the one-time admin link that `tailscale-serve.ps1` prints, removes the second case, because a hostile page cannot present the certificate. It also turns on the browser `Sec-Fetch` guard, which the plain-HTTP URL never triggers.
- **Same-origin content.** Anything served from the CPA origin, the panel and the plugin resource pages, acts with keyless admin rights. Installing a plugin or publishing a release on the panel fork is therefore equivalent to admin access. `management.disable-auto-update-panel: true` pins the current panel.

### What exposure-check.ps1 verifies

Exposure: every port of the cpa container is published on 127.0.0.1; nothing listens on `0.0.0.0` or a LAN address for 8317, 54545 or 1455, and 8318 listens on Tailscale addresses only, not even on loopback; this PC's LAN addresses cannot reach 8317 or 8318; Funnel is off; tailscale serve has exactly one web handler on port 8318, `/` to `http://127.0.0.1:8317`, keyed to the current MagicDNS name, and no raw TCP forward reaches CPA; both health URLs answer 200.

Passwordless: the Docker gateway is inside `server.trusted-proxies`; `allowed-hosts` holds the current FQDN with port 8318 (a bare FQDN is reported as written by an older `tailnet-trust.ps1`); over the tailnet the keyless session probe is 200 (SKIP when this PC is not in `allowed-devices`), the same request without a signal is 401, and with `Origin: http://evil.example` it is 401 or 403; direct `http://127.0.0.1:8317` follows `allow-local`, and forged tailnet headers or a foreign `Origin` there are refused; so is a direct request with the tailnet name as `Host` (on 8317 or without a port), a listed `X-Forwarded-For` and `X-CPA-Keyless`, which is what a DNS-rebinding page would send; a throwaway container trusted as local is a WARN. A failed keyless probe lists its preconditions, `management.allow-remote` included. It reads the live config with the management key, sends no wrong key, and changes nothing.

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
- **Passwordless access only.** Run `devyre\scripts\tailnet-trust.ps1 -Disable`. Every request needs a key again at once; clients that send a real key keep working.

## Backup

Run `devyre\scripts\backup.ps1 -Destination <folder>`. It writes `config.yaml`, `auths\` and `secrets\` to a timestamped zip, and refuses destinations inside this checkout or the CPA home. The archive holds live OAuth tokens and keys: keep it on BitLocker-protected storage or encrypt it, for example with 7-Zip AES.

## Fork tests

```powershell
go test ./internal/api/ -run 'T3Hub|QuotaReadings|TailnetAuth|DevyreKeylessProxy'
go test ./internal/managementasset/ -run DevyrePanelSource
go test ./internal/config/ -run 'DevyreDeployTemplate|ExpiringFirst|TailnetAuth'
go test ./internal/quotareading/
go test ./internal/tailnetauth/
go test ./internal/api/handlers/management/ -run 'UsageCache|QuotaObserver|RoutingQuota|ExpiringFirst|RoutingStrategy|TailnetAuth|GetAuthSession|AuthenticateManagementKey'
go test ./sdk/cliproxy/auth/ -run ExpiringFirst
go test ./sdk/cliproxy/ -run ExpiringFirst
go test ./test/ -run Devyre
```

The scripts must stay ASCII and parse in Windows PowerShell 5.1; this prints nothing when they do:

```powershell
Get-ChildItem devyre -Recurse -Filter *.ps1 | ForEach-Object { $e = $null; [void][System.Management.Automation.Language.Parser]::ParseFile($_.FullName, [ref]$null, [ref]$e); if ($e) { "$($_.Name): $($e[0].Message)" } }
```

## Safety

- Personal use only. Never share client keys, serve other people, or expose CPA publicly: Tailscale Funnel stays off and every port binds to 127.0.0.1. `exposure-check.ps1` verifies both.
- Allow only your personal devices in `management.tailnet-auth`, never a CI runner, bot or shared machine, and tag the automation hosts so they are never candidates.
- Keep session affinity on, so threads don't hop between accounts.
- Never commit secrets, auth files, `config.yaml`, `.env`, real emails, tailnet IPs, logins or the tailnet hostname.

## Credits

- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (MIT): the upstream server. This fork keeps its license.
- [router-for-me/Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) (MIT): the upstream management panel.
- [chhoumann/Cli-Proxy-API-Management-Center](https://github.com/chhoumann/Cli-Proxy-API-Management-Center) (MIT): the quota ledger (PR #2 and PR #3) that the panel fork merges.
- [bandoyer/CLIProxyAPI](https://github.com/bandoyer/CLIProxyAPI) issues #2 to #18: the research and decision log behind expiring-first routing, quota signals, poll intervals and client wiring.
- [pingdotgg/t3code](https://github.com/pingdotgg/t3code): the CLIProxyAPI hub client that `TestT3Hub_*` pins.

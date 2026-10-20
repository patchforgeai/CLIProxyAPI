# Devyre CLIProxyAPI

Devyre's fork of [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA). It pools several Claude subscriptions, plus optional Codex accounts, behind one endpoint on this PC. The endpoint is published only on the tailnet and serves a management panel with a quota ledger modeled on Theo's setup. T3 Code and Claude Code send their Claude traffic through it.

The design, the verified facts and the decisions behind this fork are in [PLAN.md](PLAN.md). Upstream's own documentation still applies to everything not listed here.

## What differs from upstream

- **Embedded panel.** The panel is built into the binary and never downloaded or updated: a release of [the panel fork](https://github.com/Devyre/Cli-Proxy-API-Management-Center), which is upstream's panel plus chhoumann's quota ledger, a 7-day Claude headline with Fable hidden in the ledger, and a dark default theme. `update-panel.ps1` refreshes it, and `-tags devyre_nopanel` builds a server without it. It never serves the stock panel. See [Embedded panel](#embedded-panel).
- **T3 Code hub contract.** `TestT3Hub_*` pin the deprecated `/v0/management` endpoints that T3 Code's "CLIProxyAPI hub" decodes (`auth-files`, `api-call`, `reset-quota`), plus `/v8/management/requests/api-call`, which the panel uses. The auth-files listing also reports a Codex plan as `id_token.chatgpt_plan_type`, the key T3 reads, next to upstream's `id_token.plan_type`.
- **Quota readings and usage cache.** One in-memory store of quota readings, fed by response headers, by usage bodies passing through `api-call`, and by an idle-credential poller. A usage cache inside `APICall` keeps the panel, the T3 hub and the poller from tripping Claude's rate-limited usage endpoint. See [Routing and quota readings](#routing-and-quota-readings).
- **`expiring-first` routing.** A built-in strategy that places a new session on the credential whose quota would be lost soonest, among the credentials with headroom for it. Session affinity still keeps a thread on its credential. `GET /v8/management/routing/quota-readings` shows the ranking.
- **Focus an account.** `/v8/management/routing/focus` focuses one credential per provider, for a while or until removed. Under `expiring-first` it then takes every request it can serve, threads already bound to another credential included, so its weekly quota is burned before it resets. The focus survives restarts. See [Focus](#focus).
- **Deploy kit.** Docker Compose on loopback only, `tailscale serve` on port 8318, PowerShell scripts for secrets, start-up, backup and upstream syncs, and client snippets.
- **Tailnet-only and passwordless.** CPA is reachable only from this PC and the tailnet, and chosen tailnet devices plus this PC use it without a key (`management.tailnet-auth`). `exposure-check.ps1` verifies both. See [Tailnet-only and passwordless](#tailnet-only-and-passwordless).
- **Sidecar lifeline.** `-lifeline-stdin` lets a supervising process own the server's lifetime through stdin, in place of the 10-second keep-alive idle limit that upstream applies with `-password`; `-password-stdin` hands it the local password there, off the command line. The server then reports its listener on stdout, and `/keep-alive` proves it holds the password without anyone sending it. Off by default. See [Sidecar lifeline](#sidecar-lifeline).
- **OAuth redirect port.** The Claude and Codex OAuth starts take an opt-in `redirect_port`, used for the authorize URL and the token exchange, so a caller with its own callback listener on an OS-assigned port never collides with whatever holds 54545 or 1455. Without it nothing changes. See [OAuth redirect port](#oauth-redirect-port).
- **Codex `response.interrupt`.** The router writes Codex's interrupt, unchanged, to the upstream socket its response runs on and never answers one with an error, and a Responses websocket client that leaves mid-response ends its upstream response at once, with or without steering. See [Codex response.interrupt](#codex-responseinterrupt).

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
| `devyre/deploy/docker-compose.yml` | Builds `cpa-devyre:current`; publishes 8317 on 127.0.0.1 only; mounts `CPA_HOME` at `/data` |
| `devyre/deploy/docker-compose.oauth.yml` | Overlay with the OAuth callback ports 54545 and 1455 (127.0.0.1); the scripts drop it when Windows has reserved them |
| `devyre/scripts/backup.ps1` | Archives `config.yaml`, `auths\` and `secrets\` |
| `devyre/scripts/cpa-common.ps1` | Helpers the scripts dot-source: native-command runner, tailscale and docker readers, HTTP and management API clients |
| `devyre/scripts/exposure-check.ps1` | PASS/FAIL report: loopback-only publishing, no LAN or Funnel exposure, the serve mapping and the passwordless policy; exits 1 on any FAIL |
| `devyre/scripts/new-secrets.ps1` | Creates the runtime folders and secrets, renders `config.yaml`, keeps `management.tailnet-auth` and restarts the container on `-Rotate`, saves the public URL |
| `devyre/scripts/sync-upstream.ps1` | Merges `upstream/main` into a sync branch and runs the checks |
| `devyre/scripts/tailnet-trust.ps1` | Writes `management.tailnet-auth` (allowed logins, devices and hosts) from `tailscale status` into the live config |
| `devyre/scripts/tailscale-serve.ps1` | Publishes CPA on the tailnet at `<machine>.<tailnet>.ts.net:8318` (HTTPS when Serve HTTPS is enabled, otherwise tailnet-only HTTP), verifies `/healthz` and repairs a serve entry left on an old tailnet name |
| `devyre/scripts/ensure-running.ps1` | Starts whatever is down: Docker Desktop, the `cpa` container, the tailnet serve entry |
| `devyre/scripts/install-autostart.ps1` | Registers the "Devyre CPA" scheduled task: `ensure-running.ps1` at sign-in and every 5 minutes |
| `devyre/start-cpa.cmd` | Double-click starter for `ensure-running.ps1` |
| `devyre/scripts/up.ps1` | Refuses to publish beyond 127.0.0.1, builds and tags the image, recreates the container, waits for `/healthz` and the panel |
| `devyre/scripts/update-panel.ps1` | Refreshes the embedded panel: a panel fork release through your own `gh` login, kept only when it matches the sha256 digest the release lists, or a clean local build of a clone; writes `management.html` and `panel.json` and never commits |
| `cmd/server/devyre_lifeline.go` | Registers `-lifeline-stdin` and `-password-stdin`, and reads the password line for `main` |
| `cmd/server/devyre_lifeline_test.go` | Both flags are booleans listed in the help output, argv scanning never takes the next argument as a value, and `-password-stdin` never goes with `-password` |
| `internal/api/devyre_lifeline.go` | `WithDevyreKeepAliveProbe`: `/keep-alive`, authenticated by the local password, without the idle watcher, answering challenges with `LifelineProof`; `WithDevyreListenReport`: `Start` reports its listener; `devyreServerState`, the fork's own server fields |
| `internal/api/devyre_lifeline_test.go` | The probe answers only the local password, starts no watcher, and adds nothing next to `WithKeepAliveEndpoint`; the proof vector; challenges answered on loopback only, never to a browser, bound to the address the request reached (through the real listener too); the listen report once, before the first connection, or with the error on a taken port |
| `internal/api/devyre_management_ban_test.go` | Over RESP, commands before `AUTH` never count toward the ban and five wrong passwords do |
| `internal/api/devyre_panel.go` | `devyreServePanel`: `/management.html` from the panel built into the binary, with its sha256 as the `ETag`, or from the file `MANAGEMENT_STATIC_PATH` names; 404 in a `devyre_nopanel` build; never downloads |
| `internal/api/devyre_panel_test.go` | Through the real server, with every request that leaves the machine refused and recorded: the embedded bytes, headers and a 304 revalidation with no request made and no file written (404 under `devyre_nopanel`), the `MANAGEMENT_STATIC_PATH` override, and 404 with `disable-control-panel` |
| `internal/api/devyre_routing_focus.go` | `devyreLoadRoutingFocus`: loads the focus file from the auth dir at start-up, never failing it; `devyreRegisterRoutingFocus`: the `/v8` routes |
| `internal/api/devyre_routing_focus_test.go` | The start-up load: an ended focus dropped, writes going to the same file, an invalid file starting empty |
| `internal/api/devyre_routing_focus_route_test.go` | The focus endpoints are on `/v8` only, behind management auth, and the readings report the focus |
| `internal/api/devyre_routing_readings_route_test.go` | The readings endpoint is on `/v8` only, behind management auth |
| `internal/api/devyre_t3_hub_contract_test.go` | `TestT3Hub_*`, the T3 Code hub contract |
| `internal/api/devyre_tailnet_auth.go` | Keyless proxy API for trusted requests (`proxy-api`), the key-only `/v1/ws` wrapper and the anti-framing headers of the panel pages |
| `internal/api/devyre_tailnet_auth_test.go` | Full-stack `tailnet-auth` tests through the real server and config loader: session endpoint, token-bearing routes, CORS, proxy principal, `/v1/ws`, framing, the tailnet name on the API port, the rendered proxy debug lines |
| `internal/api/handlers/management/devyre_oauth_redirect.go` | `redirect_port` on the Claude and Codex OAuth starts: parsing, the refusals, and the redirect-aware authorize URL and exchange |
| `internal/api/handlers/management/devyre_oauth_redirect_test.go` | Through `StartOAuthV8`: the default redirects unchanged, the moved ones per provider, every refused value and the `is_webui` combination, a Codex exchange that uses the moved redirect, and a service that cannot move it |
| `internal/api/handlers/management/devyre_quota_observer.go` | Idle-credential usage poller; also forgets the readings and the focus of removed credentials |
| `internal/api/handlers/management/devyre_quota_observer_test.go` | Poller scheduling, min-gap, backoff and pruning tests |
| `internal/api/handlers/management/devyre_routing_config_test.go` | The routing blocks survive `/v8/management` config writes |
| `internal/api/handlers/management/devyre_routing_focus.go` | `GET`, `PUT` and `DELETE /v8/management/routing/focus`; `SetRoutingFocusStore`; `forgetRemovedFocus` for the observer |
| `internal/api/handlers/management/devyre_routing_focus_test.go` | Focus endpoints: lookups by ID and file name, replacing per provider, deletes by name and provider, every refusal, only existing credentials listed, the file written, and the observer forgetting removed credentials' focus |
| `internal/api/handlers/management/devyre_routing_readings.go` | `GET /v8/management/routing/quota-readings`, the focus included |
| `internal/api/handlers/management/devyre_routing_readings_test.go` | Readings endpoint golden JSON; a focused credential's rank and fields |
| `internal/api/handlers/management/devyre_routing_strategy_test.go` | `PUT /routing/strategy` accepts `expiring-first` and its aliases |
| `internal/api/handlers/management/devyre_tailnet_auth.go` | Keyless management access (`devyreTailnetAuthorize`, which `Middleware()` asks first) and `GET /v8/management/auth/session` |
| `internal/api/handlers/management/devyre_tailnet_auth_test.go` | Middleware, ban, `allow-remote` and session tests, a `/v8` config write round trip, and the debug decision lines as the log formatter renders them |
| `internal/api/handlers/management/devyre_usage_cache.go` | Usage cache inside `APICall` |
| `internal/api/handlers/management/devyre_usage_cache_test.go` | Usage cache tests: TTLs, single-flight, stale, backoff, bypass, query variants, writes |
| `internal/auth/claude/devyre_redirect.go` | `LoopbackRedirectURI` (`http://localhost:<port>/callback`), `GenerateAuthURLWithRedirect`, `ExchangeCodeForTokensWithRedirect` |
| `internal/auth/claude/devyre_redirect_test.go` | Only the redirect changes in the authorize URL; the exchange body carries the moved redirect |
| `internal/auth/codex/devyre_redirect.go` | `LoopbackRedirectURI` (`http://127.0.0.1:<port>/auth/callback`), `GenerateAuthURLWithRedirect` (upstream already has `ExchangeCodeForTokensWithRedirect`) |
| `internal/auth/codex/devyre_redirect_test.go` | Only the redirect changes in the authorize URL |
| `internal/cmd/devyre_lifeline.go` | The stdin lifeline: `SetStdinLifeline`, the shutdown line, the run context that ends with stdin, the password line (`ReadStdinPassword`) on the same buffered reader, and the listening line |
| `internal/cmd/devyre_lifeline_test.go` | Line parsing (EOF, `shutdown`, CRLF, unknown and over-long lines, read errors), the password line (CRLF, last line, empty, blank, over-long, a shutdown line buffered behind it), the listening lines, and `StartService` in process: the listening line, `/keep-alive` with the local password and with a challenge, then a graceful stop on a shutdown line and on EOF; a taken port reported and `StartService` returning |
| `internal/config/devyre_deploy_template_test.go` | Loads the deploy template through the real config loader and checks the typed routing values; pins the `tailnet-auth` block and the keys `tailnet-trust.ps1` writes |
| `internal/config/devyre_routing.go` | `routing.expiring-first` and `routing.quota-observation` config, `SessionAffinityPersistEnabled` |
| `internal/config/devyre_routing_test.go` | Routing config defaults, clamping, aliases and save round trips, `session-affinity-persist` included |
| `internal/config/devyre_tailnet_auth.go` | `management.tailnet-auth` settings (`TailnetAuthConfig`); every setting defaults to off |
| `internal/config/devyre_tailnet_auth_test.go` | `tailnet-auth` defaults, both layouts, v8 validation, save round trips, the `config.example.yaml` block and the typed deploy-template check |
| `internal/managementasset/devyre_panel.go` | `Panel()` (the embedded panel, its version and sha256), `PanelOverridePath()`, and the stand-ins that keep upstream's updater and downloader from running |
| `internal/managementasset/devyre_panel_embed.go` | Embeds `devyre_panel/management.html` and `panel.json` in the default build |
| `internal/managementasset/devyre_panel_nopanel.go` | The `devyre_nopanel` build: no panel bytes |
| `internal/managementasset/devyre_panel/.gitattributes` | Keeps `management.html` byte for byte (`binary`) and `panel.json` in LF on every checkout |
| `internal/managementasset/devyre_panel/management.html` | The embedded panel: the panel fork's release asset |
| `internal/managementasset/devyre_panel/panel.json` | Its version, source and sha256, written by `update-panel.ps1` |
| `internal/managementasset/devyre_panel_test.go` | The record matches the file, the embedded bytes are the file, `devyre_nopanel` embeds nothing (`go list`), and neither the updater nor the downloader makes a request, writes a file or starts a goroutine |
| `internal/managementasset/devyre_updater_test.go` | Pins upstream's dormant download source to the panel fork, never the stock panel |
| `internal/quotareading/claude.go` | Claude header and usage-body parsers |
| `internal/quotareading/claude_test.go` | Claude parser tests |
| `internal/quotareading/codex.go` | Codex header and usage-body parsers |
| `internal/quotareading/codex_test.go` | Codex parser tests |
| `internal/quotareading/contract_test.go` | Pins the package API the selector, cache and poller use |
| `internal/quotareading/evaluate.go` | Gating and urgency (`Evaluate`), headroom (`EvaluateHeadroom`) |
| `internal/quotareading/evaluate_test.go` | Gating, ranking and headroom tests |
| `internal/quotareading/helpers_test.go` | Shared test helpers |
| `internal/quotareading/parse.go` | Shared parsing, `FromUsageBody` and `UsageSnapshot` |
| `internal/quotareading/parse_test.go` | Parsing tests |
| `internal/quotareading/reading.go` | Readings model: `Window`, `Kind`, `Reading` |
| `internal/quotareading/store.go` | Readings store: `Put`, `Replace`, `Effective` |
| `internal/quotareading/store_test.go` | Store tests |
| `internal/routingfocus/store.go` | The focus store: one focus per provider, `Set`, `ClearProvider`, `ClearAuth`, `Retain`, `Focused`, `List`, the lock-free `Empty`, and the atomically written `routing-focus.devyre` file |
| `internal/routingfocus/store_test.go` | Replacing per provider, clearing, retaining, ends inert and pruned, the file's exact contents and mode, invalid files, memory only, failed writes, concurrent use |
| `internal/runtime/executor/devyre_codex_interrupt.go` | `devyreCodexResponses`: which response runs on a session's upstream socket, from its `response.created` to its terminal event as the session's upstream reader reads them; `DevyreInterruptResponse`, which writes a `response.interrupt` to that socket only while its response runs there; the duplex's `devyreDuplexInterrupt`; and `devyreInterruptedEnd`, an interrupted response's end without output |
| `internal/runtime/executor/devyre_codex_interrupt_test.go` | Against a scripted upstream: the frame forwarded byte for byte and the socket kept for the next request after a zero-output interrupted end; nothing written for a response that ended, an unknown one or a missing session, with the local answers; the disabled-credential refusal; only the router's own interrupt exempting an empty end, and the bounded history; and in the duplex, the interrupt from its input and from the reader path, the duplex settling and taking the next create |
| `internal/tailnetauth/decide.go` | The trust decision: `Decide`, a pure function of the policy and the request (the request carries the OS lookup of its peer's user, asked only when a local decision depends on it) |
| `internal/tailnetauth/decide_test.go` | Table tests for every rule of the decision, local trust bound to a direct peer of the server's own user |
| `internal/tailnetauth/peer.go` | `PeerRunsAsThisUser`: whether the process at the other end of a loopback connection runs as the server's OS user; the `/proc/net/tcp` parser |
| `internal/tailnetauth/peer_windows.go` | The Windows lookup: the TCP table's owning process (`GetExtendedTcpTable`), then its token's user |
| `internal/tailnetauth/peer_linux.go` | The Linux lookup: the socket's uid in `/proc/net/tcp` and `/proc/net/tcp6` |
| `internal/tailnetauth/peer_other.go` | Other systems: no answer (`errors.ErrUnsupported`), so nothing is local there |
| `internal/tailnetauth/peer_test.go` | `/proc/net/tcp` lines (IPv4, IPv6, mapped, either byte order), a real loopback connection from this process (IPv4 and IPv6), and `FromHTTP` over a real server |
| `internal/tailnetauth/peer_windows_test.go` | A process the server may not open is not its user's |
| `sdk/api/handlers/openai/devyre_responses_socket.go` | `devyreStartSocketReader`: the one reader of a Responses websocket client, in both modes; it hands each `response.interrupt` to `devyreInterruptResponse`, queues every other frame, and cancels the request with the read error when the client leaves |
| `sdk/api/handlers/openai/devyre_responses_socket_test.go` | The real handler, auth manager and Codex executor against a scripted upstream websocket (the harness the interrupt tests share); a client that closes its socket mid-response closes the upstream socket, in both modes |
| `sdk/api/handlers/openai/devyre_responses_interrupt.go` | `devyreInterruptResponse`: the interrupt through the Codex executor, or answered locally, never with an error; the connection's live credential check |
| `sdk/api/handlers/openai/devyre_responses_interrupt_test.go` | In both modes: what Codex does on a steer (the interrupt unchanged on the upstream socket, its zero-output end passed through, the next create on the same socket, no error); interrupts before any response, after one ended and for an unknown id, answered without reaching upstream; the disabled-credential refusal |
| `sdk/cliproxy/auth/devyre_focus.go` | `devyreFocusTakesOver`: whether session affinity gives a bound session up to a usable focus |
| `sdk/cliproxy/auth/devyre_focus_test.go` | The focus in the selector (urgency, 5h, 7d and model-scoped gates, tiers, other providers, ends, rotation, the `focus` log reason) and under session affinity (explicit sessions, the fallback key, LCP, the log line, through the manager) |
| `sdk/cliproxy/auth/devyre_placement.go` | What `SessionAffinitySelector` tells its fallback about a pick (new placement or failover, the credential left, the active bindings), the bindings a credential failure suspended, whether a subagent joins its parent's credential, and `CarrySessionBindings` for a rebuilt selector |
| `sdk/cliproxy/auth/devyre_placement_test.go` | Stay power and its pools, a herd of failovers spreading over the credentials, the 429 retry through the conductor as a failover, released and removed bindings, forks and non-fork subagents, a failover that goes to a usable focus, the bounded suspended bindings, the active binding count |
| `sdk/cliproxy/auth/devyre_priority.go` | Exports the selectors' priority and availability rules, and the expiring-first view of a credential and the placement order, for the readings endpoint |
| `sdk/cliproxy/auth/devyre_session_cache.go` | The fork's views of a `SessionCache`: active bindings per credential, the keys bound to a credential, the live bindings and their restore with absolute expiries |
| `sdk/cliproxy/auth/devyre_session_cache_test.go` | Restoring bindings (expired, conflicting and live keys, the capacity, absolute expiries) and carrying them into a rebuilt selector with a new TTL |
| `sdk/cliproxy/auth/devyre_session_persist.go` | `SessionPersistence`: the bindings in `session-affinity.json`, loaded at start and saved when they changed, at most every 30 seconds, and on close |
| `sdk/cliproxy/auth/devyre_session_persist_test.go` | With a fake timer: save and restore, no timer while nothing changed, the flush on close, expired and malformed entries, corrupt and foreign files, the atomic replace, the capacity bound, following a rebuilt selector |
| `sdk/cliproxy/auth/devyre_transient.go` | A bound session's transient failure retries its own credential: which failures count, the budget and its waits, the retries of a call, a stream's start and its bootstrap |
| `sdk/cliproxy/auth/devyre_transient_test.go` | Through the conductor with a fake executor and wait: each transient failure kind, the bounded retries, credential failures, sessionless and new sessions, a credential that cooled down, a cancelled request, count tokens, a stream's start, its bootstrap and output already sent |
| `sdk/cliproxy/auth/selector_expiring_first.go` | The `expiring-first` selector, its placement order and the focus |
| `sdk/cliproxy/auth/selector_expiring_first_test.go` | Selector tests, including session affinity |
| `sdk/cliproxy/devyre_service_config_test.go` | Strategy aliases, settings and hot reload through the service config |
| `sdk/cliproxy/devyre_session_affinity.go` | `devyreRoutingSelector`: the selector a routing reload builds takes the current one's session bindings; the session bindings' persistence through the service's start, config applies and stop, and where the file lives |
| `sdk/cliproxy/devyre_session_affinity_test.go` | Bindings kept across routing reloads that change the gate and the TTL; across two real runs of the service stopped by a cancelled context; the file's place; `session-affinity-persist` off and back on, and a rebuilt selector followed |
| `sdk/cliproxy/executor/devyre_interrupt.go` | What the handler and the Codex executor share about `response.interrupt`: the outcomes, `DevyreResponseInterrupter`, the disabled-credential error and the local answers (`DevyreInterruptReply`) |
| `sdk/cliproxy/session/devyre_agent_type.go` | `applyClaudeAgentType`: the agent type a Claude Code subagent's request names (`X-Claude-Code-Agent-Type`), lowercased, and a `fork` marked as one |
| `sdk/cliproxy/session/devyre_agent_type_test.go` | Claude Code's own subagent requests with and without the agent type: a fork, a general-purpose subagent, a type in capitals, a value that is no type, a main thread naming one, and a session from `metadata.user_id` |
| `test/devyre_scripts_test.go` | Runs the PowerShell scripts with `powershell.exe` against a fake `tailscale status` and a fake management API (skipped without Windows PowerShell): the `allowed-hosts` that `tailnet-trust.ps1` writes, and its device selection (new devices only by exact MagicDNS name, renamed automation hosts, confirmation, the automation guard); `new-secrets.ps1 -Rotate` keeping the `tailnet-auth` block that the server wrote, with a fake `docker` first on `PATH`; the `cpa-common.ps1` helpers that `exposure-check.ps1` reads `allowed-hosts` with; `update-panel.ps1` on a copy of the panel folder with a fake `gh`, vendoring a release only when it matches its sha256 digest and is an HTML page, and writing its record |

### Conflict hotspots

These are all the upstream files the fork edits. Each edit is a small hunk, marked with a `// devyre:` comment where the reason is not obvious. When a sync conflicts here, keep upstream's new code and re-apply our hunk.

| File | Our change |
|---|---|
| `cmd/server/main.go` | `argvFlagConsumesValue` lists `lifeline-stdin` and `password-stdin` among the flags that take no value, and one call right after `flag.Parse` (`devyreApplyPasswordStdin`) reads the password line. The flags themselves are registered in `cmd/server/devyre_lifeline.go`. |
| `config.example.yaml` | A `devyre:` comment above `disable-control-panel` says the panel is built in, so `disable-auto-update-panel` and `panel-github-repository` have no effect, and `panel-github-repository` names the panel fork; a commented `management.tailnet-auth` block with every setting off; `routing:` documents `expiring-first`, a commented `session-affinity-persist`, and the `expiring-first` (gate, headroom, log) and `quota-observation` blocks. |
| `internal/api/handlers/management/api_tools.go` | Three hunks hook the usage cache into `APICall`: a lookup after `auth_index` and `url` are parsed, complete or fail after the upstream call, and a record on success. `/v0` and `/v8` share this handler. |
| `internal/api/handlers/management/auth_files.go` | `extractCodexIDTokenClaims` also emits `chatgpt_plan_type`, the key T3 Code reads. |
| `internal/api/handlers/management/auth_files_provider_oauth.go` | `RequestAnthropicToken` and `RequestCodexToken` read `redirect_port` first (`devyreOAuthRedirect`), and build the authorize URL and exchange the code through `devyreClaudeAuthURL`/`devyreClaudeExchange` and `devyreCodexAuthURL`/`devyreCodexExchange`, which fall back to upstream's calls without it. |
| `internal/auth/claude/anthropic_auth.go` | `GenerateAuthURL` and `ExchangeCodeForTokens` became wrappers passing `RedirectURI` to unexported `generateAuthURL` and `exchangeCodeForTokens`, which take the redirect as a parameter. |
| `internal/auth/codex/openai_auth.go` | `GenerateAuthURL` became a wrapper passing `RedirectURI` to an unexported `generateAuthURL`. |
| `internal/api/handlers/management/config_basic.go` | `normalizeRoutingStrategy` accepts `expiring-first`, so `PUT /routing/strategy` does too. |
| `internal/api/handlers/management/handler.go` | Two hunks: `Middleware()` asks `devyreTailnetAuthorize` first and skips the key for a trusted request, and the missing-key branch of `AuthenticateManagementKey` no longer calls `fail()`, so only a wrong key counts toward the ban (RESP's commands before `AUTH`, which ask with an empty key only to learn whether the IP is banned, included). |
| `internal/api/server.go` | Starts the quota observer once, right after the management handler is created, and the line after it loads the focus (`devyreLoadRoutingFocus`). One line adds `devyreTailnetContext` to the engine, so the proxy API's keyless check reads the live config. One `if`, after the keep-alive setup, registers the lifeline's `/keep-alive` (`devyreEnableKeepAliveProbe`), and the line after it keeps the listen report. The `devyre` field (`devyreServerState`) holds the fork's own server settings. `Start` calls `devyreReportListen` twice: when `net.Listen` fails, and right before the first connection is accepted. |
| `internal/api/server_management.go` | `serveManagementControlPanel` calls `devyreDenyFraming`, and once the control panel is known to be enabled it answers through `devyreServePanel` and returns, so upstream's file-or-download path after it never runs. |
| `internal/api/server_management_v8.go` | Three lines register `GET /v8/management/routing/quota-readings`, `/v8/management/routing/focus` (`devyreRegisterRoutingFocus`, its `GET`, `PUT` and `DELETE`) and `GET /v8/management/auth/session`. |
| `internal/api/server_middleware.go` | Two hunks: `accessAuthMiddleware` falls back to `devyreKeylessProxyAccess` after the API keys fail with a 401, and `serveExampleAPIKeyWarningPage` calls `devyreDenyFraming`. |
| `internal/api/server_options.go` | The `devyreKeepAliveProbe` and `devyreListenReport` fields that `WithDevyreKeepAliveProbe` and `WithDevyreListenReport` set. |
| `internal/api/server_routes.go` | `AttachWebsocketRoute` uses `devyreKeyOnlyAuthMiddleware`, so `/v1/ws` is never keyless. |
| `internal/cmd/run.go` | With the stdin lifeline on, `StartServiceWithPluginHost` runs until the lifeline ends (`devyreWithLifeline`) instead of applying the 10-second keep-alive idle limit. |
| `internal/config/config_defaults.go` | `DefaultPanelGitHubRepository` points at the panel fork; nothing fetches from it. |
| `internal/config/config_types.go` | The `Strategy` comment, the `SessionAffinityPersist` (`routing.session-affinity-persist`), `ExpiringFirst` and `QuotaObservation` fields on `RoutingConfig`, and `RemoteManagement.TailnetAuth` (`management.tailnet-auth`). |
| `internal/config/config_yaml.go` | `isKnownDefaultValue` keeps explicit zero values of the pointer-backed routing settings (`devyreKeepsExplicitZero`), so a save does not drop them. |
| `internal/managementasset/updater.go` | `defaultManagementReleaseURL` and `defaultManagementFallbackURL` point at the panel fork's releases; never restore `cpamc.router-for.me`. `StartAutoUpdater` returns through `devyreSkipAutoUpdater` (which logs the panel served) and `EnsureLatestManagementHTML` through `devyreRefuseDownload`, so no updater starts and nothing is downloaded, whoever calls them. |
| `internal/runtime/executor/codex_websockets_duplex.go` | A `response.interrupt` case next to `response.steer` in the writer's switch hands the frame to `devyreDuplexInterrupt` instead of the `default:` rejection. |
| `internal/runtime/executor/codex_websockets_session.go` | The `devyre` field (`devyreCodexResponses`) on `codexWebsocketSession`, and one line in `readUpstreamLoop` that calls `devyreObserveUpstream` for every text frame before any request reads it. |
| `internal/runtime/executor/codex_websockets_stream.go` | The per-response read loop's empty `response.incomplete` check skips the end of a response this router interrupted (`devyreInterruptedEnd`). |
| `sdk/api/handlers/openai/openai_responses_websocket.go` | Three hunks in `ResponsesWebsocket`: it starts `devyreStartSocketReader` in both modes (with steering on, the duplex input is that reader's channel, and upstream's `readResponsesWebsocketInput` is left unused); the loop reads every frame from that channel, taking the context's cancel cause as the read error; and a forward that ended because the reader cancelled the request logs at info, not as a failure. |
| `sdk/api/handlers/openai/openai_responses_websocket_forward.go` | `forwardResponsesWebsocket` ends a response on `response.incomplete` as on `response.completed`, so an interrupted response no longer closes the client socket. |
| `sdk/cliproxy/auth/conductor.go` | The `devyre` field (`devyreManagerState`) on `Manager`. |
| `sdk/cliproxy/auth/conductor_execution.go` | `executeMixedOnce` and `executeCountMixedOnce` hand a failed attempt to `devyreRetryTransient` right after the 401 refresh, and return when the request ended during its wait. |
| `sdk/cliproxy/auth/conductor_stream.go` | `executeStreamWithModelPool` takes a `devyreTransientRetry` budget per attempt and hands a failed start to `retryStart` and a failed bootstrap to `retryBootstrap`, each right after the 401 refresh. |
| `sdk/cliproxy/auth/selector.go` | Three conditions in `SessionAffinitySelector`, where it reuses a binding (the explicit-session cache hit, the fallback-key cache hit and the LCP match in `pickLCP`), also ask `devyreFocusTakesOver`; a yes treats the bound credential as unavailable, so the upstream reselect-and-rebind path runs. `SessionAffinitySelector` carries a `devyre` field (`devyreAffinityState`). `Pick` hands its fallback a placement through the context (`withPlacement`): a failover when the bound credential is unavailable (`devyreFailover`) or when a credential failure suspended the binding (`devyrePlacementFor`), a new placement otherwise; its `bind` ends a suspended binding, the failover lines name `from=` and the pick's `reason=`, and a subagent that says it is not a fork takes its parent's binding only with headroom (`devyreJoinsParent`). `extractExplicitSessionIDs` records the agent type the request names in its metadata (`devyreSetAgentType`). A cache hit, and a fork or alias joining its parent's binding, mark the request's metadata with that credential (`devyreMarkBound`) for the transient retries. `OnResult` suspends the binding it removes for a credential failure (`devyreSuspend`), and `InvalidateAuth` the bindings of the removed credential (`devyreSuspendAuth`). |
| `sdk/cliproxy/session/info.go` | `SessionInfo` carries `AgentType`, and the two Claude Code branches of `ExtractSessionInfo` (the session header, `metadata.user_id`) call `applyClaudeAgentType` for a subagent. |
| `sdk/cliproxy/auth/session_cache.go` | `SessionCache` carries `devyreOnChange`, and `replaceAliasGroupsLocked` and `removeAliasGroupLocked` end with `devyreChangedLocked()`, so every change to the bindings schedules a save. |
| `sdk/cliproxy/service.go` | The `devyreSessions` field (`devyreSessionState`) on `Service`. |
| `sdk/cliproxy/service_config.go` | `expiring-first` aliases in `normalizedRoutingRuntimeState`, construction in `newRoutingSelector`, and the gate, headroom and log settings in `routingRuntimeState`. That struct must stay comparable: no pointers, maps or slices. `applyManagerConfig` builds a changed selector through `devyreRoutingSelector`, which carries the session bindings over, and then calls `devyreApplySessionPersistence`. |
| `sdk/cliproxy/service_lifecycle.go` | `Run` calls `devyreStartSessionPersistence` right before the server starts, and `Shutdown` calls `devyreStopSessionPersistence` right after the server stopped. |

If upstream deletes the `/v0/management` routes, `TestT3Hub_*` fails first. Restore the three routes T3 uses (`GET auth-files`, `POST api-call`, `POST reset-quota`) in `internal/api/devyre_v0_shim.go`.

Upstream main has had its own `response.interrupt` support since after this fork's last sync (`6331b38b` and `d318bcc3`, upstream issues #6456 and #6483), with a reader in the same places. On the sync that brings it, keep upstream's reader and executor method and drop ours, then check what upstream still lacks and re-apply only that, with the `TestDevyre*Interrupt*` and `TestDevyreResponsesClientDisconnect*` tests as the check. In October 2026 it still answered an interrupt it could not forward with an `error` event, the failure described in [Codex response.interrupt](#codex-responseinterrupt), and still reported an interrupted response that ended without output as an empty-response failure.

## Routing and quota readings

With `routing.strategy: expiring-first` (the deploy template's setting), CPA places a new session on the credential whose quota would be lost soonest, among the credentials that can carry it:

- **Urgency** is the remaining percent of a credential's longest window, Claude's 7-day limit, divided by the hours until that window resets. The highest urgency is picked; equal urgencies take turns.
- **Gates.** A credential is skipped while a window that applies to the request has at most `gate-remaining-percent` (2) left and has not reset: the 5-hour window, the 7-day window, or a window scoped to the requested model family. When every credential is gated, the one whose gates reset first is picked, and upstream 429s and cooldowns take over.
- **Headroom.** A usable credential has headroom while its short windows (Claude's 5-hour window, Codex's primary window) have more than `headroom-short-percent` (20) left, and its long windows (the 7-day window) and the window scoped to the requested model family more than `headroom-long-percent` (5). A window whose reset is unknown or already passed does not count, and a credential without readings has headroom. A threshold at or below `gate-remaining-percent` turns its check off. Headroom comes before urgency because a session that starts on an account about to run out moves soon after, and a move rewrites the session's whole context into the next account's cache at the 1-hour write price, 40 times the read price. Under several max-effort Opus sessions a fresh 5-hour window lasted about 45 minutes.
- **Placement order.** A usable [focus](#focus) takes every request it can serve. Otherwise a new session goes to the first of these pools that is not empty: credentials with headroom and a known urgency, highest first; credentials with headroom but no urgency, mostly those without readings, taking turns; credentials without headroom, highest urgency first; and gated credentials, earliest gate reset first.
- **Credentials without readings** therefore come after the ones with readings and headroom. They get readings from their own traffic (response headers), from the panel or the T3 hub reading their usage, or from the idle poller (on by default with `expiring-first`). The poller checks every minute. It polls one Claude credential at a time, at least 10 minutes after any Claude usage call, so three new accounts all have readings about 20 minutes after a start; after that it re-polls a Claude credential once its weekly reading is 30 minutes old. Codex credentials are polled once their reading is 5 minutes old.
- **Session affinity.** A thread stays on the credential it started on, because the prompt cache is per account. It moves only when that credential becomes unavailable (cooldown, disabled, removed), the binding expires after an hour, or a [focus](#focus) takes it, never for urgency or headroom. Expiring-first only places new threads and failed-over ones.
- **Failover.** A failover keeps the placement order, but inside the two headroom pools it ranks by stay power first: the lowest remaining percent of the credential's short windows (100 when none is known) divided by one plus its active bindings for the same provider and model, a binding being active when its session made a request in the last 15 minutes; then urgency, then turns. When an account runs out, every session on it fails over within seconds. Ranked by urgency alone they all landed on the same account, which then ran out too, and they all moved again: 66 of 135 moves in two days of traffic were second moves within 5 hours. By stay power they spread over the accounts that can carry them.
- **Subagents.** A fork reads its parent's cached prefix, so its first request still takes its parent's credential while that credential is available (`session-affinity-subagents`). A subagent that says it is not a fork starts a fresh context with nothing of its parent's cache to reuse: it takes its parent's credential only while that credential has headroom for the request, and is otherwise placed like a new session, with the info line `session-affinity: subagent left its parent's auth without headroom | ... agent=<type> ...`. Subagents, with contexts of up to 950k tokens, made 85 of the 135 moves in the sample. Claude Code says which is which only to a gateway that asks for it: with `CLAUDE_CODE_GATEWAY_HINT_HEADERS=1` (Anvil sets it for its routed sessions) a subagent's request names its type in `X-Claude-Code-Agent-Type`, `fork` for a fork; without it, a fork and any other subagent send the same headers (`X-Claude-Code-Agent-Id` alone; CLI 2.1.296 and 2.1.304). A subagent that names no type may be a fork, so it takes its parent's credential as before: moving a fork rewrites its parent's whole context on another account, while staying costs a move only if the credential then runs out. Before the agent type was read, every Claude Code fork counted as a subagent of another kind and left a parent short of headroom, writing its whole context again.
- **The retry after a 429 is a failover.** A 429 on the bound credential removes the binding before the request is retried on another credential, as upstream does, so the retry used to be placed like a brand-new session. The selector now remembers each binding a credential failure removed until it would have expired, and the session's next pick, that retry included, is its failover. The sessions of a removed credential fail over the same way.
- **A transient failure keeps the session.** A request whose session already had its binding (its own, or the parent's binding a fork reads the cached prefix of) retries that credential when it fails transiently before anything reached the client: HTTP 500, 502, 503, 504 or 529 (Anthropic's overloaded), or a transport failure such as a reset, a DNS or TLS failure, or `server closed idle connection`. It waits half a second, then one more second, so at most two retries; the failed attempts count in the credential's statistics but put it in no cooldown and leave the binding alone. These failures are not the credential's fault, so another account does not improve the odds, yet in the sample two 502 pages from Anthropic's edge and one closed idle connection each moved a session to another account, which then wrote the whole context into its cache. Unchanged: a credential's own failures (401, 403, a 429 with its cooldown) fail the session over at once, a credential that became disabled, refreshing or cooling during the wait is not retried, nothing is retried once the client got its first bytes, and a request without such a binding (sessionless, a session's first request, a non-fork subagent joining its parent) moves on at once. Each retry logs `session-affinity: transient failure on the bound auth, retrying it | ... retry=1/2 wait=500ms`.
- **Bindings survive routing reloads.** A config reload that changes a routing setting (the strategy, session affinity, its TTL or subagents setting, an expiring-first setting) rebuilds the selector, and the old selector's binding cache went with it, so every session was placed anew on its next request. The Anvil app rewrites `config.yaml` at launch when it changed, and the panel writes it too. The new selector now takes the old one's bindings, and the bindings a credential failure suspended, with their absolute expiries: a new TTL applies from each binding's next use.
- **Bindings survive restarts.** The router restarts with the app that runs it (7 times in the two days of the sample), and the bindings lived in memory only, so each restart placed every session anew. With `routing.session-affinity-persist` (on by default; it needs session affinity) they are kept in `session-affinity.json`, in `WRITABLE_PATH` when it is set (the Anvil app sets it to the router's data folder), else next to `config.yaml`. The file is versioned JSON listing each binding's key, its other alias keys, its credential and its absolute expiry. It is written atomically (a temporary file in the same folder, readable by this user only, renamed over it), at most every 30 seconds while bindings change, and once more on a graceful stop: a signal, or the lifeline's `shutdown` line or EOF, which all end in the service's stop. A change arms one save timer; nothing runs while nothing changed. At start, expired and malformed entries are dropped, a corrupt or foreign file is logged and ignored, and the cache's capacity bounds what is restored. A restored binding is used like a live one: its credential is checked at every pick, so a binding to a removed credential fails its session over.
- **Logs.** Every pick logs `expiring-first: pick | ...` (at info with `log-picks`, otherwise debug) with its pool as `reason`: `focus`, `most-urgent`, `no-data`, `low-headroom` (naming the windows) or `all-gated`, and for a failover `failover-stay-power` (with `stay_power=` and `bindings=`), `failover-low-headroom` or `failover-all-gated` (a usable focus still answers `focus`). The info-level `session-affinity:` line of a failover names the credential left (`from=`) and that reason; after a credential failure it reads `bound auth failed, reselected`.
- **Priority tiers** keep their meaning: only the highest available tier is considered.

`GET /v8/management/routing/quota-readings` (management key required; not on `/v0`) shows what the selector sees: per credential, its effective windows with `remaining_percent`, `resets_at`, `observed_at` and `source` (`header`, `usage` or `poll`), whether it is `usable` and why not (`gate_reason`, such as `"5h exhausted"` or `"7d exhausted"`), whether it has `headroom` and, without it, why (`headroom_reason`, such as `"5h at 14% left"`), its `urgency_per_hour`, its `rank` within its provider in the placement order above (1 is where a new session goes next; 0 means gated, unavailable or in a lower tier), and whether it is `focused`, with `focus_until` (the focus's end, `null` without one or when it is not focused). A focused credential that is ranked at all is rank 1. The endpoint and the selector share one ordering function, so the ranks cannot drift from the picks. Model-scoped windows are ignored there, so the view matches a request for an unscoped model.

### Focus

Focusing a credential makes the router use it for every request it can serve, until the focus ends or is removed. It is meant for an account whose weekly quota should be burned first, such as one that just got a full weekly reset: the 7-day window resets whether its quota was used or not.

- **One per provider.** Focusing a credential replaces any focus of its provider. Focuses of different providers apply side by side.
- **What it can serve.** The focused credential must be among the selector's candidates: not disabled, cooling down or otherwise unavailable, and in the highest available priority tier, so priority tiers keep their meaning and a focus below the top tier does nothing. And its readings must not gate it for the request's model: neither its 5-hour nor its 7-day window may be exhausted, nor a window scoped to the requested model family (an exhausted Fable window gates Fable requests only). Then the pick is the focus, whatever the urgencies, logged with the reason `focus`. Otherwise the pick is the normal expiring-first pick.
- **Threads move to it.** Focus deliberately overrides session affinity: a thread bound to another credential moves to the focus on its next request that the focus can serve, whether the binding came from an explicit session ID, the fallback key or the prompt-prefix (LCP) match, and is rebound to it. The log shows `session-affinity: focus takes over the binding | session=... from=<auth> focus=<auth>`. The moved thread rebuilds its prompt cache on the new account once.
- **Afterwards.** A thread moved to the focus stays bound to it, like any binding. When the focus ends, is removed or becomes gated, its threads stay on that credential until it becomes unavailable (for example a 429 cooldown) or the binding expires; new threads go back to the expiring-first order.
- **Only with `expiring-first`.** The other strategies ignore the focus.
- **Ends.** A focus with an end is inert from that instant on, everywhere, and is dropped at the next change or start. A credential that is removed loses its focus within a minute (the quota observer's tick).

The endpoints need the management key (or passwordless trust) and exist only on `/v8`. Every answer, `200`, is the list of active focuses whose credential still exists, by provider:

```json
{"focus":[{"auth_id":"claude-a.json","auth_index":"1a2b3c4d","name":"claude-a.json","provider":"claude","until":"2026-10-21T12:00:00Z","set_at":"2026-10-19T12:00:00Z"}]}
```

`name` is the credential's file name as the credential list shows it, or its ID when it has none; `until` is `null` for a focus without an end.

| Request | Effect |
|---|---|
| `GET /v8/management/routing/focus` | The list. |
| `PUT /v8/management/routing/focus` with `{"name":"<auth ID or file name>","until":"<RFC 3339>"}` | Focuses the credential, found by ID, then by file name; `"until": null` or no `until` means no end. 404 `{"error":"auth file not found"}` for an unknown credential, 409 for a plugin's virtual credential, 400 for a body that is not such an object, an `until` that is not RFC 3339 with a zone, or one that is not in the future. |
| `DELETE /v8/management/routing/focus?name=<auth ID or file name>` or `?provider=<provider>` | Removes that credential's focus, or the provider's. Exactly one of the two (400 otherwise); removing a focus that is not there succeeds. |

**Persistence.** The focus is kept in `routing-focus.devyre` in the auth directory (`auths\` here, so `backup.ps1` includes it): `{"version":1,"focus":[{"auth_id":...,"provider":...,"until":...|null,"set_at":...}]}`, mode 0600, replaced atomically through a temporary file. It is not a `.json` file because the auth watcher loads every `*.json` there as a credential. The server reads it once at start; a missing file means nothing is focused, and an unreadable or invalid one is logged as a warning and ignored, so nothing is focused and the next change replaces it. A change that cannot be saved answers 500 and changes nothing. Only a configured auth directory is used: the server binary always resolves `auth-dir` (its default included) before it starts, so a config built in code without one (tests, embedders) keeps the focus in memory only and never reads or rewrites the default directory, which may hold another deployment's focus. So does an auth directory that does not resolve.

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

The panel is built into the image, so nothing else is needed: no token, no download, no access to the panel fork.

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
- To rotate secrets, run `new-secrets.ps1 -Rotate`. It keeps the previous `config.yaml` as `config.yaml.bak-<timestamp>`, carries `management.tailnet-auth` (the passwordless devices, which exist only in `config.yaml`) over unchanged, and restarts the running container, so the old keys stop working at once. Then update the password manager, the T3 hub key and T3's `ANTHROPIC_AUTH_TOKEN`. If an allowed device may be compromised, add `-ResetTailnetAuth`: the block then comes from the template with empty lists, nothing is keyless, and you allow your devices again with `tailnet-trust.ps1 -Include <names>` (a run without `-Include` has nothing to keep).
- To bypass the pool, use `claude-direct` for one run, or `Disable-CpaPool`. In T3, use the "Claude Direct" instance.
- After a tailnet rename, run `tailscale-serve.ps1`, then `tailnet-trust.ps1` with no arguments, then `exposure-check.ps1`.

## Keep it running

- **Start now:** double-click `devyre\start-cpa.cmd`. It runs `devyre\scripts\ensure-running.ps1`, which checks the chain and fixes only what is down:
  1. If the Docker engine is unreachable, it starts Docker Desktop and waits up to 5 minutes for the engine.
  2. If `cpa` is not answering `http://127.0.0.1:8317/healthz`, it runs `docker compose up -d` and waits for health. If the container is "Up" but still unreachable (no port published), it recreates it.
  3. If the tailnet URL is down, for example after a tailnet rename, it runs `tailscale-serve.ps1 -Http`, at most once an hour.
  It is safe to run any time.
- **Autostart:** run `powershell -ExecutionPolicy Bypass -File devyre\scripts\install-autostart.ps1` once.
  - It registers the per-user scheduled task "Devyre CPA", which runs `ensure-running.ps1` hidden at every sign-in (after 30 s) and every 5 minutes after that.
  - A crash, a Docker restart, sleep/resume or a tailnet rename heals within minutes.
  - It starts Docker Desktop itself, so it works even when Docker Desktop's "start at sign-in" entry is disabled in Task Manager's Startup apps.
  - No admin rights are needed. Docker Desktop runs only inside a signed-in session, so CPA comes up when you sign in, not at the login screen.
  - Remove it with `install-autostart.ps1 -Remove`.
- **Log:** `%USERPROFILE%\.cli-proxy-api\logs\ensure-running.log`. A healthy check writes nothing.
- **OAuth callback ports are optional.** 54545 (Claude) and 1455 (Codex) live in `devyre/deploy/docker-compose.oauth.yml`. At boot Windows reserves random blocks of the dynamic port range (49152-65535) for Hyper-V (`netsh interface ipv4 show excludedportrange protocol=tcp`). When a block covers 54545, Docker cannot publish it. That used to keep the whole container down, 8317 included. Now `up.ps1` and `ensure-running.ps1` start without the overlay and log a WARN. Logins still work: paste the callback URL into the panel's "Submit Callback URL". To get the ports back after a reboot that frees them, run `up.ps1 -NoBuild`.

## Tailnet-only and passwordless

CPA is reachable only from this PC and from your tailnet, never from the LAN or the internet:

- The container publishes 8317, and 54545 and 1455 when the OAuth overlay is in effect, on 127.0.0.1 only. `up.ps1` refuses to start a compose file that publishes a port anywhere else, and stops a running container that does.
- `tailscale serve` publishes `http(s)://<machine>.<tailnet>.ts.net:8318` to the tailnet only. Funnel stays off.

On top of that, `management.tailnet-auth` lets chosen tailnet devices, this PC included through its tailnet URL, use CPA without a key. The panel opens straight into the dashboard, the management API needs no key, and with `proxy-api: true` the proxy API needs no API key either. Everything else still needs the key, exactly as before. Read [Who is trusted, and why](#who-is-trusted-and-why) first: it trusts every local process that learns the right headers.

### Set it up

1. Preview: `devyre\scripts\tailnet-trust.ps1 -Include <this PC>,<phone>,<laptop> -ShowOnly`. Names are MagicDNS names: the machine names in the admin console and in `tailscale status`, which the tailnet keeps unique, matched case-insensitively. Host names are never matched, because each node reports its own and several nodes can share one (iPhones all report `localhost`). A device that is not allowed yet must be named exactly. A wildcard such as `iphone*` or `*` only keeps devices that are already allowed, plus this PC, so on a first run `-Include *` selects only this PC. The table lists every device with its decision and the reason. Each new device is listed with its node ID, OS and registration date: a phone you just set up shows a recent date and the OS you expect.
2. Write: the same command without `-ShowOnly`. When it adds new devices, the script asks first; `-Force` skips the question once you have checked the preview, and a session that cannot ask writes nothing. The script writes only `management.tailnet-auth`, with `PUT /v8/management/config/management/tailnet-auth`, then re-reads the management section and fails unless nothing else changed. The running server applies it at once. A server built before this feature has no `/v8/management/auth/session` and would refuse the block, so the script writes nothing there and asks you to run `up.ps1` first.
3. Check: `devyre\scripts\exposure-check.ps1`. It must end with 0 FAIL.

The block: `allowed-logins` holds the login that owns this PC; `allowed-devices` holds every Tailscale IP, IPv4 and IPv6, of the selected devices; `allowed-hosts` holds this PC's MagicDNS FQDN and short name, each with tailscale serve's port (`<machine>.<tailnet>.ts.net:8318` and `<machine>:8318`), plus `localhost` and `127.0.0.1`. The server trusts a tailnet name only together with that port. An `allowed-hosts` written before this rule had bare names, which grant no tailnet trust any more: after updating the server, run `tailnet-trust.ps1` with no arguments once (`exposure-check.ps1` reports the bare names). When the block is new, `enabled` and `proxy-api` start as `true` and `allow-local` as `false`, like the deploy template: in the container every request arrives through the Docker gateway, which hides which program or container sent it, so `allow-local` would trust nothing, and this PC uses the tailnet URL (it is in `allowed-devices`) for passwordless access. Afterwards the script keeps their live values. Each list fails closed: an empty list means no keyless access through it.

- **Add a device:** re-run with `-Include` naming every device to keep and the new one by its exact MagicDNS name (the selection replaces the list), `-ShowOnly` first. Check the new device's node ID, OS and registration date, and the `allowed-devices +` lines under "Changes", before you confirm. New devices of your login need the key until you do.
- **Remove a device:** re-run without it in `-Include`, or add `-Exclude <name>`.
- **After a tailnet rename:** run `tailscale-serve.ps1`, then `tailnet-trust.ps1` with no arguments. Without `-Include` it keeps the devices that are already allowed and refreshes the names: the FQDN in `allowed-hosts` goes stale on a rename while the short name keeps working.
- **Turn it off:** `tailnet-trust.ps1 -Disable`. Every request needs a key again at once; `-Enable` turns it back on. `allow-local` and `proxy-api` are in the panel's config editor under `management.tailnet-auth`, or set one from this PC with the management key, for example: `Invoke-RestMethod -Method Put -Uri http://127.0.0.1:8317/v8/management/config/management/tailnet-auth/allow-local -Headers @{ Authorization = 'Bearer <management key>' } -ContentType application/json -Body 'false'`.

Scripts and `curl` send `X-CPA-Keyless: 1`, or any `Authorization: Bearer` value, and no key: `curl.exe -H "X-CPA-Keyless: 1" http://<machine>.<tailnet>.ts.net:8318/v8/management/auth/session` (a fork-only endpoint, not in upstream's API docs) answers `{"authenticated":true,"method":"tailnet","login":"<login>","device":"<tailnet IP>"}`: `device` is the caller's tailnet IP and `login` its login (empty for a listed tagged node). A server running natively, not in a container, with `allow-local` answers `"method":"local"` to programs of its own OS user on `http://127.0.0.1:<port>`. Trust is checked before the key, so `"key"` appears only when a valid key authenticated an untrusted request; otherwise the answer is 401. The panel probes that endpoint at start-up. The header is a constant, not a secret.

### Who is trusted, and why

A request skips the key only when all of these hold:

- `tailnet-auth.enabled` is true, and the container's direct TCP peer is loopback or inside `server.trusted-proxies` (the Docker gateway).
- It is not a Funnel request.
- Its `Host` is in `allowed-hosts`, and an `X-Forwarded-Host` equals it. An entry with a port matches only that port.
- It passes the browser guard. A present `Origin` must be this same origin, and a present `Sec-Fetch-Site` must be `same-origin` or `none`. Without `Origin`, the request must carry a non-browser signal: `X-CPA-Keyless: 1`, `Authorization: Bearer <non-empty token>`, or a non-empty `X-Management-Key`, `X-Api-Key` or `X-Goog-Api-Key`. Browsers never add these on their own; a page from another site can add one only in CORS mode, which always sends its `Origin`, and the guard rejects that `Origin`. Query keys, Basic and other schemes do not count.
- Then exactly one path applies, chosen by the `Host`:
  - **Tailnet** (this PC's MagicDNS name, through tailscale serve): the `Host` names tailscale serve's port and matches an `allowed-hosts` entry with that same port (`<machine>.<tailnet>.ts.net:8318`); a bare name, a `Host` without a port and `server.port` (8317) never count. `X-Forwarded-For` is exactly one tailnet IP that is listed in `allowed-devices`, and the `Tailscale-User-Login` that serve set is in `allowed-logins`. A listed tagged node, which serve stamps with no identity, needs only its IP, and only while `allowed-logins` is not empty. tailscale serve overwrites `X-Forwarded-For` and the `Tailscale-User-*` headers, so tailnet devices cannot forge them, and it keeps the `Host` the browser sent, which always names port 8318.
  - **Local** (a loopback `Host` such as `localhost` or `127.0.0.1`, with `allow-local: true`): a direct request from a program of the server's own OS user. The direct TCP peer must be loopback: a request through `server.trusted-proxies`, the Docker gateway included, is never local, because the proxy hides which program or container sent it (so in the Docker deployment `allow-local` trusts nothing). And the OS must say the peer's process runs as the server's user: on Windows the TCP table's owning process and its token, on Linux the socket's uid in `/proc/net/tcp`; elsewhere (macOS) nothing is local. Proxy headers (`X-Forwarded-For`, `X-Real-IP`, `Forwarded`, `X-Forwarded-Host`, `Tailscale-*`) never arrive through serve on a loopback name, so with one the request is untrusted.
- None of the headers the decision reads (`X-Forwarded-For`, `X-Forwarded-Host`, `Origin`, `Sec-Fetch-Site`, `X-CPA-Keyless`, `Tailscale-User-Login`, `Tailscale-User-Name`) appears twice.
- For the management API, trust never opens more than the key would: a management key must be configured, and `management.allow-remote` must be true unless the client is loopback. Inside the container no client is loopback, this PC and tailscale serve's hop included, so the deploy template keeps `allow-remote: true`.

Trusted management responses carry no `Access-Control-*` headers and send `Cache-Control: no-store`, so another origin can never read them. `/management.html` and the safe-mode page at `/` refuse framing (`Content-Security-Policy: frame-ancestors 'none'`, `X-Frame-Options: DENY`).

To audit keyless access, turn on `observability.logs.debug` (in the panel's config editor). Every decision is then logged at debug level, for example `tailnet-auth: management GET "/v8/management/config" trusted without a key, via=tailnet login="<login>" device=<tailnet IP> reason="allowed tailnet device"`, or `... needs the key reason="device not in allowed-devices"` for a refusal. Proxy API lines carry the request ID of their access log line. Keys are never logged.

**Keep your own automation out: tag it.** Every node on this tailnet, the CI runner and the bot VM included, is untagged and owned by the same login, so tailscale serve stamps them all with the allowed login. Only `allowed-devices` keeps them out, and an allowed CI runner or bot could read the Claude OAuth tokens through the management API. The barrier that holds is tagging those hosts in the tailnet policy (for example `tag:ci` and `tag:bot`): tagged nodes are never candidates and get no identity headers. Until they are tagged, keep in mind:

- The device list is made of Tailscale IPs, which the control plane assigns, so a device that is already allowed stays put whatever it calls itself.
- Names are a weaker signal. A node reports its own host name and OS, and its MagicDNS name follows the host name unless you pin the machine name in the admin console. A compromised automation host can therefore rename itself to look like a phone. This is why `tailnet-trust.ps1` adds a new device only when you name it exactly, never through a wildcard, and why it shows the node ID and registration date, which the node cannot change.
- The script also leaves out any device whose MagicDNS name or host name holds the token `ci`, `runner`, `bot`, `build` or `agent`, unless `-AllowAutomationName` gives its exact MagicDNS name. That guard only catches mistakes, not a machine that picked its name to slip past it.

**The trust boundary is this PC's loopback.** Inside the container, tailscale serve's hop, browsers and programs on this PC, and other containers reaching the host through `host.docker.internal` all arrive from the same Docker gateway, so the server cannot tell them apart. Software that can reach `127.0.0.1:8317` can also send forged `X-Forwarded-For` and `Tailscale-*` headers with any `Host`, the tailnet name on port 8318 included. A web page cannot: browsers never let a page set `Host`, so a page that gets the tailnet name resolved to 127.0.0.1 (DNS rebinding) reaches the loopback publish with `Host: <machine>.<tailnet>.ts.net:8317`, which the tailnet path refuses. Nothing else may listen on port 8318 on loopback; `exposure-check.ps1` fails if something does, and it sends that rebinding request itself and expects a 401. So:

- **tailnet-auth trusts every local process.** The trusted set is the allowed tailnet devices plus anything that can reach this PC's loopback and learn the headers to forge: any program of any OS user on this PC, and any container. `exposure-check.ps1` fails when a throwaway container is trusted as local.
- `allow-local` changes nothing about that: in the container it trusts no request at all, and with it off, software on this PC can still forge the tailnet headers.
- Processes running as your user can read `secrets\` and `auths\` anyway.
- Passwordless access suits only a single-user PC whose containers you trust.
- It depends on loopback-only publishing: published on the LAN, any device there could forge the headers. `up.ps1` refuses that and `exposure-check.ps1` checks it. `server.trusted-proxies` must also contain the Docker gateway. Passwordless access reads that list live, but bans and logs read it only at start, so restart the container after changing it.

### What still needs the key

- Every device not in `allowed-devices`: the CI runner, the bot VM, devices of other users, tagged nodes you did not list, and new devices.
- Funnel requests, cross-site browser requests, and requests without `Origin` that carry no keyless signal.
- RESP on port 8317 and `/keep-alive` take only the key or the local password (under `-lifeline-stdin`, `/keep-alive` also answers a supervisor's challenge with a proof, which opens nothing). `/v1/ws`, the AI Studio relay socket, is never keyless.
- Unchanged and unauthenticated, as before: the OAuth callbacks (state-matched), `/v0/resource/plugins/*`, `/healthz`, `/` and `/management.html`.

A request without a key no longer counts toward the ban, and neither do RESP's commands before `AUTH`; only a wrong non-empty key does, and five of them ban the client IP for 30 minutes. Trusted requests never touch the counter, and a wrong key on a trusted request is ignored, so T3 Code can keep any key.

**Proxy API (`proxy-api: true`).** A valid API key always wins, so configured clients keep their own usage rows and isolation. A trusted request without a valid API key is attributed to `tailnet:<login>@<device IP>` (the login is empty for a listed tagged node) or to `local`; like every keyless request it needs a non-browser signal, and any non-empty `Authorization: Bearer` or `X-Api-Key` value counts. That principal also seeds caller-scope session isolation, the Claude MCP alias secret, the Codex prompt-cache key and the xAI reasoning-replay namespace, which is why it is per device. It need not be secret: the server derives it from verified identity, and only software inside the trust boundary could forge it.

### Residual risks

- **Plain HTTP.** Over plain HTTP, only DNS authenticates the tailnet URL. On a device whose tailnet names are answered by someone else's DNS, a hostile network can serve its own page under `http://<machine>.<tailnet>.ts.net:8318`. Two cases:
  - Tailscale is off or not answering DNS on this PC, and the name is resolved to 127.0.0.1. The page then reaches only the loopback publish on 8317, where the tailnet path never applies because it is bound to port 8318. That holds over plain HTTP and HTTPS alike.
  - A device is connected to the tailnet but uses another DNS server, with "Use Tailscale DNS" off. The page can then be rebound to this PC's tailnet IP, so it talks to CPA through tailscale serve as that device, with its keyless rights. Keep Tailscale DNS on for every allowed device. Never open the tailnet URL while Tailscale is off, and if you did on an untrusted network, clear that site's data on the device.

  Enabling Serve HTTPS, with the one-time admin link that `tailscale-serve.ps1` prints, removes the second case, because a hostile page cannot present the certificate. It also turns on the browser `Sec-Fetch` guard, which the plain-HTTP URL never triggers.
- **Same-origin content.** Anything served from the CPA origin, the panel and the plugin resource pages, acts with keyless admin rights. Installing a plugin is therefore equivalent to admin access. The panel changes only through a commit to this repository and a rebuild (`update-panel.ps1`, then `up.ps1`): a release published on the panel fork never reaches a running server, and neither does a file left in `static\`.

### What exposure-check.ps1 verifies

Exposure: every port of the cpa container is published on 127.0.0.1; nothing listens on `0.0.0.0` or a LAN address for 8317, 54545 or 1455, and 8318 listens on Tailscale addresses only, not even on loopback; this PC's LAN addresses cannot reach 8317 or 8318; Funnel is off; tailscale serve has exactly one web handler on port 8318, `/` to `http://127.0.0.1:8317`, keyed to the current MagicDNS name, and no raw TCP forward reaches CPA; both health URLs answer 200.

Passwordless: the Docker gateway is inside `server.trusted-proxies`; `allowed-hosts` holds the current FQDN with port 8318 (a bare FQDN is reported as written by an older `tailnet-trust.ps1`, and a block that is on with empty `allowed-hosts` and `allowed-devices`, the template's, is a SKIP rather than a stale name); over the tailnet the keyless session probe is 200 (SKIP when this PC is not in `allowed-devices`), the same request without a signal is 401, and with `Origin: http://evil.example` it is 401 or 403; direct `http://127.0.0.1:8317` without a key is refused whatever `allow-local` says (`allow-local` on is a WARN: it trusts nothing in the container), and forged tailnet headers or a foreign `Origin` there are refused; so is a direct request with the tailnet name as `Host` (on 8317 or without a port), a listed `X-Forwarded-For` and `X-CPA-Keyless`, which is what a DNS-rebinding page would send; with `allow-local` on, a throwaway container trusted as local is a FAIL. A failed keyless probe lists its preconditions, `management.allow-remote` included. It reads the live config with the management key, sends no wrong key, and changes nothing.

## Sidecar lifeline

Upstream stops a server started with `-password` 10 seconds after the last authenticated `GET /keep-alive`, so the process that started it has to ping it every few seconds, and loses it after a 10-second stall of its own. With `-lifeline-stdin` that process holds the server's stdin instead:

- The server shuts down gracefully, as on SIGTERM, when stdin reaches EOF (the supervisor closed it, or exited and the OS closed it) or reads the line `shutdown`. Surrounding whitespace and a trailing CR are ignored; other lines are ignored with a warning. On Windows, where a parent cannot send a console child a signal, this is the graceful way to stop it.
- **The listening line.** The moment its API listener is bound, before it accepts a connection, the server prints `lifeline: listening on <address>` on stdout (`127.0.0.1:8317`); when it cannot bind, `lifeline: cannot listen on <address>: <error>`, and it exits. Only this process writes to the pipe its supervisor reads, so a supervisor that waits for the line never sends anything to a port another program took first, and needs neither polling nor `/healthz`.
- `/keep-alive` has no idle limit. It answers 200 to the local password and 401 to anything else, as upstream. It also answers a challenge, the header `X-CPA-Lifeline-Challenge: <22 to 128 base64url characters>` with no credential, with `{"status":"ok","proof":"<proof>"}`: base64url (unpadded) HMAC-SHA256 keyed by the local password over `cpa-lifeline-proof/v1`, the server address the request reached (`127.0.0.1:8317`, `[::1]:8317`) and the challenge, one per line. A supervisor that compares it with its own computation, in constant time, knows the process on the port holds the password it handed over, without ever sending it: another program on the port, which answers 200 to anything, has no proof, and a proof from a server on another port names that port. Challenges are answered only on loopback (403 otherwise), never to a request that carries `Origin` (403), and with `Cache-Control: no-store`. `internal/api.LifelineProof` computes it.
- Without `-password` there is no `/keep-alive`, as upstream.
- Connect stdin to a pipe the supervisor keeps open. With stdin at `NUL` or `/dev/null` the server stops at once.

**`-password-stdin`** reads the local password from the first line of stdin instead of `-password`, whose value other local users can read from the process list on Linux (`/proc/<pid>/cmdline`) and macOS. The line ending (LF or CRLF) is not part of it; an empty, blank or over-long (4 KB) line, or stdin ending first, stops the server with exit code 1, and so does `-password` given too. With `-lifeline-stdin` the rest of stdin is the lifeline, read from the same buffer, so a supervisor may write the password and later lines at once.

A supervisor can tell whether a build has all of this from its help output: `-h` lists `-lifeline-stdin` and `-password-stdin`. They came together: a build that lists `-password-stdin` also prints the listening line and answers challenges.

The flags are off by default and change nothing else. The Docker deployment does not use them.

## OAuth redirect port

`GET /v8/management/oauth/auth-url?provider=claude|codex&redirect_port=<port>` (and the deprecated `/v0/management/anthropic-auth-url` and `codex-auth-url`, which share the handlers) puts a loopback redirect on that port into the authorize URL, and the same redirect into the token exchange, which the provider requires. A caller that runs its own callback listener on a port the OS picked for it, such as a desktop app's sidecar supervisor, then never collides with another program holding the provider's fixed port (this deployment publishes 54545 and 1455 itself, and `codex login` takes 1455). Without the parameter nothing changes.

| Provider | Redirect for `redirect_port=<port>` | Why this form |
|---|---|---|
| Claude | `http://localhost:<port>/callback` | What Claude Code itself sends: its own listener binds `127.0.0.1` on port 0, the OS picks the port, for every sign-in, with the same client id. |
| Codex | `http://127.0.0.1:<port>/auth/callback` | auth.openai.com accepts any port only for the loopback IP literal (RFC 8252 §7.3). `localhost` with a port other than 1455, `[::1]`, another path or another host answer `invalid_authorize_request`. |

- A value that is not plain decimal digits from 1 to 65535 answers 400 `invalid redirect_port`, and so does an empty one. With `is_webui` it answers 400 too: that flow's callback forwarder listens on the fixed port.
- The router never listens on the port; the caller does, and hands the code to `/v8/management/oauth/callback` as before.
- A caller can tell a router without the parameter (upstream, or an older fork) from the returned URL: its `redirect_uri` still names the fixed port.

Evidence (2026-10-13, no sign-in and nothing submitted: each authorize URL a throwaway router returned was loaded in headless Chrome 154 with a fresh profile, only `redirect_uri` changed, and the page it settled on was read): for Codex, `localhost:1455`, `127.0.0.1:1455` and `127.0.0.1:<random>` reached the "Welcome back" sign-in page, while `localhost:<random>`, `[::1]:<random>`, `localhost:1455/other`, `127.0.0.1:<random>/other` and `https://example.com/auth/callback` got `{"error":{"code":"invalid_authorize_request"}}`. claude.ai checks nothing before sign-in (the non-loopback control also reached its sign-in page), so for Claude the evidence is Claude Code 2.1.296 itself, whose OAuth listener starts with `listen(0, "127.0.0.1")` and sends `http://localhost:${port}/callback` in both requests. The app's `scripts/probe-oauth-redirects.ts` repeats the check.

## Codex response.interrupt

What Codex 0.163.0 does ([`responses_websocket.rs`](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/codex-api/src/endpoint/responses_websocket.rs#L738-L828), [`turn.rs`](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/core/src/session/turn.rs#L2730-L2758), [`common.rs`](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/codex-api/src/common.rs#L412-L429)):

- **When new input arrives while a response streams** (`turn/steer`), with its instant interrupt (`features.instant_interrupt`, on by default in 0.163) and a model whose catalog entry sets `use_responses_lite` (this router's Codex catalog sets it for the current models), it sends `{"type":"response.interrupt","response_id":"<id from response.created>","mode":"discard_partial_items"}` on the same socket. It then reads that response until it ends, ignoring any acknowledgement, and sends its next `response.create` there with `previous_response_id`. If nothing comes back, it waits for the response to end on its own. Its own tests model the backend's answer ([`pending_input.rs`](https://github.com/openai/codex/blob/rust-v0.163.0/codex-rs/core/tests/suite/pending_input.rs#L1242-L1352)): `response.interrupt.accepted` with the `response_id`, optionally `response.output_item.interrupted`, then `response.incomplete` with `incomplete_details.reason: "interrupted"`, which Codex takes as the response's completion.
- **An `error` event** with a status, read at any point, fails the request being read, the next one included.
- **Stopping a turn** (`turn/interrupt`) sends no frame: Codex drops the stream and closes its socket.

The router rejected the frame with `{"type":"error","status":400,"error":{"message":"unsupported websocket request type: response.interrupt",...}}`. Without `upstream.codex.response-steering`, which Anvil does not set, the handler read the client socket only between responses, so the interrupt waited until the response it was meant to stop had run to completion, and the rejection then failed Codex's next request and the turn. With steering on, the duplex rejected it at once. Now:

- **One reader owns the client socket, in both modes** (`devyre_responses_socket.go`). It handles each `response.interrupt` as it arrives and queues every other frame as before. A client that closes its socket mid-response, as Codex does on a stop, cancels the request at once, so the router closes the upstream socket, which ends the response. Without steering this used to wait until a later write to the client failed.
- **Forwarded while its response runs.** The session's upstream reader follows which response runs on its socket, from `response.created` to its terminal event (`devyre_codex_interrupt.go`). An interrupt naming that response is written to that socket, unchanged, on the credential that socket belongs to. Upstream answers it, and its acknowledgement and the response's end reach the client like any other event.
- **Otherwise answered here, never with an error, and never written upstream**, since between requests nothing reads upstream's answer for the client and it would arrive as the first event of the client's next request. An interrupt for a response that already ended on the socket, which is the race Codex is in when the response finishes as it decides to interrupt, gets `{"type":"response.interrupt.accepted","response_id":"..."}`: the response has stopped, all the interrupt asked for. One for a response the socket never ran, or before any response, gets `response.interrupt.failed` with `error.code: "response_not_in_progress"`. Codex ignores both while it drains a response; a client that waits for the acknowledgement keeps the socket on the first and retires it on the second.
- **A disabled credential.** Like `response.steer`, the interrupt is not written to an account that is no longer enabled, and the connection fails, so the client starts again on an enabled one.
- **The interrupted response's end.** `response.incomplete` now ends a response like `response.completed`; without steering it used to close the client socket. A response that ends without output after an interrupt this router forwarded passes through instead of becoming the empty-response failure.

With steering on, an interrupt that reaches the duplex's input is handled the same way, although the reader takes them first. A response from an HTTP (non-websocket) upstream cannot be interrupted: its interrupt gets `response.interrupt.failed` and the response runs to its end, which no longer fails the turn.

Evidence (2026-10-19, no account used): the real `codex app-server` 0.163.0, with `CODEX_HOME` in a temporary folder and a provider pointing at a router built before and after this change, against a local stand-in upstream (a Codex API-key credential with a `base-url` and `websockets: true`) that streams a 60-second answer and records every frame. `turn/steer` mid-answer: before, the interrupt never reached the stand-in, the answer streamed for 60 s and the turn failed with the 400 above; after, the stand-in received `{"type":"response.interrupt","response_id":"resp_2","mode":"discard_partial_items"}` in the same millisecond as the steer, the follow-up request came on the same socket, the turn completed 0.1 s after the steer, and the next turn used that socket too. `turn/interrupt` while the answer was silent: before, the router closed the upstream socket 20.4 s after Codex closed its own, when the stand-in next sent something; after, in the same millisecond.

## Embedded panel

The server answers `/management.html` from a copy of the panel built into the binary, `internal/managementasset/devyre_panel/management.html`: the release asset of [the panel fork](https://github.com/Devyre/Cli-Proxy-API-Management-Center), byte for byte. `panel.json` next to it records the version, the source and the sha256, and `go test ./internal/managementasset/` fails when the file and the record disagree.

- **Nothing is downloaded.** No release lookup, no fallback page, no update every 3 hours and no file under `static\`. Upstream's downloader stays in `updater.go`, so syncs merge cleanly, but it never runs. `disable-auto-update-panel`, `panel-github-repository` and GitHub tokens have no effect, and the panel fork can be private. A `management.html` that an older build left in `%USERPROFILE%\.cli-proxy-api\static\` is ignored and can be deleted.
- **Responses.** `Content-Type: text/html; charset=utf-8` and the anti-framing headers as before, plus the sha256 as a strong `ETag` with `Cache-Control: no-cache`: a browser revalidates and gets a 304 until the binary changes. At start the log names the panel, for example `management panel: the embedded v1.25.3-devyre.2 (sha256 71a9e04b...); never downloaded`.
- **Override.** When upstream's `MANAGEMENT_STATIC_PATH` names an existing file, or a folder holding `management.html`, that file is served instead, as it is. It is never downloaded or updated, and while it is missing the embedded panel answers. Use it to try a panel build without rebuilding the server. The deployment does not set it.
- **Without the panel.** `go build -tags devyre_nopanel ./cmd/server` leaves it out, about 3 MB less, for embedders that turn the control panel off, such as a desktop app's sidecar: `/management.html` then answers 404 (unless the override names a file). The default build and the Docker image include it. `disable-control-panel: true` answers 404 in either build.

**Refresh it** with `devyre\scripts\update-panel.ps1`, for the latest release, or `-Tag <tag>`. It downloads `management.html` with your own `gh` login, refuses it unless it matches the sha256 digest the release lists and is an HTML page, and rewrites `management.html` and `panel.json`. `-Clone <path>` builds a local clone of the panel fork instead, the way its release workflow does (`bun install --frozen-lockfile`, then `bun run build` with `VERSION` set to the clone's `git describe`). The clone must have no uncommitted changes and reach a `v*-devyre.*` tag; the record then names the commit. The script never commits: run the panel tests it prints, review the diff, commit with `feat(devyre): embed panel <version>`, and run `up.ps1`.

## Sync with upstream

1. Run `devyre\scripts\sync-upstream.ps1`. It requires a clean tree, fast-forwards `main` from `origin`, creates `sync/upstream-YYYYMMDD` and merges `upstream/main`. It then runs the checks: `gofmt -l`, `go build ./cmd/server` and `go build -tags devyre_nopanel ./cmd/server`, `go test ./...`, and `go test -tags devyre_nopanel ./internal/managementasset/ ./internal/api/ -run DevyrePanel`. It never pushes.
2. If the merge conflicts, resolve the conflicts at the hotspots above, keeping upstream's behavior plus our hunks. Commit the merge, then run the checks by hand. `go test ./internal/api/ -run T3Hub` must stay green.
3. Push the branch and open the PR against the fork, never against upstream:
   `gh pr create --repo patchforgeai/CLIProxyAPI --base main --head sync/upstream-YYYYMMDD`.
4. Merge the PR, tag `v<upstream version>-devyre.<n>`, and run `up.ps1`.

The panel fork syncs separately in its own repository: merge `upstream/main`, check `chhoumann/dev` for ledger updates, run `bun run verify`, then tag `v<upstream version>-devyre.<n>` and push the tag; its release workflow publishes `management.html`. Then embed that release here with `devyre\scripts\update-panel.ps1 -Tag <tag>`, commit, and run `up.ps1`.

## Rollback

- **Server image.** `docker images cpa-devyre` lists the builds. Run `docker tag cpa-devyre:<previous-sha> cpa-devyre:current`, then `devyre\scripts\up.ps1 -NoBuild`.
- **Routing only.** In the panel, set `routing.strategy` back to `round-robin`. It hot-reloads.
- **Panel.** Run `devyre\scripts\update-panel.ps1 -Tag <previous-tag>`, or `git revert` the commit that embedded the panel, then commit and run `up.ps1`. Rolling back the server image (above) also rolls back the panel built into it.
- **Clients.** Run `Disable-CpaPool`. In T3, use the "Claude Direct" instance or remove the proxy variables from the Claude instance.
- **Passwordless access only.** Run `devyre\scripts\tailnet-trust.ps1 -Disable`. Every request needs a key again at once; clients that send a real key keep working.

## Backup

Run `devyre\scripts\backup.ps1 -Destination <folder>`. It writes `config.yaml`, `auths\` and `secrets\` to a timestamped zip, and refuses destinations inside this checkout or the CPA home. The archive holds live OAuth tokens and keys: keep it on BitLocker-protected storage or encrypt it, for example with 7-Zip AES.

## Fork tests

```powershell
go test ./internal/api/ -run 'T3Hub|QuotaReadings|RoutingFocus|TailnetAuth|DevyreKeylessProxy|DevyreKeepAlive|DevyrePanel|LifelineProof|DevyreListenReport|OnlyWrongPasswordsCountTowardTheBan'
go test ./internal/cmd/ -run 'Lifeline|PasswordLine|ReportListen|TakenPort'
go test ./cmd/server/ -run 'LifelineStdin|PasswordStdin'
go test ./internal/managementasset/ -run DevyrePanel
go test -tags devyre_nopanel ./internal/managementasset/ ./internal/api/ -run DevyrePanel
go test ./internal/config/ -run 'DevyreDeployTemplate|ExpiringFirst|TailnetAuth|DevyreSession'
go test ./internal/quotareading/
go test ./internal/routingfocus/
go test ./internal/tailnetauth/
go test ./internal/api/handlers/management/ -run 'UsageCache|QuotaObserver|RoutingQuota|RoutingFocus|ForgetRemovedFocus|ExpiringFirst|RoutingStrategy|TailnetAuth|GetAuthSession|AuthenticateManagementKey|DevyreRedirect'
go test ./internal/auth/claude/ ./internal/auth/codex/ -run Devyre
go test ./sdk/cliproxy/auth/ -run 'ExpiringFirst|Focus|DevyreSession|DevyreTransient'
go test ./sdk/cliproxy/ -run 'ExpiringFirst|DevyreSession'
go test ./sdk/api/handlers/openai/ ./internal/runtime/executor/ -run Devyre
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

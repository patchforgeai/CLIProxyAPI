# Devyre CPA — implementation plan

Fork CLIProxyAPI and its management panel. Make the panel's Quota page look like Theo's (t3.gg) "CPAMC" ledger, but led by the account-wide weekly limit instead of Fable. Run it the way Theo does: an always-on box on the tailnet, session affinity and "expiring-first" routing. Plug it into T3 Code as a CLIProxyAPI hub, and point T3 Code's and Claude Code's Claude traffic at it so 3 Claude accounts (plus optional Codex and others) are pooled and load-balanced.

- **Status:** ready to implement. Written 2026-10-03 against pinned upstream commits (section 4).
- **Executor:** Claude Code "ultracode" workflow, starting from `Z:\Code\repos\Devyre\CLIProxyAPI`.
- **Reference frame:** `.reference/theo-quota-frame.png`, kept local and excluded from git.
- **Run it as:** `ultracode: execute PLAN.md`. Phase P0 moves this file to `devyre/PLAN.md`.

---

## 0. Summary

### What Theo runs

Established from the reference frame and the source code:

| Layer | Theo's setup | Upstream? |
|---|---|---|
| Panel shell | Logo, "CPAMC / CLI Proxy API Console", nav groups OPERATE / GATEWAY / OBSERVE / CONTROL, and every nav item | **Yes**, stock CPAMC v1.25.x |
| Quota page | "Ledger" view: provider summary strip with pooled %, dense per-credential rows, "Show emails", Ledger/Cards select | **No.** Theo's fork is private. A public MIT port calibrated against this exact frame exists: `chhoumann/Cli-Proxy-API-Management-Center@dev` |
| Server | CLIProxyAPI v8 with session affinity and expiring-first routing (burns the credential whose quota resets soonest) | Affinity yes, off by default. Expiring-first **no** |
| Hosting | Always-on box on the tailnet at `https://<host>.ts.net:8318` | n/a |
| T3 Code | "CLIProxyAPI hub" for Usage → Limits, plus Claude provider env vars pointing at the proxy | Built into T3 Code |

### What we build

1. **Panel fork** (`Devyre/Cli-Proxy-API-Management-Center`): upstream v1.25.3 plus chhoumann's ledger, pinned. Our changes on top:
   - The Claude headline is the account-wide **7-day limit**, which Opus 5.5 consumes.
   - The secondary pool is the **5-hour** window.
   - **Fable is hidden** in the ledger but stays in Cards.
   - Dark theme is the default.
   - Adds an `expiring-first` routing option, a cache-aware refresh and an optional "next up" hint.
2. **Server fork** (`Devyre/CLIProxyAPI`): upstream v8.0.13 plus:
   - The panel is served from our fork's releases.
   - Contract tests pin the `/v0` endpoints T3's hub calls.
   - A usage cache in `api-call`.
   - Quota readings and an idle-credential usage poller.
   - The `expiring-first` strategy and a readings endpoint.
   - A deploy kit.
3. **Deploy** on this PC: Docker Desktop on loopback only, with `tailscale serve --https=8318` in front.
4. **Wire clients:**
   - T3 Code: hub plus a pooled Claude instance.
   - Claude Code CLI: PowerShell profile.
   - Optional: Codex and "claudex".

### Milestones

- **M1, "looks and works like Theo":** the UI fork is released and the server fork is deployed with round-robin plus affinity. 3 Claude accounts are pooled, the T3 hub is added and T3's Claude goes through the pool.
- **M2, "routes like Theo":** usage cache, quota readings, poller and `expiring-first` are live.
- **M3, polish:** "next up" hints in the ledger and upstream-sync automation.

### What only the user can do (HITL gates)

Pause and ask at each of these:

1. Approve creating the forks and installing toolchains (P0).
2. Enable HTTPS certificates in the Tailscale admin console (DEP-1).
3. Save the generated management key in a password manager (DEP-2).
4. Log in the 3 Claude accounts through the panel's OAuth page, one browser profile per account (DEP-8).
5. Enter the T3 Code settings: add the hub and set the Claude instance's environment (CLI-1, CLI-2).
6. Approve the PowerShell profile change (CLI-3).

---

## 1. Verified facts (do not re-research)

| # | Fact | Evidence |
|---|---|---|
| F1 | Theo's sidebar and header are stock upstream CPAMC: `title.abbr="CPAMC"`, `sidebar.subtitle="CLI Proxy API Console"`, groups `nav_groups.{operate,gateway,observe,control}`. The items are Dashboard, Quick Start, AI Providers, Auth Files, OAuth Login, Quota Management, Logs Viewer, Config Panel, Plugins, Plugin Store and "Management Center Info", which renders truncated | panel `src/i18n/locales/en.json`, `src/components/layout/MainLayout.tsx:595-700`, `src/router/MainRoutes.tsx` |
| F2 | Upstream never had a Ledger view, "Show emails" or the Ledger/Cards select. History and PRs were searched | `git log -S`, `gh pr list --search ledger` |
| F3 | `chhoumann/Cli-Proxy-API-Management-Center` (MIT) `dev` @ `24633a8` is upstream `main` @ `ee79a79` (v1.25.3) plus 8 commits that implement exactly this. PR #2: "Ledger layout modeled on the dashboard in Theo's setup video", verified by a calibrated overlay on the 4K frame and 1514 passing tests. PR #3: the Claude headline is the account-wide weekly window. Masking produces `claude-t•••@l•••.dev.json` | chhoumann PR #2, PR #3 |
| F4 | Upstream's "7-day Fable 5" label is Claude's model-scoped weekly window: `limits[]` entries with `kind:"weekly_scoped"` and `scope.model.display_name:"Fable 5"`, legacy key `iguana_necktie`, window id `seven-day-fable`. `seven_day` is "7-day limit" (`seven-day`) and `five_hour` is "5-hour limit" (`five-hour`) | `src/features/quota/providers/claude/data.ts`, `src/utils/quota/constants.ts:119-131` |
| F5 | Ledger numbers are **remaining**, not used. Theo's "409% of 500%" is the sum over 5 accounts | ledger.ts, and the frame's arithmetic checks out |
| F6 | **T3 Code hub contract.** <br>• Where: Settings → Providers → Usage providers → Add hub, entering URL and management key for one environment. <br>• Implementation: `apps/server/src/usage/cliproxyApi.ts`. <br>• Calls `GET /v0/management/auth-files`. <br>• Calls `POST /v0/management/api-call`. For Claude: `GET https://api.anthropic.com/api/oauth/usage` with `anthropic-beta: oauth-2025-04-20`. For Codex: `GET …/wham/usage`, `GET …/wham/rate-limit-reset-credits`, `POST …/consume`. <br>• Calls `POST /v0/management/reset-quota {auth_index}`. <br>• Auth: `Authorization: Bearer <plaintext key>`. 15 s timeout, concurrency 4. <br>• Only `provider` values `claude` and `codex` are read; disabled files are skipped. <br>• Display only: the docs say "configure the provider separately to send agent requests through the hub". <br>• Auto-checks at most every 5 min per environment | T3 source and `docs/user/usage.md` |
| F7 | Server v8.0.13 still registers `/v0/management/*` next to `/v8/management/*`, but its AGENTS.md marks v0 as deprecated. `POST /v0/management/api-call` and `POST /v8/management/requests/api-call` share one handler, `APICall` | `internal/api/server_management.go:28-188`, `server_management_v8.go:31` |
| F8 | Strategies: `round-robin` (default), `weighted-round-robin` and `fill-first`. Session affinity is off by default. No strategy reads quota. Headers are captured into `Auth.Quota.Signals` but never used for selection: `Anthropic-Ratelimit-Unified-{5h,7d,7d_oi}-{Utilization,Reset,Status}`, where utilization is a 0..1 fraction and reset is unix seconds, and `X-Codex-{Primary,Secondary}-{Used-Percent,Reset-At,Reset-After-Seconds,Window-Minutes}` | `sdk/cliproxy/auth/selector.go`, `quota_signals.go`, `internal/runtime/executor/helps/claude_ratelimit.go` |
| F9 | Scheduler plugins can't see quota data or affinity bindings, can't stack, and are skipped in Home mode. Expiring-first therefore belongs in a core selector | bandoyer/CLIProxyAPI #4, #10; `sdk/pluginapi/types.go:486-543` |
| F10 | Theo routes "expiring first", keeps threads on one account (the prompt cache is per account), uses WebSockets for Codex and runs at home behind Tailscale. He warns against serving public traffic | video "If you have a Claude sub, watch this" (summary) |
| F11 | Claude's `/api/oauth/usage` is tightly rate-limited: 429s reported at 30-60 s and sometimes at 10 min, with `retry-after` missing. Its `utilization` is a **percent** (0..100), unlike the header fraction. Codex `/wham/usage` tolerates 60 s | anthropics/claude-code#30930, #31637 via bandoyer #13 |
| F12 | Panel download: `management.panel-github-repository` points at GitHub `releases/latest`, the asset is `management.html` and its digest is verified. It is checked at start and every 3 h. The fallback `https://cpamc.router-for.me/` is used **only when the local file is missing**. `MANAGEMENT_STATIC_PATH` or `WRITABLE_PATH` relocate it | `internal/managementasset/updater.go` |
| F13 | Management auth: <br>• A plaintext `secret-key` is hashed in place on first start. <br>• 5 failures from one client IP ban that IP for 30 min. <br>• Any non-loopback client needs `allow-remote: true`. <br>• `MANAGEMENT_PASSWORD` silently forces remote access on, so don't use it | `internal/api/handlers/management/handler.go:263-340`, bandoyer #7 |
| F14 | OAuth callbacks: Claude redirects to `http://localhost:54545/callback`. The panel starts a forwarder on `0.0.0.0:54545` that 302s to the management callback. Codex uses 1455. A browser on another device uses "Submit Callback URL" | `auth_files_oauth_callback.go:18,40-75` |
| F15 | Claude Code through a proxy: <br>• Set `ANTHROPIC_BASE_URL` (no `/v1`) and `ANTHROPIC_AUTH_TOKEN`. Don't use `ANTHROPIC_API_KEY`, which prompts. <br>• Token auth turns off claude.ai-only features: Remote Control, claude.ai MCP connectors and `/usage`. <br>• Token auth drops the prompt-cache TTL to 5 min unless `ENABLE_PROMPT_CACHING_1H=1`. <br>• It also turns tool search off unless `ENABLE_TOOL_SEARCH=true`. This last point was read from the Claude Code 2.1.287 binary by bandoyer, so **verify**. <br>• T3 docs: put router env in the instance's Environment variables, mark the token Sensitive and set `ANTHROPIC_API_KEY` explicitly empty | bandoyer #15, #18; T3 `docs/user/providers-claude.md` |
| F16 | This machine: <br>• Windows 11 Pro, PowerShell 5.1, profile `Z:\Documents\WindowsPowerShell\Microsoft.PowerShell_profile.ps1`. <br>• Docker 29.8; Desktop is installed but not running. <br>• Tailscale 1.102.4, logged in; `CertDomains` is empty, so tailnet HTTPS is **not enabled**. <br>• TZ is Pacific. <br>• No Go, Node or Bun. `git core.autocrlf=true` (system). <br>• `claude` is at `C:\Users\Devyre\.local\bin\claude.exe`. <br>• `gh` is authenticated as **Devyre** with `repo` and `workflow` scopes | local probes |
| F17 | Upstream `.gitignore` ignores `docs/*`, `config.yaml`, `.env`, `auths/*`, `static/*` and `plugins/*` at any depth. Fork-specific files therefore live in a new top-level `devyre/` folder | upstream `.gitignore` |

---

## 2. Decisions (do not relitigate)

| # | Decision | Why |
|---|---|---|
| D1 | Two forks shaped like upstream: the server, and the panel shipped as a GitHub Release `management.html`. The server pulls the panel from **our** release | Upstream syncs stay cheap, and it's the mechanism the server already has |
| D2 | Panel = upstream `main` merged with `chhoumann/dev@24633a8` as a real merge, so authorship is kept. Credit it in README | MIT, already calibrated against Theo's frame, 1500+ tests. Don't rebuild it |
| D3 | **Claude ledger, per the user:** <br>• Headline = `seven-day` ("7-day limit"). <br>• Secondary pooled line = `five-hour`. <br>• Row columns = 7-day, 5-hour, then any other reported model windows. <br>• `seven-day-fable` is **hidden in Ledger**, still visible in Cards, and shown only if it is an account's only window | "Opus 5.5 is great, we don't really use Fable right now" |
| D4 | Default theme `dark`. Everything else in the shell stays upstream | That *is* Theo's look (F1) |
| D5 | New built-in strategy `expiring-first`, a core selector wrapped by `SessionAffinitySelector`. <br>• Urgency = remaining % of the credential's longest window ÷ hours until that window resets. <br>• Short windows (≤ 24 h) and model-scoped windows only gate. Long windows rank, and also gate once exhausted. <br>• Unknowns rank last, round-robin among themselves. <br>• Priority tiers keep their meaning. <br>• **Bound threads never migrate for urgency.** They move only when the credential becomes unavailable or the affinity TTL (1 h) lapses | Matches Theo (F10) and bandoyer's analysis (#8, #10). A migration rewrites the whole cached prefix (about $0.50-0.80 per 100k tokens at API rates), which outweighs the ordering gain |
| D6 | One in-memory **quota-readings** store fed from three sources: <br>• Response headers, already in `Auth.Quota.Signals`. <br>• Every usage body that passes through `api-call` (panel and T3 hub). <br>• An idle-credential poller. <br>The newest reading per window wins | Covers idle credentials without extra traffic |
| D7 | **Usage cache** inside `APICall`, for allowlisted GET usage and profile URLs only: <br>• TTL: Claude 5 min, Codex 60 s, profile 1 h. <br>• Single-flight per key. <br>• Serves stale data on error. <br>• Claude 429 backoff: 5 min doubling to 1 h. <br>• Explicit bypass header with a 30 s floor | Three consumers (T3 hub, panel, poller) must not trip Claude's limiter (F11) |
| D8 | Keep the T3 hub's `/v0` endpoints and pin them with contract tests. If upstream deletes them, add a `devyre_v0_shim.go` | F6 and F7 |
| D9 | Host on this PC with Docker Compose, published on 127.0.0.1 only. Expose with `tailscale serve --bg --https=8318`. Port 8318 is Theo's; T3's own `tailscale serve` uses 443, so they don't clash. Moving to a Linux box later uses the same compose file | Docker is installed and gives the closest-to-upstream runtime |
| D10 | Secrets: a management key and one API key per client (`t3-code`, `claude-code-cli`, `codex-cli`, `other-devices`), generated locally and stored under `%USERPROFILE%\.cli-proxy-api\secrets\` (user-only ACL) and in a password manager. Never in git | Per-client usage attribution, plus revocation |
| D11 | Clients are pooled by default, with a one-step bypass: `claude-direct` and a "Claude Direct" T3 instance | Same as Theo and bandoyer #15 |
| D12 | Out of scope: <br>• PRs, issues or syncs to `router-for-me` or `chhoumann`. <br>• Forking T3 Code. <br>• Tailscale Funnel and public exposure. <br>• Re-creating Theo's private code byte for byte | Safety and focus |

---

## 3. Architecture

```
 tailnet devices (this PC, laptop, phone)
   │  https://<machine>.<tailnet>.ts.net:8318      tailscale serve (TLS; adds X-Forwarded-For)
   ▼
 Windows host 127.0.0.1:8317 ── Docker Desktop publish ──▶ container "cpa" (Devyre/CLIProxyAPI)
                                                          ├─ /management.html            ← Devyre panel release (verified digest)
                                                          ├─ /v1/messages, /v1/responses, /v1/chat/completions, /v1/models
                                                          ├─ /v8/management/*            ← panel
                                                          ├─ /v0/management/{auth-files,api-call,reset-quota}  ← T3 Code hub
                                                          ├─ routing: SessionAffinity( ExpiringFirst )
                                                          ├─ quota readings ◀── headers │ api-call usage cache │ idle poller
                                                          └─ /data/auths: claude-*.json ×3 (+ codex/xai/… optional)
 T3 Code (this PC)
   ├─ Usage → Limits  ⇐ hub (URL + management key) → /v0/management/*
   └─ Claude instance env ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN → /v1/messages (pooled)
 Claude Code CLI  — PowerShell profile env → /v1/messages (pooled); `claude-direct` bypasses
```

---

## 4. Layout, names, pins

```
Z:\Code\repos\Devyre\
  CLIProxyAPI\                          server fork (origin = Devyre/CLIProxyAPI, upstream = router-for-me/CLIProxyAPI)
    devyre\                             every fork-only file (never ignored by upstream rules)
      PLAN.md                           this plan (moved here in P0-2)
      README.md                         fork notes: what differs, run, sync, conflict hotspots
      deploy\docker-compose.yml
      deploy\config.template.yaml
      deploy\.env.example               (.env itself is gitignored)
      scripts\new-secrets.ps1  up.ps1  tailscale-serve.ps1  backup.ps1  sync-upstream.ps1
      clients\powershell-profile.ps1    clients\t3-code.md    clients\codex-config.toml
    .reference\theo-quota-frame.png     local only (.git/info/exclude)
  Cli-Proxy-API-Management-Center\      panel fork (origin = Devyre/…, upstream = router-for-me/…, chhoumann = ledger source)
%USERPROFILE%\.cli-proxy-api\           runtime, never in git → mounted at /data in the container
  config.yaml  auths\  logs\  static\  plugins\  secrets\
```

The panel's AGENTS.md expects the backend at `../CLIProxyAPI`, and this layout satisfies it.

| Item | Value |
|---|---|
| Upstream server pin | `d7914afdedca7af95ee974a42453dc49fc1388ce` (= v8.0.13) |
| Upstream panel pin | `ee79a794526a30c03748a8864a9ac6589a31833b` (= v1.25.3) |
| Ledger source pin | `24633a862049c75c510bbbaad2bc14bfa97edc92` (`chhoumann/dev`) |
| Panel release tags | `v1.25.3-devyre.N`. Always `<upstream version>-devyre.<n>` |
| Server tags | `v8.0.13-devyre.N`, git tag only. The image is built locally as `cpa-devyre:current` and `cpa-devyre:<sha>` |
| Public URL | `https://<machine>.<tailnet>.ts.net:8318`. Get it from `tailscale status --json` → `Self.DNSName`. Keep it out of git; put it only in `devyre/deploy/.env` |

If upstream moved past the pins when you start, use the new HEADs. The ledger merge (UI-1) may then need conflict resolution; see the guidance there.

---

## 5. Rules of engagement for implementers

1. **Never** push, open PRs or issues, or comment against `router-for-me/*` or `chhoumann/*`. In each clone run `gh repo set-default Devyre/<repo>`, and always pass `--repo Devyre/<repo>` to `gh pr create`. A fork's `gh pr create` defaults to the *parent*.
2. Never commit secrets, auth files, `config.yaml`, real emails, the tailnet hostname, or unmasked screenshots.
3. Follow each repo's `AGENTS.md`.
   - **Server:** gofmt; `go build ./cmd/server` after every change; English comments; no `log.Fatal`; controllable clocks in TTL and ordering tests; no new timeouts after an upstream connection is established (the APICall timeout is the allowed exception).
   - **Panel:** Bun 1.3.14; i18n in all **4** locales (en, zh-CN, zh-TW, ru); tests in `tests/` using `bun:test`; `bun run verify` before handoff.
4. Keep the fork diff isolated:
   - New Go files are named `devyre_*.go` or live in new packages.
   - Edits to upstream files are minimal hunks, with a `// devyre:` comment where the reason isn't obvious.
5. Don't touch `/v0/management` handlers except the shared `APICall` internals.
6. Branches are `devyre/<topic>`. Open a PR into the fork's `main` (`gh pr create --repo Devyre/… --base main`), self-merge once tests pass, and use Conventional Commits.
7. Clone with `core.autocrlf=false` and `core.eol=lf` (F16). Otherwise gofmt and Prettier flag every file.
8. At every HITL step, stop and ask the user with a concrete instruction, then continue.

---

## 6. Execution graph

| ID | Task | Depends on | Can run with | HITL |
|---|---|---|---|---|
| P0-1…P0-7 | Bootstrap: forks, clones, remotes, toolchains, Docker | — | — | approve forks + installs |
| UI-1 | Merge ledger | P0 | SRV-*, RT-1/2 | — |
| UI-2 | Claude 7-day headline, Fable hidden | UI-1 | UI-3, UI-4 | — |
| UI-3 | Dark default | UI-1 | UI-2 | — |
| UI-4 | Visual harness + Theo fixture | UI-2, UI-3 | SRV-* | — |
| UI-6a | Release `v1.25.3-devyre.1` (M1) | UI-2…UI-4 | — | — |
| SRV-1 | Panel source → fork | P0 | UI-* | — |
| SRV-2 | T3 hub contract tests | P0 | SRV-1 | — |
| SRV-3 | Deploy kit | P0 | SRV-1/2 | — |
| SRV-4 | Fork README | SRV-1…3 | — | — |
| DEP-1…DEP-9 | Deploy M1 | UI-6a, SRV-1…3 | RT-* | Tailscale HTTPS, key storage, OAuth ×3 |
| CLI-1…CLI-5 | Wire clients | DEP-8 | RT-* | T3 settings, profile |
| RT-1 | Readings model + parsers | P0 | RT-2 | — |
| RT-2 | Readings store | RT-1 | RT-3 | — |
| RT-3 | Usage cache in APICall | RT-2 | RT-5 | — |
| RT-4 | Idle poller | RT-2, RT-3 | RT-5 | — |
| RT-5 | `expiring-first` selector + config wiring | RT-2 | RT-3, RT-4 | — |
| RT-6 | Readings endpoint | RT-5 | UI-5 | — |
| UI-5 | Strategy option, cache bypass, "next up" | RT-3, RT-6 (API in this plan) | — | — |
| UI-6b | Release `v1.25.3-devyre.2` (M2) | UI-5 | — | — |
| DEP-10 | Redeploy with expiring-first (M2) | RT-*, UI-6b | — | — |
| E2E | Section 13 checklist | everything | — | final review with user |

---

## 7. P0 — Bootstrap

**P0-1, create forks.** This is outward-facing; the user asked for it.

```powershell
gh repo fork router-for-me/CLIProxyAPI --clone=false --default-branch-only
gh repo fork router-for-me/Cli-Proxy-API-Management-Center --clone=false --default-branch-only
gh repo view Devyre/CLIProxyAPI --json parent --jq .parent.nameWithOwner                       # router-for-me/CLIProxyAPI
gh repo view Devyre/Cli-Proxy-API-Management-Center --json parent --jq .parent.nameWithOwner   # router-for-me/Cli-Proxy-API-Management-Center
```

**P0-2, server fork into this folder.** The folder already holds `PLAN.md` and `.reference/`, so initialize in place instead of cloning:

```powershell
Set-Location Z:\Code\repos\Devyre\CLIProxyAPI
git init -b main
git config core.autocrlf false; git config core.eol lf; git config core.longpaths true
git remote add origin   https://github.com/Devyre/CLIProxyAPI.git
git remote add upstream https://github.com/router-for-me/CLIProxyAPI.git
git fetch origin; git fetch upstream --tags
git checkout -B main origin/main
git branch --set-upstream-to=origin/main main
Add-Content .git\info\exclude ".reference/"
New-Item -ItemType Directory devyre | Out-Null
Move-Item PLAN.md devyre\PLAN.md
git add devyre/PLAN.md; git commit -m "docs(devyre): add implementation plan"
git push origin main
gh repo set-default Devyre/CLIProxyAPI
```

**P0-3, panel fork.**

```powershell
Set-Location Z:\Code\repos\Devyre
git clone -c core.autocrlf=false -c core.eol=lf https://github.com/Devyre/Cli-Proxy-API-Management-Center.git
Set-Location Cli-Proxy-API-Management-Center
git remote add upstream  https://github.com/router-for-me/Cli-Proxy-API-Management-Center.git
git remote add chhoumann https://github.com/chhoumann/Cli-Proxy-API-Management-Center.git
git fetch --all --tags
gh repo set-default Devyre/Cli-Proxy-API-Management-Center
```

**P0-4, Actions.**
- Enable Actions on the panel fork; its `release.yml` builds `management.html` when a `v*` tag is pushed:
  `gh api -X PUT repos/Devyre/Cli-Proxy-API-Management-Center/actions/permissions -F enabled=true -f allowed_actions=all`.
- Leave Actions **disabled** on the server fork. Its workflows push to Docker Hub and upstream release channels:
  `gh api -X PUT repos/Devyre/CLIProxyAPI/actions/permissions -F enabled=false`.

**P0-5, toolchains (HITL: approve installs).**

```powershell
winget install -e --id GoLang.Go                                   # needs go >= 1.26.0 (go.mod)
powershell -c "& ([scriptblock]::Create((irm https://bun.sh/install.ps1))) -Version 1.3.14"   # panel pins bun@1.3.14
go version; bun --version
```

Fallback with no host installs: run builds in containers, for example
`docker run --rm -v ${PWD}:/src -w /src golang:1.26-bookworm go test ./...` and
`docker run --rm -v ${PWD}:/app -w /app oven/bun:1.3.14 bun run verify`.

**P0-6, Docker Desktop.** Start it. In Settings → General, enable "Start Docker Desktop when you sign in". Check with `docker info --format '{{.ServerVersion}}'`.

**P0-7, reference frame.** It is already at `.reference/theo-quota-frame.png` (1770×990 at 90% browser zoom, i.e. about a 1967×1100 CSS viewport). It stays local.

**Done when:** both forks exist, both clones have the 3 remotes, `go version` ≥ 1.26 and `bun --version` = 1.3.14 (or the container fallback works), and `docker info` succeeds.

---

## 8. UI track — panel fork (`Z:\Code\repos\Devyre\Cli-Proxy-API-Management-Center`)

### UI-1 Merge the ledger

```powershell
git switch -c devyre/ledger main
git merge --no-ff 24633a862049c75c510bbbaad2bc14bfa97edc92 -m "merge: quota ledger from chhoumann/Cli-Proxy-API-Management-Center@24633a8 (MIT)"
bun install --frozen-lockfile
bun run verify
```

- **If upstream `main` moved:** conflicts land in `src/features/quota/*`, `src/i18n/locales/*` and `tests/quota*`. Keep upstream's new behavior and re-apply the ledger on top. Keys in the 4 locale files are additive.
- **README:** add a "Fork notes" section that credits router-for-me (MIT) and chhoumann's ledger (MIT, PR #2 and #3).
- **Done when:** `bun run verify` is green, with about 1514 or more tests.

What the merge brings (don't rebuild it):

| Piece | Where |
|---|---|
| Ledger view model: remaining-% readers per provider, `summarizeLedger`, `ledgerColumns`, `headlineRemaining`, `maskCredentialName` | `src/features/quota/ledger.ts` |
| Summary strip, rows, manual resets, Refresh/Reset actions | `src/features/quota/components/QuotaLedger.tsx` and `.module.scss` |
| Header: search icon, "Show emails", ink "Refresh all credentials" pill | `QuotaHeader.tsx` |
| Ledger/Cards select (persisted) and sort select; "Most remaining" sort | `QuotaPage.tsx`, `uiState.ts`, `logic.ts`, `constants.ts` |
| Tests | `tests/quotaLedger.test.ts`, `tests/quotaPageLogic.test.ts`, `tests/quotaUiState.test.ts` |

### UI-2 Claude headline = 7-day limit; hide Fable (user requirement)

Edit `src/features/quota/ledger.ts`:

1. Change the summary windows. The headline comes first; the secondary line renders as "5-hour limit N% · Show":
   ```ts
   SUMMARY_WINDOW_IDS.claude = ['seven-day', 'five-hour'];
   ```
2. Add a ledger-only visibility list and a column order:
   ```ts
   /** Windows the ledger hides (the Cards view still shows them). Fable is unused here; Opus 5.5 draws on the account-wide 7-day window. */
   export const LEDGER_HIDDEN_WINDOW_IDS: Partial<Record<QuotaProviderType, readonly string[]>> = {
     claude: ['seven-day-fable'],
   };
   /** Explicit column order; ids not listed keep first-seen order after these. */
   export const LEDGER_COLUMN_ORDER: Partial<Record<QuotaProviderType, readonly string[]>> = {
     claude: ['seven-day', 'five-hour', 'seven-day-opus', 'seven-day-sonnet', 'seven-day-oauth-apps', 'seven-day-cowork'],
   };
   ```
3. In `ledgerMeters(type, quota, t)`, filter out `LEDGER_HIDDEN_WINDOW_IDS[type]`. **Fallback:** if filtering would empty a non-empty list, because the account reports only Fable, return the unfiltered list so no row renders blank.
4. In `ledgerColumns(accounts, summary)`, add a `type` parameter and update the one caller in `QuotaLedger.tsx`. When `LEDGER_COLUMN_ORDER[type]` exists, sort ids by their index in that list, with unknown ids after in first-seen order. Otherwise keep chhoumann's ordering. Without this, the secondary summed `five-hour` would sort **last**.
5. Leave `headlineRemaining` alone. It already uses `SUMMARY_WINDOW_IDS` order, so "Most remaining" sorts by the 7-day window.

Tests, in `tests/quotaLedger.test.ts`:
- Claude summary ids are `['seven-day','five-hour']`, and the headline label key is `claude_quota.seven_day`.
- Fable is excluded from meters and columns when other windows exist, and present when it is the only window.
- With Opus present, the column order is `['seven-day','five-hour','seven-day-opus']`.
- The Theo-frame fixture (Appendix G) gives a headline of 454 / capacity 500 with segments `[79,100,100,75,100]`, and a secondary `five-hour` of 499.

**Done when:** `bun run verify` is green. On the fixture, the Claude card reads **"7-day limit · 454% of 500%"** with **"5-hour limit 499% Show"**, rows show `7-day limit | 5-hour limit`, and no Fable column appears.

### UI-3 Dark by default

In `src/stores/useThemeStore.ts`, change the persisted initial `theme: 'auto'` to `theme: 'dark'`. A user's saved choice still wins, and "auto" stays selectable. Update any test that asserts the default.

**Done when:** a fresh browser profile opens the panel dark. Its tokens are `[data-theme='dark']` in `src/styles/themes.scss`: page `#151412`, cards `#1d1b18`.

### UI-4 Visual verification harness (dev-only; never shipped)

- `tests/fixtures/theoFrame.ts`: the Appendix G data.
  - 11 auth files: 5 Claude, 3 Codex, 1 xAI, 1 Kimi, plus 1 non-quota file so the sidebar badge reads 11.
  - Use synthetic emails, e.g. `claude-taylor@lumen.dev.json`, which masks to `claude-t•••@l•••.dev.json`.
  - Usage payloads per provider, using the real upstream shapes. Copy the exact shapes from `src/features/quota/providers/*/data.ts` parsers.
  - A frozen clock of 2026-09-11 20:50 America/Los_Angeles.
- `scripts/visual/quota-frame.spec.ts` (Playwright):
  1. Build with `bun run build`, then serve `dist/` with `bunx vite preview --host 0.0.0.0 --port 4173`. `--host` lets the Playwright container reach it; stop it after the run.
  2. In the test, intercept `**/v8/management/**` with `page.route`:
     - `requests/api-call` answers from the fixture by `{url, authIndex}`.
     - `auth-files` returns the list.
     - Anything else gets `200 {}` (extend if a page errors).
     - Echo `X-CPA-VERSION` and `X-CPA-SUPPORT-PLUGIN: true` headers so the plugin nav items render as in Theo's sidebar.
  3. Log in through the form with any key.
  4. Use `timezoneId: 'America/Los_Angeles'` and a 1967×1100 viewport.
  5. Screenshot `#/quota` in Ledger, with emails masked and with emails shown.
- Make `scripts/visual/` a self-contained Playwright project with its own `package.json` pinning `@playwright/test`, kept outside the Bun install.
  - Add `scripts/visual/**` to the `ignores` in `eslint.config.js` so `bun run lint` doesn't type-check it. The root `tsconfig.json` already covers only `src`.
  - Exclude `scripts/visual/node_modules/` and `scripts/visual/__out__/` via `.git/info/exclude`.
- Run Playwright in Docker so the host needs no Node:
  `docker run --rm --ipc=host -v ${PWD}:/work -w /work/scripts/visual mcr.microsoft.com/playwright:<tag matching the pinned @playwright/test> sh -c "npm ci && npx playwright test"`.
  From the container, the preview server is `http://host.docker.internal:4173`. If the session has the Claude-in-Chrome tools, they are an acceptable alternative.
- **Done when:**
  - The screenshot matches `.reference/theo-quota-frame.png` region by region: sidebar, title and meta line, Show emails / Refresh pills, provider tabs with counts, Ledger select, the 4 summary cells, the "Claude 5" group header and the rows.
  - The **only** intended differences are the Claude labels and columns from UI-2, plus the search icon chhoumann documents.
  - There are no page errors, no horizontal overflow at 1600 px or 390 px, and no unmasked email in the masked shot.
  - Screenshots go in `scripts/visual/__out__/`, which is gitignored via `.git/info/exclude`.

### UI-5 M2 features (after RT; the API contract is fixed in section 10)

1. **Strategy option `expiring-first`:**
   - `src/types/visualConfig.ts`: add it to `RoutingStrategy`.
   - `src/hooks/useVisualConfig.ts`: normalize `expiring-first | expiringfirst | ef | soonest-reset`.
   - `src/features/config/components/sections/SectionNetwork.tsx`: add the option and hint.
   - `src/features/config/searchIndex.ts`: add keywords.
   - `src/features/dashboard/DashboardPage.tsx`: add the label.
   - i18n keys in all 4 locales: `basic_settings.routing_strategy_expiring_first` = "Expiring first" and `…_desc` = "Prefer the credential whose quota would be lost soonest (remaining ÷ hours to reset). Threads stay on their credential."
2. **Cache bypass on explicit refresh.**
   - User-initiated refresh paths (the per-row "Refresh quota" and "Refresh all credentials" in `src/features/quota/hooks/useQuotaActions.ts` and the provider `fetchQuota` calls they trigger) pass `{ headers: { 'X-CPA-Usage-Cache': 'refresh' } }` to `apiCallApi.request(payload, config)`.
   - Background loads don't. Thread a `bypassCache` flag through the provider `fetchQuota(file, t, opts?)` signature; keep it optional so other providers compile.
3. **"Next up" hint, optional (M3).**
   - When `GET /v8/management/routing/quota-readings` returns 200 with `strategy: "expiring-first"`, the rank-1 credential per provider shows a small muted chip after its plan text: `quota_management.ledger_next_up` = "Next up".
   - Its tooltip shows `quota_management.ledger_urgency` = "{{rate}}%/h until reset".
   - A 404, or any other strategy, hides it. Keep the visual weight minimal so the layout still matches Theo's.
   - Add a pure-logic test for the rank lookup.

**Done when:** `bun run verify` is green and the config panel round-trips `routing.strategy: expiring-first`. Load the config, save it unchanged, and confirm the server's config still contains it plus the `routing.expiring-first` and `routing.quota-observation` blocks.

### UI-6 Releases

1. Merge `devyre/*` into `main` via a PR to the fork.
2. Tag and push:
   ```powershell
   git tag v1.25.3-devyre.1; git push origin main --tags
   ```
3. Check that `gh release view v1.25.3-devyre.1 --repo Devyre/Cli-Proxy-API-Management-Center --json assets --jq '.assets[].name'` lists `management.html`.

Release `-devyre.1` at M1 after UI-1…UI-4, and `-devyre.2` at M2 after UI-5. The server picks up a new release at its next start or within 3 h. Upstream's `release.yml` publishes non-prerelease, which "latest" requires.

---

## 9. SRV track — server fork basics (`Z:\Code\repos\Devyre\CLIProxyAPI`)

### SRV-1 Serve the panel from our fork

- In `internal/managementasset/updater.go`:
  ```go
  defaultManagementReleaseURL  = "https://api.github.com/repos/Devyre/Cli-Proxy-API-Management-Center/releases/latest"
  defaultManagementFallbackURL = "https://github.com/Devyre/Cli-Proxy-API-Management-Center/releases/latest/download/management.html" // devyre: never fall back to the stock panel
  ```
- In `config.example.yaml`, set `management.panel-github-repository` to the fork URL.
- Grep `Cli-Proxy-API-Management-Center` and `cpamc.router-for.me` across `*.go` and update any other default consistently. Adjust `updater_test.go` if it pins the constants.
- **Done when:** `go test ./internal/managementasset/...` passes. With an empty static dir, a started server logs `management asset updated successfully` from the Devyre release.

### SRV-2 Pin the T3 hub contract

Add `internal/api/devyre_t3_hub_contract_test.go`. Reuse the harness patterns in `internal/api/handlers/management/api_tools_test.go` and the server tests that build the gin engine with a management key. Assert exactly what T3 decodes (F6):

1. `GET /v0/management/auth-files` with `Authorization: Bearer <key>` → 200 with `{"files":[…]}`. Each file has:
   - `id`: string
   - `auth_index`: **string**
   - `provider`: `"claude"` or `"codex"` for those types
   - `email`: string, if known
   - `disabled`: bool
   - `id_token.chatgpt_account_id` and `id_token.chatgpt_plan_type`: strings, when present, for Codex

   Use a fake Claude auth and a fake Codex auth registered in the manager.
2. `POST /v0/management/api-call` with `{auth_index, method:"GET", url:<httptest URL>, header:{"Authorization":"Bearer $TOKEN$"}}` → 200 with `{"status_code":200,"header":{…},"body":"<string>"}`, and the upstream sees the substituted token.
3. `POST /v0/management/reset-quota` with `{"auth_index":…}` → 2xx JSON.
4. A wrong key returns 401 or 403, and no ban is triggered for fewer than 5 failures.
5. `POST /v8/management/requests/api-call` behaves identically, since the panel uses it.

Name the tests `TestT3Hub_*`. If an upstream sync deletes v0 routes, these fail first; restore the three routes in `internal/api/devyre_v0_shim.go`.

**Done when:** `go test ./internal/api/ -run T3Hub` is green.

### SRV-3 Deploy kit

Create `devyre/deploy/docker-compose.yml` (Appendix B), `devyre/deploy/config.template.yaml` (Appendix A), `devyre/deploy/.env.example` (Appendix C), `devyre/scripts/*.ps1` (Appendix D) and `devyre/clients/*` (Appendix E and F).

**Done when:** every `.ps1` parses without executing. Don't dot-source them; `new-secrets.ps1` would generate real secrets. Check with:

```powershell
Get-ChildItem devyre\scripts\*.ps1, devyre\clients\*.ps1 | ForEach-Object {
  $errs = $null; [void][System.Management.Automation.Language.Parser]::ParseFile($_.FullName, [ref]$null, [ref]$errs)
  if ($errs) { throw "$($_.Name): $($errs[0].Message)" }
}
```

The compose file also validates: `docker compose -f devyre\deploy\docker-compose.yml --env-file devyre\deploy\.env.example config` (with `CPA_HOME` set in the example).

### SRV-4 Fork README (`devyre/README.md`)

Cover:
- What differs from upstream, with a table of devyre files.
- How to run: `new-secrets.ps1` → `up.ps1` → `tailscale-serve.ps1`.
- How to sync (section 14).
- **Conflict hotspots**, the upstream files we touch: `api_tools.go`, `server_management_v8.go`, `server.go`, `service_config.go`, `config_basic.go`, `config_types.go`, `updater.go`, `config.example.yaml`.
- Rollback.

---

## 10. RT track — expiring-first routing (M2)

### RT-1 Readings model and parsers (new package `internal/quotareading`)

```go
package quotareading

type Kind int // KindShort: ≤24h window, gates only. KindLong: >24h, ranks, and gates once exhausted. KindScoped: model-specific, gates matching models.
type Source string // "header" | "usage" | "poll"

type Window struct {
    ID          string        // claude: "5h","7d","7d:fable","7d:opus","7d:sonnet",…  codex: "primary","secondary"
    Kind        Kind
    UsedPercent float64       // 0..100, clamped; NaN never stored
    ResetsAt    time.Time     // zero = unknown
    Length      time.Duration // 5h / 7d, or from Window-Minutes / limit_window_seconds
    Model       string        // lowercased family for KindScoped ("fable","opus","sonnet")
    ObservedAt  time.Time
    Source      Source
}
type Reading struct { AuthID, Provider string; Windows []Window }
```

Parsers. Each one is pure and has table tests.

| Func | Input | Notes |
|---|---|---|
| `FromClaudeHeaders(signals map[string]string, observedAt time.Time) []Window` | `Auth.Quota.Signals` | `anthropic-ratelimit-unified-5h-utilization` is a **fraction** (×100). `-5h-reset` is unix seconds. Same for `7d`. Map `7d_oi` → `7d:fable` (Scoped). Match keys case-insensitively, the way `collectQuotaSignals` stores them. Take fixtures from `helps/claude_ratelimit_test.go` |
| `FromClaudeUsage(body []byte, observedAt time.Time) []Window` | `/api/oauth/usage` | `five_hour`/`seven_day` `{utilization: **percent**, resets_at: RFC3339 or null}`. Map `seven_day_opus`/`seven_day_sonnet` → Scoped. Map `limits[]` with `kind=="weekly_scoped"` and `scope.model.display_name` → Scoped(model family) using `percent`. Skip the legacy `iguana_necktie` key when `limits[]` has Fable |
| `FromCodexHeaders(signals, observedAt)` | signals | `x-codex-{primary,secondary}-used-percent`; reset from `-reset-at` (unix) or `observedAt + reset-after-seconds`; `Length` from `-window-minutes`. Kind is Short when ≤ 24 h, else Long |
| `FromCodexUsage(body, observedAt)` | `/wham/usage` | `rate_limit.{primary_window,secondary_window}{used_percent, reset_at, limit_window_seconds}`. Confirm `reset_at` units against the panel's `src/features/quota/providers/codex/data.ts` and pin them in a test |

### RT-2 Store (`internal/quotareading/store.go`)

- `type Store struct{…}`, concurrency-safe, with methods:
  - `Put(authID, provider string, ws []Window)`: per window ID, replace only if the new reading is newer.
  - `Replace(authID, provider string, ws []Window, observedAt time.Time)`: `ws` is the complete set of windows of one usage body. Stored windows it lacks are dropped unless observed later, then it records like `Put`. Without this, a window the provider stopped reporting (a plan change, or positional Codex windows that changed meaning) would roll forward as a fresh full window forever.
  - `Get(authID) Reading`
  - `Forget(authID)`
  - `Snapshot() []Reading`
- `func Default() *Store`: a process-wide singleton.
- `func Effective(st *Store, authID, provider string, signals map[string]string, signalsAt time.Time, now time.Time) Reading` merges store windows with header windows; the newest per ID wins. Its parameters are **primitives** so `sdk/cliproxy/auth` can import it without a cycle; `quotareading` must not import `sdk/cliproxy/auth`.
- **Normalization at read time:** if `ResetsAt` is set and not after `now`, the window has reset. Treat it as `UsedPercent=0` with `ResetsAt = ResetsAt + n·Length`, rolled forward past `now`. If `Length` is unknown, mark it unknown.
- Tests use controllable clocks, never sleeps.

### RT-3 Usage cache in `APICall` (`internal/api/handlers/management/devyre_usage_cache.go`)

- **Allowlist:** method `GET`, a non-empty auth index, and an exact URL of `https://api.anthropic.com/api/oauth/usage`, `https://api.anthropic.com/api/oauth/profile` or `https://chatgpt.com/backend-api/wham/usage`. Never cache POSTs or the Codex credits URLs, because T3 redeems resets through them.
  - **Claude usage query variants**, such as the panel's reset-grant check `?cedar_ember=1&skip_spend=1`, are cached too, under their full URL with the Claude usage TTL, up to 4 distinct variants per credential. Further variants pass through uncached. Anthropic rate limits the endpoint per account whatever the query, so every variant, cached or not, counts toward the poller's Claude min-gap and shares the credential's 429 backoff.
  - **Writes:** a 2xx `POST`, `PUT`, `PATCH` or `DELETE` through `api-call` for a credential, such as a redeemed reset, makes all of that credential's cached responses stale. The next read goes upstream; the stale body stays as the fallback, and the Claude backoff is kept.
- **Key:** `authIndex + "|" + url`. **TTL:** Claude usage 5 min, Claude profile 1 h, Codex usage 60 s, all configurable.
- **Behavior:**
  - **hit:** return the cached `apiCallResponse` without touching tokens or upstream.
  - **miss:** single-flight. The leader calls upstream; followers wait on the leader's result, honoring the request ctx.
  - **2xx:** store it, then feed `quotareading.Default().Replace(auth.ID, provider, UsageSnapshot(...))` with `Source: usage`. Only a body with the endpoint's usage shape is recorded, and a query variant's body is merged with `Put` instead.
  - **429 / 5xx / transport error:** if a cached 2xx exists, return it with `X-CPA-Usage-Cache: stale` in `apiCallResponse.Header`; otherwise pass the error through unchanged.
  - **Claude 429:** start a per-auth backoff of 5 min, doubling up to 60 min, cleared on the next 2xx. During backoff, don't call upstream; serve stale data or the last 429.
  - **Bypass:** if the management request header `X-CPA-Usage-Cache: refresh` is present **and** the last upstream call for the key is ≥ 30 s old, act as a miss. Otherwise serve the cache.
  - Always annotate `X-CPA-Usage-Cache: hit|miss|stale|bypass` in the returned `header` map, or `backoff` when the stored Claude 429 is served.
- **Hook** into `api_tools.go` with three small hunks: lookup after `authIndex` and `urlStr` are parsed (before token resolution), complete or fail after the upstream response or transport error, and record on success. Keep the APICall flow otherwise unchanged; v0 and v8 share it.
- **Tests** in `devyre_usage_cache_test.go` (httptest upstream counting calls; fake clock):
  - TTL expiry.
  - 10 concurrent requests → 1 upstream call.
  - Stale on 429, and backoff doubling.
  - The bypass floor.
  - POSTs and credits URLs are never cached.
  - Readings are written on 2xx.
  - The T3 contract tests (SRV-2) still pass.

### RT-4 Idle usage poller (`internal/api/handlers/management/devyre_quota_observer.go`)

- `func (h *Handler) StartQuotaObserver(ctx context.Context)`, started once from `internal/api/server.go` right after `s.mgmt = managementHandlers.NewHandler(...)`. Use a `sync.Once` with `context.Background()`, the same precedent as `managementasset.StartAutoUpdater`.
- **Each minute it reads the current config** and does nothing unless the poller is enabled. That means an explicit `routing.quota-observation.poller.enabled`, which when unset defaults to on exactly when `routing.strategy` is `expiring-first`. For every enabled OAuth auth of provider `claude` or `codex`:
  - Poll only if its newest long-window reading is older than the provider interval: Claude 30 min, Codex 5 min.
  - Skip it if it is in usage-cache backoff.
  - For Claude, also require that the **provider-wide** gap since the last Claude usage call is ≥ `claude-min-gap` (10 min). With 3 accounts, the startup sweep finishes in about 20 min while header readings fill in from traffic.
- **The request** goes through the same cache path as `APICall`: Claude uses `Authorization: Bearer <token>` and `anthropic-beta: oauth-2025-04-20`. Codex uses the header set T3 sends, which is in F6 and `apps/server/src/usage/cliproxyApi.ts`. Resolve the token and transport with the existing `h.authByIndex`, `h.resolveTokenForAuth` and `h.apiCallTransport`. A poll result therefore also warms the cache for the panel and T3.
- Never poll disabled auths, xAI paid, or Meta (bandoyer #13, #14). xAI free and Kimi are out of scope until the user adds those credentials.
- Each tick, poller on or off, also forgets the stored readings of credentials the manager no longer lists.
- Logging is at debug level and never includes tokens or bodies.
- **Tests:** a fake clock drives the scheduling decisions (who is due, the min-gap, skips) through a pure `nextPolls(now, auths, readings, lastCall) []authID` function.

### RT-5 `expiring-first` selector and config wiring

**New file `sdk/cliproxy/auth/selector_expiring_first.go`.**

```go
// ExpiringFirstSelector prefers the credential whose quota would be lost soonest.
type ExpiringFirstSelector struct {
    GateRemainingPercent float64          // default 2
    LogPicks             bool
    Now                  func() time.Time // test hook; nil → time.Now
    mu                   sync.Mutex
    rr                   map[string]string // provider:model → last picked ID among ties/unknowns
}

func (s *ExpiringFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error)
```

Algorithm. It is deterministic given readings and `now`.

1. `available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)`. This already drops disabled and cooling credentials and keeps the **highest priority tier**. Then `available = preferCodexWebsocketAuths(ctx, provider, available)`.
2. For each auth, compute `r := quotareading.Effective(quotareading.Default(), a.ID, a.Provider, a.Quota.Signals, a.Quota.ObservedAt, now)`.
3. **Gate:** the auth is gated if any Short or Long window, or any Scoped window whose `Model` family matches the requested `model`, has remaining ≤ `GateRemainingPercent` and `ResetsAt` after now. An exhausted Long window gates too: the credential would only answer 429 until it resets. Family match: the model name contains "fable", "opus" or "sonnet", lowercased.
4. **Urgency:** take the Long window with the largest `Length`. If there is none, take the longest window of any kind. Then `urgency = remaining% / max(hoursUntil(ResetsAt), 0.25)`. If there is no window or no reset time, the urgency is unknown.
5. **Order:**
   - Known urgencies descending, with ties broken by round-robin on ID.
   - Then unknowns, round-robin.
   - If every candidate is gated, fall back to the gated ones ordered by earliest gate reset, and let upstream 429 or cooldown handle it.
6. Log with `selectorLogEntry(ctx)` fields: `provider`, `model`, `auth`, `urgency`, `reason` (`most-urgent`, `no-data`, `all-gated`). Use info level when `LogPicks` is set, otherwise debug. Never log tokens.

**Wiring:**
- `sdk/cliproxy/service_config.go`: add `case "expiring-first","expiringfirst","ef","soonest-reset": state.strategy = "expiring-first"` in `normalizedRoutingRuntimeState`, and construct it in `newRoutingSelector`. Copy the gate and log settings into `routingRuntimeState` as **plain values** (`float64`, `bool`). `applyConfigUpdate…` compares states with `*s.appliedRoutingState != routingState` (`service_config.go:217`), so the struct must stay comparable: no pointers, maps or slices. The existing code already wraps it with `SessionAffinitySelector` when affinity is on.
- `internal/api/handlers/management/config_basic.go`: add the same case to `normalizeRoutingStrategy` so `PUT /routing/strategy` accepts it.
- `internal/config/config_types.go`: extend the `Strategy` comment and add two fields to `RoutingConfig`. Define the structs in the new file `internal/config/devyre_routing.go`:
  ```go
  ExpiringFirst    ExpiringFirstConfig    `yaml:"expiring-first,omitempty" json:"expiring-first,omitempty"`
  QuotaObservation QuotaObservationConfig `yaml:"quota-observation,omitempty" json:"quota-observation,omitempty"`
  ```
  - `ExpiringFirstConfig{GateRemainingPercent *float64; LogPicks bool}`
  - `QuotaObservationConfig{UsageCache{Enabled *bool; ClaudeUsageTTL, CodexUsageTTL, RefreshFloor string}; Poller{Enabled *bool; ClaudeInterval, ClaudeMinGap, CodexInterval string}}`

  Durations are strings parsed like `session-affinity-ttl`. Defaults: the usage cache is **on** whatever the strategy; the poller is on when the strategy is `expiring-first`.
- Check `internal/config/config_yaml.go` for v8 key handling: ordering, migration or allowlists. Add a load → save → load round-trip test proving both blocks survive a save through `/v8/management` (bandoyer: "a successful configuration write through /v8/management migrates").
- `config.example.yaml`: document the strategy and the two blocks under `routing:`.
- `isBuiltInSelector`: leave it **unchanged**. Non-built-in selectors take the legacy pick path, which affinity already forces (`conductor_selection.go:655-673`).

**Tests** in `selector_expiring_first_test.go`, table-driven with a fake clock:
- A (40% left, resets in 3 h) beats B (90%, 4 d).
- D (80%, 2 h) beats C (1%, 10 min).
- A credential with an exhausted 5-hour window is gated, and so is one with an exhausted weekly window.
- A Fable-scoped window gates only Fable models.
- A passed reset counts as a full window with low urgency.
- Unknowns rank after the knowns, round-robin among themselves.
- The priority tier is respected.
- The all-gated fallback works.
- Codex WebSocket preference is preserved.
- Through `SessionAffinitySelector`: a bound session keeps its credential while it's available, even when another is more urgent; on unavailability, failover picks the most urgent.
- Plus a `service_config` test that the strategy aliases normalize.

### RT-6 Readings endpoint (`internal/api/handlers/management/devyre_routing_readings.go`)

Register `GET /v8/management/routing/quota-readings` with one line in `server_management_v8.go`. The response:

```json
{
  "strategy": "expiring-first",
  "generated_at": "2026-10-03T21:00:00Z",
  "credentials": [
    {
      "auth_id": "…", "auth_index": "…", "provider": "claude", "label": "claude-…json",
      "priority": 0, "usable": true, "gate_reason": "", "urgency_per_hour": 13.2, "rank": 1,
      "windows": [
        {"id": "7d", "kind": "long", "remaining_percent": 21, "resets_at": "…", "observed_at": "…", "source": "header"}
      ]
    }
  ]
}
```

- `rank` is per provider among usable credentials in the top priority tier, in a model-agnostic view where Scoped windows are ignored.
- The endpoint is read-only, management-auth protected and never returns tokens.
- Tests use golden JSON from a fake store.

**RT done when:**
- `gofmt -l .` is empty, `go build ./cmd/server` passes and `go test ./...` is green, including `-run 'T3Hub|ExpiringFirst|UsageCache|QuotaReading|QuotaObserver'`.
- With live traffic, the logs show `reason=most-urgent` picks.
- Setting `routing.strategy: expiring-first` hot-reloads without a restart, the same as other strategies.

---

## 11. DEP track — deployment on this PC

| Step | Action | Done when |
|---|---|---|
| DEP-1 (HITL) | **Tailscale.** In the admin console → DNS, MagicDNS must be on; enable **HTTPS Certificates** | `tailscale status --json` shows non-empty `CertDomains` |
| DEP-2 (HITL) | **Secrets.** Run `devyre\scripts\new-secrets.ps1`. It creates `%USERPROFILE%\.cli-proxy-api\{auths,logs,static,plugins,secrets}` with a user-only ACL on `secrets`, generates the management key and client keys, renders `config.yaml` from the template and prints the management key **once**. The user saves it in their password manager | The file exists and the user confirms the key is saved |
| DEP-3 | **Env.** Copy `devyre\deploy\.env.example` → `.env` and set `CPA_HOME`, `CPA_TZ` and `CPA_PUBLIC_URL` (the ts.net URL) | `.env` exists and git ignores it |
| DEP-4 | **Build and run.** `devyre\scripts\up.ps1` builds `cpa-devyre:current`, also tags it with the git sha, and runs `docker compose up -d` | `docker ps` shows `cpa` running. `docker logs cpa` shows the config loaded and `management asset updated successfully` from the Devyre release. `Invoke-WebRequest http://127.0.0.1:8317/management.html` returns 200 |
| DEP-5 | **Publish on the tailnet.** `devyre\scripts\tailscale-serve.ps1` runs `tailscale serve --bg --https=8318 http://127.0.0.1:8317` | `https://<machine>.<tailnet>.ts.net:8318/management.html` loads from this PC and one other tailnet device |
| DEP-6 | **Calibrate trusted proxies.** <br>1. Temporarily set `observability.logs.debug: true`; it hot-reloads. <br>2. Make one request through the ts.net URL, then one through `http://127.0.0.1:8317`. <br>3. Read the access-log client IPs in Logs Viewer. The ts.net request must show this PC's **tailnet IP** (100.x). The direct request shows the Docker gateway, e.g. 172.x.0.1. <br>4. If the ts.net request also shows the gateway, `server.trusted-proxies` doesn't cover that hop. Add the exact gateway IP and restart; trusted-proxies only applies on restart. <br>5. Set `debug` back to false | Tailnet requests resolve to tailnet IPs, so one client's failed key attempts can't ban every client |
| DEP-7 (HITL) | **Panel login.** Open the ts.net URL, log in with the management key and pick English. Confirm the strategy is `round-robin` (M1) and session affinity is on. In Config Panel → API keys, name the 4 client keys `t3-code`, `claude-code-cli`, `codex-cli` and `other-devices` | The sidebar, header and tabs match the frame |
| DEP-8 (HITL) | **3 Claude accounts.** Go to OAuth Login → Anthropic (Claude) and do it 3 times. **Use a separate browser profile or private window per account** so the right claude.ai session authorizes. On this PC the `localhost:54545` callback completes by itself; from another device, paste the callback URL into "Submit Callback URL" | Auth Files lists 3 `claude-*.json`. Quota → Ledger shows 3 rows with the 7-day limit headline. A plan label ("Max") appears once the profile call succeeds |
| DEP-9 | **Smoke test.** `GET /v1/models` with the `claude-code-cli` key lists the Claude models; note the Opus 5.5 id, expected `claude-opus-5-5`. Then send `POST /v1/messages` with that model and a short prompt. Run it from 3 shells, each with a different `X-Claude-Code-Session-Id` header, and confirm in Logs that requests spread across credentials and that a repeated session id sticks to its first credential | Both checks hold |
| DEP-10 (M2) | **Expiring-first.** Pull `main`, rerun `up.ps1`, set `routing.strategy: expiring-first` and uncomment the `expiring-first` and `quota-observation` blocks. Check `GET /v8/management/routing/quota-readings` and watch the logs for `reason=most-urgent` | Readings are populated for all 3 accounts within about 30 min, sooner with traffic |

---

## 12. CLI track — wire the clients

### CLI-1 T3 Code hub (HITL in T3 Code)

1. Update T3 Code to the latest version.
2. Open **Settings → Providers → Usage providers → Add hub** and enter:
   - Environment: this PC
   - URL: `https://<machine>.<tailnet>.ts.net:8318`
   - Management key: the plaintext key
   - Label: `CPA`

**Done when:** Usage → Limits → Claude shows **one pooled card** for the 3 accounts, with 5-hour and weekly windows and one segment per account. T3 deduplicates an account it also sees directly. T3's own UI still lists model-scoped windows such as Fable, because T3 isn't forked.

### CLI-2 T3 Code Claude traffic through the pool (HITL in T3 Code)

1. In **Settings → Providers → Claude**, on the default instance, set **Environment variables**:

   | Variable | Value |
   |---|---|
   | `ANTHROPIC_BASE_URL` | `https://<machine>.<tailnet>.ts.net:8318`, with no `/v1` |
   | `ANTHROPIC_AUTH_TOKEN` | the `t3-code` key, marked **Sensitive** |
   | `ANTHROPIC_API_KEY` | explicitly empty |
   | `ENABLE_PROMPT_CACHING_1H` | `1`. Recommended; verify per F15 |
   | `ENABLE_TOOL_SEARCH` | `true`. Recommended; verify per F15 |

2. Add a second Claude instance **"Claude Direct"**: same binary, **same** config dir and no proxy env. It's the bypass for claude.ai-only features. Because both instances share the config dir, threads can switch between them.
3. Optional, Theo's "3 clicks", once Codex accounts are in the pool: on the pooled instance, use **Add custom model** → `gpt-5.6-sol` (confirm the id via `/v1/models`).

**Done when:** a new T3 Claude thread's requests appear in the panel Logs under the `t3-code` key; the Ledger shows usage moving on the picked account; and the same thread keeps its credential (affinity).

### CLI-3 Claude Code CLI (HITL: profile change)

Append `devyre\clients\powershell-profile.ps1` (Appendix E) to `$PROFILE`, which here is `Z:\Documents\WindowsPowerShell\Microsoft.PowerShell_profile.ps1`. It sets `ANTHROPIC_BASE_URL` and `ANTHROPIC_AUTH_TOKEN` from the secrets file and defines `claude-direct`, plus optional `claudex`. Never put these in `~/.claude/settings.json`: those values override the shell **and** T3's per-instance env (bandoyer #15).

**Done when:** in a new shell, `claude -p "say ok"` succeeds, the panel Logs show the `claude-code-cli` key, and `claude-direct` still uses the user's own login.

### CLI-4 Codex (optional, only if ChatGPT accounts are added)

1. Log in through OAuth Login → Codex.
2. Append `devyre\clients\codex-config.toml` (Appendix F) to `%USERPROFILE%\.codex\config.toml`.
3. Set `CLIPROXY_API_KEY` in the profile.
4. Run `codex -c model_provider=cliproxy`. In T3, the Codex instance gets launch args `-c model_provider=cliproxy` and env `CLIPROXY_API_KEY=<t3-code key>`.
5. Turn on `websockets: true` for each Codex credential under Auth Files → fields.

### CLI-5 Other tailnet devices

Use the same steps with the ts.net URL and the `other-devices` key, or a dedicated key per device if per-device attribution is wanted.

---

## 13. End-to-end acceptance (sign-off with the user)

- [ ] `https://<machine>.<tailnet>.ts.net:8318/management.html#/quota` matches the reference frame in shell, header, tabs, summary strip and rows. Claude reads **"7-day limit … of 300%"** for 3 accounts, the secondary line is **"5-hour limit"**, and there is **no Fable** in the Ledger. Theme is dark. "Show emails" toggles masking.
- [ ] Panel version (Management Center Info) shows `v1.25.3-devyre.N`; the server shows `v8.0.13-devyre.N` or the sha.
- [ ] T3 Code → Usage → Limits shows the pooled Claude card from the hub.
- [ ] T3 Code Claude threads and the `claude` CLI go through CPA (Logs show the client key names), and a thread sticks to one credential.
- [ ] (M2) `routing.strategy: expiring-first` is live. `/v8/management/routing/quota-readings` ranks the 3 accounts. A cold new thread lands on rank 1, and the logs show `reason=most-urgent`.
- [ ] Claude usage endpoint stays healthy: no repeated 429 in Logs over 24 h with the panel, the T3 hub and the poller all active. `X-CPA-Usage-Cache` shows `hit` on repeat T3 checks.
- [ ] `go test ./...` and `bun run verify` are green on both forks' `main`; `TestT3Hub_*` passes.
- [ ] No secrets, emails or the tailnet hostname are in either fork's own commits. Scan only fork commits, since upstream history is full of unrelated `@`s: `git log -p upstream/main..main | Select-String -Pattern 'cpa-(mgmt|t3|cc|cx|dev)-|\.ts\.net|[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[a-z]{2,}'`. Review the hits; `Co-Authored-By` trailers and the synthetic fixture emails are expected.
- [ ] Bypass works: `claude-direct` and the "Claude Direct" T3 instance.
- [ ] Backup script produces an archive. The user knows where the management key and the backup live.

---

## 14. Runbooks

### Sync upstream into the server fork (weekly, or when a fix is needed)

1. Run `devyre\scripts\sync-upstream.ps1`. It creates `sync/upstream-YYYYMMDD` from `main` and merges `upstream/main`.
2. Resolve conflicts at the hotspots listed in `devyre/README.md`, keeping upstream behavior plus our hunks.
3. `gofmt -l .` (empty), then `go build ./cmd/server`, then `go test ./...`.
4. Open a PR into the fork (`--repo Devyre/CLIProxyAPI`), merge it, tag `v<upstream>-devyre.<n>`, then run `up.ps1`.

### Sync the panel fork

1. `git fetch --multiple upstream chhoumann`.
2. Branch, `git merge upstream/main`, and check `git log main..chhoumann/dev` for ledger updates worth merging.
3. `bun run verify`, then rerun the visual harness (UI-4).
4. PR, tag `v<upstream>-devyre.<n>` and push; the release workflow publishes. The server picks it up within 3 h or on restart.

### Roll back

- **Server:** `docker tag cpa-devyre:<previous-sha> cpa-devyre:current; docker compose -f devyre\deploy\docker-compose.yml --env-file devyre\deploy\.env up -d`.
- **Panel:** mark the previous release as latest with `gh release edit <tag> --repo Devyre/Cli-Proxy-API-Management-Center --latest`, delete `%USERPROFILE%\.cli-proxy-api\static\management.html`, then restart the container.

### Back up

Run `devyre\scripts\backup.ps1`. It writes `config.yaml`, `auths\` and `secrets\` to a dated archive in a target the user chooses. **The archive holds live OAuth tokens.** Keep it on BitLocker-protected storage or encrypt it, for example with 7-Zip AES.

### Troubleshooting

| Symptom | Fix |
|---|---|
| `IP banned …` | Wait 30 min or restart the container; check that client keys and the T3 hub key are correct (F13) |
| Panel shows the stock UI | `panel-github-repository` is wrong, or the static file was missing and the fallback fired. Fix the config, delete `static\management.html`, restart |
| T3 hub "could not list accounts" | Check the URL has no path, the key is plaintext, `allow-remote: true`, and T3's machine can reach the ts.net URL |
| Claude usage 429s | Raise `claude-usage-ttl` and `claude-min-gap`; avoid spamming manual refresh |
| Clients fail while the PC sleeps | Use `claude-direct`. Long term, move the same compose file to an always-on Linux box (D9) |

---

## 15. Risks and mitigations

| Risk | Mitigation |
|---|---|
| **Account enforcement.** Anthropic's consumer terms restrict automated and third-party use of subscription OAuth. Relay patterns (one endpoint, many tokens) are what detection targets; the April 2026 relay bans are an example | Personal use only. Never share keys, never expose publicly (Funnel stays off) and never serve other people. Keep affinity on so traffic doesn't hop per request. Use a stable home egress. Theo gives the same warning (F10). The user accepts the residual risk |
| Upstream churn: dozens of commits a day on both repos | Isolated `devyre_*` files, small hunks, a conflict-hotspot list, contract tests, and weekly syncs |
| Upstream removes `/v0` | `TestT3Hub_*` fails first; restore it with `devyre_v0_shim.go` (D8) |
| Claude usage endpoint throttling | Usage cache, backoff, the poller's provider-wide min-gap, and headers as the primary signal (D6, D7) |
| Cache-write cost from credential moves | Affinity on with a 1 h TTL, and no urgency-driven migration (D5) |
| Docker Desktop is down when the PC sleeps or reboots | Auto-start Docker and `restart: unless-stopped`. Bypass commands exist. A Linux box is the long-term move |
| A ledger regression after the chhoumann merge | Pinned SHA, the visual harness and `bun run verify` |

---

## 16. Addendum: Tailnet passwordless (2026-10-04)

**Request:** "Make it so that my shit is cut off from the internet aside from tailnet and that it doesnt require a password." The tailnet was renamed to a new `<tailnet>.ts.net` name on the same day. User-facing documentation: `devyre/README.md`, "Tailnet-only and passwordless".

### Verified facts (do not re-research)

| # | Fact |
|---|---|
| T1 | `tailscale serve` 1.102.4 (`ipn/ipnlocal/serve.go`) proxies to `http://127.0.0.1:8317`. It **sets** `X-Forwarded-For` to the single tailnet source IP, overwriting client values, sets `X-Forwarded-Host` to the incoming Host and keeps Host. It **deletes** client-supplied `Tailscale-User-Login`, `Tailscale-User-Name`, `Tailscale-User-Profile-Pic`, `Tailscale-Funnel-Request` and `Tailscale-Headers-Info`, then sets `Tailscale-User-Login`/`-Name` (RFC 2047 Q-encoded when non-ASCII) for untagged user-owned source nodes, this PC included. Tagged nodes get no identity headers; Funnel requests get `Tailscale-Funnel-Request: ?1` and no identity. `Origin` and `Sec-Fetch-*` pass through. Serve routes by Host: only the machine's short name and FQDN on 8318 reach CPA, anything else gets 404 from tailscaled. |
| T2 | Inside the container every request comes from the Docker bridge gateway (172.16.0.0/12). Publishing is 127.0.0.1-only, so only processes on this PC reach the container. Serve's hop, local browsers and programs, and other containers reaching the host through `host.docker.internal` all look the same to CPA. |
| T3 | The tailnet: this PC, two iOS devices, two other Windows PCs, a CI runner and a bot VM. All are untagged and owned by the single owner login, so serve stamps the automation hosts with the owner login too. Real names, IPs and logins live only in the runtime config. |
| T4 | Browsers send no `Sec-Fetch-*` headers to URLs that are not potentially trustworthy (the plain-HTTP tailnet URL is one), and no `Origin` on GET/HEAD no-cors requests (img, script, iframe, navigation, redirects). Every CPA response carries `Access-Control-Allow-Origin: *`. |
| T5 | Plain-HTTP serve keys its entries by the stored login profile's MagicDNS name, which refreshes only on a real prefs edit, so after a tailnet rename the new FQDN gets 404 from tailscaled. `tailscale debug prefs` omits `ProfileName` when no nickname is set (absent means no nickname) and also prints `Config.PrivateNodeKey`. |
| T6 | The v8 config writer replaces lists whole; `GET /v8/management/config/management/tailnet-auth` is 404 until the block exists; a server built before the block exists rejects it as an unknown field. |

### Goals

- **G1 Exposure:** CPA is reachable only from this PC (loopback) and from tailnet devices through tailscale serve. Never the LAN, never the internet (no Funnel, no 0.0.0.0 publish), with guards that catch a future misconfiguration.
- **G2 Passwordless:** allowed tailnet devices of allowed logins, and optionally direct requests on this PC, need no management key and optionally no proxy API key. Everyone else (automation hosts, unknown devices, unlisted tagged nodes, Funnel, anything cross-site) still needs the key. The panel opens straight into the dashboard on allowed devices.

### Config (`management.tailnet-auth`)

Code defaults are all off or empty; the deploy template switches `enabled`, `allow-local` and `proxy-api` on and leaves the lists empty for `devyre/scripts/tailnet-trust.ps1` to fill.

```yaml
management:
  tailnet-auth:
    enabled: false        # master switch
    allowed-logins: []    # Tailscale-User-Login values; empty = no tailnet trust at all (fail closed)
    allowed-devices: []   # tailnet IPs allowed without a key; empty = no tailnet device is keyless (fail closed)
    allowed-hosts: []     # Host names keyless requests may target; empty = no keyless trust (fail closed)
    allow-local: false    # trust direct requests on this PC whose Host is localhost, 127.0.0.1 or ::1
    proxy-api: false      # also accept trusted requests on the proxy API without an API key
```

### Trust decision (one pure function on the server)

1. `enabled` is true, and the direct TCP peer (`c.RemoteIP()`, not `ClientIP()`) is loopback or inside `server.trusted-proxies`.
2. No `Tailscale-Funnel-Request` header.
3. The normalized Host is in `allowed-hosts`; an `X-Forwarded-Host` equals the Host.
4. Browser guard: a present `Sec-Fetch-Site` is `same-origin` or `none`; a present `Origin` is a real origin whose host:port equals the Host. Without `Origin` the request must carry a non-browser signal: `X-CPA-Keyless: 1`, `Authorization: Bearer <non-empty>`, or a non-empty `X-Management-Key`, `X-Api-Key` or `X-Goog-Api-Key` (never query keys or other schemes). A cross-origin page can add a custom header only in CORS mode, which always adds `Origin`.
5. The Host picks the path. A **loopback Host** allows only the local path, and any `X-Forwarded-For`, `X-Real-IP`, `Forwarded`, `X-Forwarded-Host` or `Tailscale-*` header there is forged, so untrusted; trusted only with `allow-local`. A **tailnet Host** allows only the tailnet path: `X-Forwarded-For` is exactly one IP in 100.64.0.0/10 or fd7a:115c:a1e0::/48 that is **listed in `allowed-devices`** (always; there is no "any device of the login" mode). With any `Tailscale-User-*` header, the decoded `Tailscale-User-Login` must be in `allowed-logins`; without one (a tagged node) the listed IP suffices, but an empty `allowed-logins` still disables all tailnet trust.
6. Any of the headers the decision reads present twice is untrusted.
7. Anything else falls back to the key check, unchanged. Trusted requests never touch the failure counter, and a wrong key on a trusted request is ignored. A request with **no** credential never counts as a failed attempt, trusted or not.

### Application points

- **Management:** one `// devyre:` hunk at the top of `Handler.Middleware()` covers every `/v0/management` and `/v8/management` route, the plugin management routes and the new `GET /v8/management/auth/session` (200 with `method` `key`, `tailnet` or `local`; 401 without a key, not counted). Trust never opens more than the key would: it needs a configured management key, and with `management.allow-remote: false` only a loopback client can be trusted (inside the container no client is loopback, so the template keeps it true). Trusted management responses drop the `Access-Control-*` headers and send `Cache-Control: no-store`. OAuth callbacks, `/v0/resource/plugins/*`, `/healthz`, `/` and `/management.html` stay as they were; RESP on 8317 and `/keep-alive` stay key or password only.
- **Proxy API:** inside `accessAuthMiddleware`, after `manager.Authenticate` fails with a 401-class error, `proxy-api` plus a trusted decision sets `userApiKey` to `tailnet:<login>@<device IP>` or `local` and `accessProvider` to `tailnet-auth`. A valid API key always wins. `/v1/ws` (AI Studio relay) uses a key-only variant.
- **Anti-framing:** the panel and the safe-mode page send `Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY`.
- **Panel:** probes `/v8/management/auth/session` without `Authorization` (and never when framed); on 200 it connects without a key, sends `X-CPA-Keyless: 1` to its own origin and never an `Authorization` header; logout is remembered for the browser session with a "Continue with Tailscale" button; the identity shows as "Signed in via Tailscale - <login>" or "Signed in from this PC".

### Ops (built in `devyre/`)

| Item | What it does |
|---|---|
| `scripts/tailnet-trust.ps1` | Candidates: this PC and untagged peers of the owner login. `-Include` wildcards (host name or first MagicDNS label) select; without `-Include` a re-run keeps the devices already allowed. `-Exclude` removes; names with the token `ci`, `runner`, `bot`, `build` or `agent` are dropped unless `-AllowAutomationName` names them. A selected device contributes all its Tailscale IPs. An empty selection writes nothing and exits 1. Writes only `PUT /v8/management/config/management/tailnet-auth` with the complete block; flags keep their live values (template values when the block is new); then re-reads `management` and exits 1 unless only `tailnet-auth` changed. The snapshot holds the secret-key hash and is compared in memory only. A server without `GET /v8/management/auth/session` predates the block and gets no write. `-ShowOnly`/`-WhatIf`, `-Disable`, `-Enable`. |
| `scripts/exposure-check.ps1` | PASS/FAIL/WARN/SKIP, exit 1 on any FAIL: docker bindings, listeners, LAN reachability, Funnel, exactly one serve handler on 8318 (`/` -> `http://127.0.0.1:8317`, current FQDN, no raw TCP forward), both health URLs, gateway inside `trusted-proxies`, FQDN in `allowed-hosts`, the keyless session probe (SKIP when this PC is not listed), the no-signal and `Origin: http://evil.example` probes, `allow-local` and forged headers on the direct path, and a WARN-only throwaway-container probe. |
| `scripts/tailscale-serve.ps1` | After configuring, verifies `/healthz` on the current FQDN. On a 404 it refreshes the profile (temporary nickname, then the original, in try/finally, with `--nickname=<value>` as one token and a ProfileName read-back after each step, never printing the prefs), and resets and re-adds serve when entries keyed to an old FQDN remain and every entry is on 8318; otherwise it prints the manual steps. Never enables Funnel. `-ShowOnly`/`-WhatIf`. |
| `scripts/up.ps1` | Refuses a compose file that publishes beyond 127.0.0.1, and stops a running `cpa` that does. |
| `scripts/cpa-common.ps1` | Shared helpers; never prints or stores the management key. |
| `deploy/config.template.yaml` | The block on with empty lists; the `trusted-proxies` comment says the gateway range is load-bearing for keyless access, which reads the list live, while bans and logs need a restart. Pinned by `TestDevyreDeployTemplate_TailnetAuthBlock` (raw YAML) and `TestTailnetAuthConfig_DeployTemplateBlock` (typed). |

### Decisions (do not relitigate)

| # | Decision | Why |
|---|---|---|
| D13 | `allowed-devices` is authoritative and fails closed | Every node, the CI runner and bot VM included, shares the owner login, so the login check alone would admit the automation hosts, which could pull the Claude tokens |
| D14 | Without `Origin`, require an affirmative non-browser signal | Plain HTTP never triggers the Sec-Fetch guard, and no-cors GETs carry no `Origin` |
| D15 | The Host splits the local and tailnet paths; proxy markers on a loopback Host are forged | The peer IP cannot tell serve's hop from local software |
| D16 | A missing key never counts toward the ban | The session probe and keyless clients would otherwise ban the shared Docker gateway IP |
| D17 | `tailnet-trust.ps1` writes only the `tailnet-auth` subtree and verifies the rest is untouched | A whole-config write could drop or reorder unrelated settings |
| D18 | Out of scope: a tailscaled sidecar, capping tailnet trust by `allow-local` | Not needed for a single-user PC; documented as residual risk instead |

### Residual risks (documented in the README)

- The trusted set is the allowed devices plus anything that can reach this PC's loopback, containers included; `allow-local: false` does not stop local software, which can forge tailnet headers.
- Over plain HTTP only DNS authenticates the tailnet URL; Serve HTTPS removes that and turns on the Sec-Fetch guard.
- Anything served from the CPA origin (panel, plugin resource pages) acts with keyless admin rights.

### Acceptance

- [ ] On the deployed build, `exposure-check.ps1` ends with 0 FAIL.
- [ ] An allowed phone opens `<tailnet URL>/management.html` straight into the dashboard; the CI runner and bot VM get 401 without the key.
- [ ] T3 Code's hub keeps working with any non-empty key from this PC.
- [ ] Server `go test ./...` and panel `bun run verify` stay green.

---

## Appendix A — `devyre/deploy/config.template.yaml`

```yaml
# Devyre CPA runtime config (v8 layout). Rendered by devyre/scripts/new-secrets.ps1 into
# %USERPROFILE%\.cli-proxy-api\config.yaml. Never commit the rendered file.
config-version: 8

server:
  host: ""                 # all interfaces inside the container; the host publishes 127.0.0.1:8317 only
  port: 8317
  # tailscale serve -> 127.0.0.1:8317 -> Docker Desktop -> container. Trust those hops so the
  # real tailnet client IP (X-Forwarded-For) drives bans and logs. Calibrate in DEP-6. Restart after edits.
  trusted-proxies:
    - 127.0.0.1
    - 172.16.0.0/12
    - 192.168.65.0/24

management:
  allow-remote: true                 # the container never sees loopback; panel and T3 hub arrive via tailscale
  secret-key: "__MANAGEMENT_KEY__"   # plaintext here; hashed in place on first start
  disable-control-panel: false
  disable-auto-update-panel: false   # verified-digest updates from our fork at start and every 3h
  panel-github-repository: "https://github.com/Devyre/Cli-Proxy-API-Management-Center"

access:
  api-keys:
    - "__KEY_T3_CODE__"
    - "__KEY_CLAUDE_CODE_CLI__"
    - "__KEY_CODEX_CLI__"
    - "__KEY_OTHER_DEVICES__"

routing:
  strategy: "round-robin"            # M1. Change to "expiring-first" at DEP-10 (M2).
  session-affinity: true
  session-affinity-ttl: "1h"
  session-affinity-subagents: true
  retry:
    request-retry: 3
    max-retry-credentials: 0
    max-retry-interval: 30
  # Available once the RT track ships (M2):
  # expiring-first:
  #   gate-remaining-percent: 2
  #   log-picks: true
  # quota-observation:
  #   usage-cache:
  #     enabled: true
  #     claude-usage-ttl: "5m"
  #     codex-usage-ttl: "60s"
  #     refresh-floor: "30s"
  #   poller:
  #     enabled: true
  #     claude-interval: "30m"
  #     claude-min-gap: "10m"
  #     codex-interval: "5m"

oauth:
  auth-dir: "/data/auths"

observability:
  logs:
    debug: false
    logging-to-file: true            # feeds the panel's Logs Viewer (written under WRITABLE_PATH/logs)
    logs-max-total-size-mb: 200
    request-log: false               # never record prompts by default
  usage:
    usage-statistics-enabled: true   # Dashboard throughput / usage
    redis-usage-queue-retention-seconds: 60

plugins:
  enabled: true                      # Plugins / Plugin Store like Theo's sidebar; nothing installed
  dir: "/data/plugins"
```

## Appendix B — `devyre/deploy/docker-compose.yml`

```yaml
name: cpa
services:
  cpa:
    image: cpa-devyre:current
    build:
      context: ../..
      dockerfile: Dockerfile
      args:
        VERSION: ${CPA_VERSION:-devyre}
        COMMIT: ${CPA_COMMIT:-none}
        BUILD_DATE: ${CPA_BUILD_DATE:-unknown}
    container_name: cpa
    command: ["./CLIProxyAPI", "--config", "/data/config.yaml"]
    environment:
      TZ: ${CPA_TZ:-America/Los_Angeles}   # Go honors TZ at runtime (tzdata is in the image)
      WRITABLE_PATH: /data                 # logs -> /data/logs, panel -> /data/static
    ports:
      - "127.0.0.1:8317:8317"     # API + panel (tailscale serve fronts this)
      - "127.0.0.1:54545:54545"   # Claude OAuth callback for panel logins from this PC
      - "127.0.0.1:1455:1455"     # Codex OAuth callback (optional)
    volumes:
      - ${CPA_HOME:?set CPA_HOME in .env}:/data
    restart: unless-stopped
```

## Appendix C — `devyre/deploy/.env.example`

```dotenv
# Copy to .env (gitignored). Forward slashes are fine on Windows.
CPA_HOME=C:/Users/Devyre/.cli-proxy-api
CPA_TZ=America/Los_Angeles
# https://<machine>.<tailnet>.ts.net:8318  — from: tailscale status --json → Self.DNSName
CPA_PUBLIC_URL=
```

## Appendix D — scripts (`devyre/scripts/`, PowerShell 5.1 compatible)

`new-secrets.ps1`:

```powershell
#requires -Version 5.1
<# Generates CPA secrets and renders %USERPROFILE%\.cli-proxy-api\config.yaml from the template.
   Idempotent: existing secrets and config are kept unless -Rotate is passed. #>
[CmdletBinding()] param([switch]$Rotate)
$ErrorActionPreference = 'Stop'
$cpaHome = Join-Path $env:USERPROFILE '.cli-proxy-api'
$secrets = Join-Path $cpaHome 'secrets'
foreach ($d in 'auths','logs','static','plugins','secrets') {
  $p = Join-Path $cpaHome $d
  if (-not (Test-Path $p)) { New-Item -ItemType Directory -Path $p -Force | Out-Null }
}
icacls $secrets /inheritance:r /grant:r "$($env:USERNAME):(OI)(CI)F" | Out-Null
$utf8 = New-Object System.Text.UTF8Encoding($false)

function New-Token([string]$Prefix) {
  $bytes = New-Object byte[] 32
  $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
  try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
  return $Prefix + '-' + [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+','-').Replace('/','_')
}
function Get-Secret([string]$Name, [string]$Prefix) {
  $path = Join-Path $secrets "$Name.txt"
  if ($Rotate -or -not (Test-Path $path)) { [IO.File]::WriteAllText($path, (New-Token $Prefix), $utf8) }
  return ([IO.File]::ReadAllText($path)).Trim()
}
$values = [ordered]@{
  '__MANAGEMENT_KEY__'      = Get-Secret 'management-key'         'cpa-mgmt'
  '__KEY_T3_CODE__'         = Get-Secret 'client-t3-code'         'cpa-t3'
  '__KEY_CLAUDE_CODE_CLI__' = Get-Secret 'client-claude-code-cli' 'cpa-cc'
  '__KEY_CODEX_CLI__'       = Get-Secret 'client-codex-cli'       'cpa-cx'
  '__KEY_OTHER_DEVICES__'   = Get-Secret 'client-other-devices'   'cpa-dev'
}
$configPath = Join-Path $cpaHome 'config.yaml'
if ((Test-Path $configPath) -and -not $Rotate) {
  Write-Host 'config.yaml exists; left unchanged (use -Rotate to regenerate secrets and re-render).'
} else {
  $text = [IO.File]::ReadAllText((Join-Path $PSScriptRoot '..\deploy\config.template.yaml'))
  foreach ($k in $values.Keys) { $text = $text.Replace($k, $values[$k]) }
  [IO.File]::WriteAllText($configPath, $text, $utf8)
  Write-Host "Rendered $configPath"
}
Write-Host 'Management key (save it in your password manager; config.yaml stores only its hash after first start):'
Write-Host $values['__MANAGEMENT_KEY__']
```

`up.ps1`:

```powershell
#requires -Version 5.1
[CmdletBinding()] param([switch]$NoBuild)
$ErrorActionPreference = 'Stop'
$repo    = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$compose = Join-Path $repo 'devyre\deploy\docker-compose.yml'
$envFile = Join-Path $repo 'devyre\deploy\.env'
if (-not (Test-Path $envFile)) { throw "Missing $envFile (copy .env.example)" }
$sha = (git -C $repo rev-parse --short HEAD).Trim()
$env:CPA_VERSION    = (git -C $repo describe --tags --always).Trim()
$env:CPA_COMMIT     = $sha
$env:CPA_BUILD_DATE = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
if (-not $NoBuild) {
  docker compose -f $compose --env-file $envFile build
  if ($LASTEXITCODE -ne 0) { throw 'docker compose build failed' }
  docker tag cpa-devyre:current "cpa-devyre:$sha" | Out-Null
}
docker compose -f $compose --env-file $envFile up -d
if ($LASTEXITCODE -ne 0) { throw 'docker compose up failed' }
$deadline = (Get-Date).AddSeconds(90)
do {
  try { Invoke-WebRequest -Uri 'http://127.0.0.1:8317/management.html' -UseBasicParsing -TimeoutSec 5 | Out-Null; $ok = $true }
  catch { $ok = $false; Start-Sleep -Seconds 2 }
} until ($ok -or (Get-Date) -gt $deadline)
if (-not $ok) { docker logs --tail 80 cpa; throw 'CPA did not serve management.html within 90s' }
Write-Host "CPA up (image cpa-devyre:$sha). Local: http://127.0.0.1:8317/management.html"
```

`tailscale-serve.ps1`:

```powershell
#requires -Version 5.1
$ErrorActionPreference = 'Stop'
$ts = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'
& $ts serve --bg --https=8318 http://127.0.0.1:8317
& $ts serve status
$dns = ((& $ts status --json | ConvertFrom-Json).Self.DNSName).TrimEnd('.')
Write-Host "Panel:   https://$($dns):8318/management.html"
Write-Host "T3 hub:  https://$($dns):8318   (put this in devyre/deploy/.env as CPA_PUBLIC_URL)"
```

`backup.ps1`:

```powershell
#requires -Version 5.1
[CmdletBinding()] param([Parameter(Mandatory)][string]$Destination)
$ErrorActionPreference = 'Stop'
$cpaHome = Join-Path $env:USERPROFILE '.cli-proxy-api'
$stamp = Get-Date -Format 'yyyyMMdd-HHmm'
$out = Join-Path $Destination "cpa-backup-$stamp.zip"
Compress-Archive -Path (Join-Path $cpaHome 'config.yaml'), (Join-Path $cpaHome 'auths'), (Join-Path $cpaHome 'secrets') -DestinationPath $out
Write-Warning "$out contains live OAuth tokens and keys. Keep it on encrypted storage."
```

`sync-upstream.ps1`: a guided script; stop on any failure.

```powershell
#requires -Version 5.1
$ErrorActionPreference = 'Stop'
$repo = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
git -C $repo fetch upstream --tags
git -C $repo switch main; git -C $repo pull --ff-only origin main
$branch = "sync/upstream-$(Get-Date -Format yyyyMMdd)"
git -C $repo switch -c $branch
git -C $repo merge --no-ff upstream/main -m "merge: upstream/main $((git -C $repo rev-parse --short upstream/main).Trim())"
if ($LASTEXITCODE -ne 0) { throw "Merge conflicts: resolve per devyre/README.md 'Conflict hotspots', then rerun the checks." }
Push-Location $repo
try {
  $unformatted = gofmt -l .
  if ($unformatted) { throw "gofmt needed: $unformatted" }
  go build -o "$env:TEMP\cpa-build-check.exe" ./cmd/server; if ($LASTEXITCODE) { throw 'build failed' }
  Remove-Item "$env:TEMP\cpa-build-check.exe" -ErrorAction SilentlyContinue
  go test ./...; if ($LASTEXITCODE) { throw 'tests failed' }
} finally { Pop-Location }
Write-Host "Green. Push $branch and open: gh pr create --repo Devyre/CLIProxyAPI --base main --head $branch"
```

## Appendix E — `devyre/clients/powershell-profile.ps1`

Append this to `$PROFILE`.

```powershell
# --- CLIProxyAPI pool (devyre) ---------------------------------------------------------------
$CpaBaseUrl = 'https://<machine>.<tailnet>.ts.net:8318'   # on the host itself http://127.0.0.1:8317 also works
$cpaKeyFile = Join-Path $env:USERPROFILE '.cli-proxy-api\secrets\client-claude-code-cli.txt'
if (Test-Path $cpaKeyFile) {
  $env:ANTHROPIC_BASE_URL   = $CpaBaseUrl                         # no /v1 for Claude Code
  $env:ANTHROPIC_AUTH_TOKEN = ([IO.File]::ReadAllText($cpaKeyFile)).Trim()
  $env:ENABLE_PROMPT_CACHING_1H = '1'                             # keep 1h cache TTL with token auth (verify, F15)
}
function claude-direct {
  # Own claude.ai login; restores the pool env afterwards.
  $saved = @{ U = $env:ANTHROPIC_BASE_URL; T = $env:ANTHROPIC_AUTH_TOKEN }
  Remove-Item Env:ANTHROPIC_BASE_URL, Env:ANTHROPIC_AUTH_TOKEN -ErrorAction SilentlyContinue
  try { & claude @args } finally { $env:ANTHROPIC_BASE_URL = $saved.U; $env:ANTHROPIC_AUTH_TOKEN = $saved.T }
}
function claudex {
  # Optional: Claude Code harness on GPT via the pool. Needs Codex credentials in CPA; confirm the id with /v1/models.
  $vars = @{ CLAUDE_CODE_SUBAGENT_MODEL = 'gpt-5.6-sol'; CLAUDE_CODE_ALWAYS_ENABLE_EFFORT = '1'; CLAUDE_CODE_MAX_TOOL_USE_CONCURRENCY = '3' }
  foreach ($k in $vars.Keys) { Set-Item "Env:$k" $vars[$k] }
  try { & claude --model gpt-5.6-sol @args } finally { foreach ($k in $vars.Keys) { Remove-Item "Env:$k" -ErrorAction SilentlyContinue } }
}
# ----------------------------------------------------------------------------------------------
```

## Appendix F — `devyre/clients/codex-config.toml` (optional) and `t3-code.md`

```toml
# Append to %USERPROFILE%\.codex\config.toml. Select per client with: codex -c model_provider=cliproxy
[model_providers.cliproxy]
name = "OpenAI"                                   # Codex keys remote compaction etc. off the display name
base_url = "https://<machine>.<tailnet>.ts.net:8318/v1"
env_key = "CLIPROXY_API_KEY"                      # sent as Authorization: Bearer
wire_api = "responses"
supports_websockets = true
requires_openai_auth = true
```

`t3-code.md` restates CLI-1 and CLI-2 as a click-by-click checklist for the user: the hub fields, the Claude instance env table, the "Claude Direct" instance, and the optional custom model `gpt-5.6-sol`.

## Appendix G — Theo-frame fixture (UI-4, UI-2 tests)

The frame clock is **2026-09-11 20:50 America/Los_Angeles**. Values are **remaining %** (used = 100 − remaining). Claude plan: Max for all 5 (profile `account.has_claude_max: true`).

| Claude file (synthetic) | 7-day Fable 5 (hidden in Ledger) | 5-hour | 7-day limit | Resets (local) |
|---|---|---|---|---|
| `claude-taylor@lumen.dev.json` | 58 | 100 (`resets_at: null`) | 79 | 7d and Fable 09/13 13:00 |
| `claude-tess@pixel.gg.json` | 100 | 100 (null) | 100 | 09/15 22:00 |
| `claude-tom@tidal.gg.json` | 100 | 100 (null) | 100 | 09/16 14:00 |
| `claude-tara@tundra.gg.json` | 51 | 99 (resets 09/11 23:50) | 75 | 7d and Fable 09/12 23:00 |
| `claude-tyler@tempo.gg.json` | 100 | 100 (null) | 100 | 09/17 09:00 |

| Other | Window | Remaining | Reset |
|---|---|---|---|
| Codex ×3 (`codex-<8hex>-c…@…json`, plan Pro) | Weekly (secondary) | 17, 0, 0 | earliest 09/14 18:23 |
| xAI ×1 | Weekly | unknown (`--`) | 09/17 17:29 |
| Kimi ×1 | Weekly (`summary`) | 100 | 09/17 15:56 |
| 1 extra non-quota auth file | — | — | makes the sidebar badge read 11 |

| View | Expected Claude cell | Rows |
|---|---|---|
| Theo's original | 409 of 500, label "7-day Fable 5", secondary "7-day limit 454%" | — |
| **Ours** | **454 of 500**, label "7-day limit", segments `[79,100,100,75,100]`, next reset "in 1 day · 09/12, 23:00", secondary "5-hour limit 499%" | `7-day limit │ 5-hour limit` |

## Appendix H — Sources

- Upstream server: https://github.com/router-for-me/CLIProxyAPI (v8.0.13, `d7914af`). Upstream panel: https://github.com/router-for-me/Cli-Proxy-API-Management-Center (v1.25.3, `ee79a79`).
- Ledger port: https://github.com/chhoumann/Cli-Proxy-API-Management-Center/pull/2 and /pull/3 (`dev` @ `24633a8`).
- Prior decision log for the same goal: https://github.com/bandoyer/CLIProxyAPI/issues/2, plus issues #3 (quota signals), #4 (routing plugins), #5 (thread ids), #6 (cache writes), #7 (tailnet serving), #8 (expiring-first rule), #10 (core selector), #11 (WebSockets), #13 (poll intervals), #14 (poller), #15 (client wiring) and #18.
- T3 Code: https://github.com/pingdotgg/t3code: `apps/server/src/usage/cliproxyApi.ts`, `docs/user/usage.md` (Connect a CLIProxyAPI hub), `docs/user/providers-claude.md` (router env).
- Theo: "If you have a Claude sub, watch this" (YouTube `D8PikZ1KhUo`); x.com/theo/status/2080535363207233575 (gpt-5.6-sol in Claude Code in T3 in 3 clicks); x.com/theo/status/2076114415368482854 (claudex tl;dr).
- Claudex recipe: https://explainx.ai/blog/gpt-5-6-sol-claude-code-claudex-setup-guide-july-2026.
- Multi-account enforcement context: https://dev.to/vainamoinen/two-multi-account-claude-code-architectures-one-anthropic-accepts-one-they-ban-2om7.

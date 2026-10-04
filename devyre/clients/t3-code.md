# T3 Code with the CPA pool

A click-by-click checklist for wiring T3 Code to the Devyre CLIProxyAPI (CPA) deployment (plan steps CLI-1 and CLI-2). Do it after the Claude accounts are logged in to CPA (DEP-8).

Have these ready:

- **Public URL:** `https://<machine>.<tailnet>.ts.net:8318`. `devyre\scripts\tailscale-serve.ps1` prints it, and it is `CPA_PUBLIC_URL` in `devyre\deploy\.env`.
- **Management key:** from your password manager. `new-secrets.ps1` printed it once; `new-secrets.ps1 -ShowManagementKey` prints it again.
- **`t3-code` client key:** copy it without displaying it:

  ```powershell
  (Get-Content -Raw "$env:USERPROFILE\.cli-proxy-api\secrets\client-t3-code.txt").Trim() | Set-Clipboard
  ```

## 1. Add the usage hub (Usage -> Limits)

1. Update T3 Code to the latest version.
2. Open **Settings -> Providers -> Usage providers -> Add hub**.
3. Fill in the fields:

   | Field | Value |
   |---|---|
   | Environment | this PC |
   | URL | `https://<machine>.<tailnet>.ts.net:8318` (no path, no `/v1`) |
   | Management key | the plaintext management key |
   | Label | `CPA` |

4. Save.

**Check:** Usage -> Limits -> Claude shows **one pooled card** for the Claude accounts, with the 5-hour and weekly windows and one segment per account. T3 de-duplicates an account it also sees directly. T3's own UI still lists model-scoped windows such as Fable, because T3 is not forked.

The hub only displays quota. It calls `/v0/management/auth-files`, `/v0/management/api-call` and `/v0/management/reset-quota` at most every 5 minutes per environment. It does not route T3's agent traffic; section 2 does that.

## 2. Send T3's Claude traffic through the pool

1. Open **Settings -> Providers -> Claude** and select the default instance.
2. Under **Environment variables**, add:

   | Variable | Value | Notes |
   |---|---|---|
   | `ANTHROPIC_BASE_URL` | `https://<machine>.<tailnet>.ts.net:8318` | No `/v1`. |
   | `ANTHROPIC_AUTH_TOKEN` | the `t3-code` client key | Mark it **Sensitive**. |
   | `ANTHROPIC_API_KEY` | *(empty)* | Set it explicitly empty so no API key is used. |
   | `ENABLE_PROMPT_CACHING_1H` | `1` | Recommended: keeps the 1-hour prompt cache with token auth. Verify. |
   | `ENABLE_TOOL_SEARCH` | `true` | Recommended: keeps tool search on with token auth. Verify. |

3. Save.

**Check:**

- Start a new Claude thread. In the panel's **Logs Viewer**, its requests show the `t3-code` key.
- **Quota -> Ledger** shows usage moving on the picked account.
- Follow-up messages in the same thread keep the same credential (session affinity).

## 3. Add a "Claude Direct" bypass instance

Token auth through the pool turns off claude.ai-only features: Remote Control, claude.ai MCP connectors and `/usage`. Keep a direct instance for those.

1. In **Settings -> Providers -> Claude**, add a second instance named **Claude Direct**.
2. Use the **same binary** and the **same config dir** as the pooled instance.
3. Add **no** proxy environment variables. It uses your own claude.ai login.

Because both instances share the config dir, a thread can switch between them.

## 4. Optional: GPT models in the pooled instance

Only after Codex (ChatGPT) accounts are logged in to CPA:

1. Confirm the model id with the `t3-code` key:

   ```powershell
   $key = (Get-Content "$env:USERPROFILE\.cli-proxy-api\secrets\client-t3-code.txt").Trim()
   (Invoke-RestMethod -Uri 'https://<machine>.<tailnet>.ts.net:8318/v1/models' -Headers @{ Authorization = "Bearer $key" }).data.id
   ```

2. On the pooled Claude instance, use **Add custom model** and enter `gpt-5.6-sol` (or the id the list shows).

## Troubleshooting

| Symptom | Fix |
|---|---|
| Hub says "could not list accounts" | The URL has no path; the key is the plaintext management key (not the hash in `config.yaml`); `management.allow-remote` is `true`; T3's machine can open the ts.net URL. |
| `IP banned ...` | Five wrong management keys from one client IP ban it for 30 minutes. Fix the key, then wait or restart the container (`docker restart cpa`). |
| Claude threads fail while the PC sleeps or Docker is down | Use the **Claude Direct** instance until CPA is back. |
| Remote Control, connectors or `/usage` missing | Expected through the pool; use **Claude Direct**. |

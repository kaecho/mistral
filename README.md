<div align="center">

# Mistral

Connect Mistral to CLIProxyAPI (CPA). Clients keep using CPA's address and key.

[![Release](https://img.shields.io/github/v/release/kaecho/mistral?style=flat&color=2563eb)](https://github.com/kaecho/mistral/releases/latest)
[![Platforms](https://img.shields.io/badge/platforms-Linux%20%7C%20macOS%20%7C%20Windows-64748b)](https://github.com/kaecho/mistral/releases)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License](https://img.shields.io/github/license/kaecho/mistral?color=16a34a)](LICENSE)

[中文](README.zh.md) · [Releases](https://github.com/kaecho/mistral/releases) · [Issues](https://github.com/kaecho/mistral/issues)

</div>

## What it does

- Accepts OpenAI Chat Completions, Responses, Claude, and Gemini requests and converts them to Mistral chat completions (the `/openai/v1` compatibility facade by default).
- Rewrites Mistral thinking blocks into the field each protocol expects, so clients do not have to parse arrays inside `delta.content`.
- Manages multiple Mistral API keys. Each key can have a priority, be disabled, or be tested on its own.
- Fails over to the next credential when the upstream rejects the current one, and keeps recently failed keys out of the first attempt.
- Fetches the upstream model list and creates mappings from selected models, so model IDs do not have to be typed by hand.
- Sends a small request for a single mapping to check the key, model ID, and reasoning setting together.
- Records request logs and statistics: first-byte time, total time, output speed, cache hits, and failover attempts.
- Maintains a stable `prompt_cache_key` and mirrors it in the `x-affinity` header so Mistral's load balancer keeps one conversation on the cache node that already holds its prefix. The key comes from the client when present, otherwise from the conversation root (instructions plus first user message), so turns and compaction replays of the same history keep hitting the same node.
- Cancels streaming requests that stop producing bytes for too long.
- Optionally ignores CPA's global proxy when the host transport cannot reach the upstream directly.
- Reads the included monthly allowance per credential from Mistral's billing API (`/billing/subscription` and `/billing/budget`), shows API and Vibe Code usage in the console, routes around a credential when its allowance is spent, and brings it back automatically at the reset date.
- Runs the console in English or Chinese, follows the CPA language on first load, and keeps a manual language switch.

## Install

The plugin id is `mistral`. It needs a CPA build with dynamic library plugins and JSON ABI v1.

1. Install **Mistral** from the CPA plugin marketplace, or download the archive for your platform from [Releases](https://github.com/kaecho/mistral/releases).
2. Put the library in CPA's `plugins/<os>/<arch>/` directory and keep the archive's filename:
   - Linux amd64 / arm64: `mistral.so`
   - macOS arm64 / amd64: `mistral.dylib`
   - Windows amd64: `mistral.dll`
3. Restart CPA. Replacing the file without a restart leaves the old library loaded.
4. Add `auths/mistral.json`:

```json
{"type":"mistral"}
```

Without this auth file, routing returns `auth_not_found`. The file does not need an API key. The plugin manages keys itself.

## Getting started

Open the **Mistral** menu in the CPA management page, or go to:

```text
http://<cpa>/v0/resource/plugins/mistral/console
```

The console tries to reuse the CPA management session. It reads `cli-proxy-auth` from the browser when CPA is opened directly, and also reads the same-origin parent page when opened from the plugin menu. If neither source is available, the page asks you to open it from the Mistral menu. It does not prompt for a key.

First-run order:

1. Add a Mistral API key under Credentials, then click Check to confirm `GET /models` succeeds. If you do not have a key, click Create Vibe API key to open `https://chat.mistral.ai/code/extensions`.
2. Under Models, choose a credential, click Fetch upstream models, select what you need, and add them.
3. Point your client at CPA's address and CPA client key. Use the client-side model name from the mapping table.

## Configuration

Saving in the console writes the whole `plugins.configs.mistral` block back to CPA's config file. The old format with one `api-key` and a `models` list still loads. The first console save rewrites it into the structure below.

```yaml
plugins:
  configs:
    mistral:
      enabled: true
      priority: 100
      base-url: "https://api.mistral.ai/v1"
      console-url: "https://console.mistral.ai"
      endpoint: "auto"
      log-retention: 200
      stream-idle-seconds: 0
      failover: true
      ignore-proxy: false
      cooldown-seconds: 60
      quota-monitoring: true
      quota-refresh-seconds: 60
      credentials:
        - id: "primary"
          label: "Primary"
          api-key: "mstrl_..."
          disabled: false
          priority: 10
        - id: "backup"
          label: "Backup"
          api-key: "mstrl_..."
          disabled: false
          priority: 5
      models:
        - id: "deepseek-v4.1-flash"
          upstream-id: "deepseek-v4-1-flash-1017"
          reasoning: ""
        - id: "glm-5.3"
          upstream-id: "zai-glm-5-3"
          reasoning: "max"
```

| Field | Meaning |
| --- | --- |
| `base-url` | Mistral API base URL, including `/v1`. Defaults to `https://api.mistral.ai/v1`. Model listing and billing always use this address. |
| `endpoint` | `auto` (default), `openai`, or `native`. `auto` uses `https://api.mistral.ai/openai/v1/chat/completions` on the official host and keeps the native `/chat/completions` path on a custom host; `openai` forces the compatibility facade; `native` forces the old route. The facade accepts `store=false`, `developer`, `reasoning_content`, `seed`, `user`, `metadata`, `max_completion_tokens` and similar fields the native route rejects with 422, and returns thinking as `reasoning_content` directly. |
| `console-url` | Console origin used to read the key scope from `/api/vibe/whoami`. Defaults to `https://console.mistral.ai`. Empty disables scope detection, so nothing is paused automatically. |
| `log-retention` | Number of request log entries to keep, from 10 to 5000. |
| `stream-idle-seconds` | Stream idle cutoff. 0 disables it. Above 0, an upstream request is cancelled after this many seconds without new bytes. |
| `failover` | When enabled, a failed upstream attempt moves to the next credential by priority. When disabled, only the first credential in the current order is used. |
| `ignore-proxy` | When enabled, the plugin connects to Mistral directly and bypasses CPA's global proxy. Off by default. |
| `cooldown-seconds` | Base cooldown after a failure, from 0 to 3600. 0 disables cooldown, but failover still moves through the list. |
| `quota-monitoring` | When enabled (default), the plugin reads the plan allowance per credential and routes around a key whose allowance is spent. |
| `quota-refresh-seconds` | Background allowance check interval, from 10 to 3600. Defaults to 60. |
| `credentials[].priority` | Higher numbers are tried first. |
| `credentials[].disabled` | When true, the credential is not scheduled. |
| `models[].id` | Model name exposed to clients. |
| `models[].upstream-id` | Model ID sent to Mistral. |
| `models[].reasoning` | Empty passes through the client's `reasoning_effort`. A value such as `low`, `medium`, `high`, `xhigh`, or `max` forces an override. |

If a gateway only implements the native `/v1` route, set `endpoint` to `native`; the same applies if `/openai/v1` answers 404 on the official host.

## Calling a model

With the mapping `deepseek-v4.1-flash` → `deepseek-v4-1-flash-1017`, send the client-side name as `model`:

```json
{
  "model": "deepseek-v4.1-flash",
  "messages": [{"role": "user", "content": "Hello"}],
  "stream": true
}
```

The plugin replaces `model` with the upstream ID and keeps `reasoning_effort`. The default `endpoint: auto` uses the OpenAI-compatible facade on the official host, so `store=false`, `developer`, `reasoning_content` and similar fields pass through. Only `native` removes what the old route rejects (`store`, plus `reasoning_content`, `reasoning`, and `refusal` on assistant messages) and folds old thinking content back into the content array Mistral understands.

## Thinking blocks

The compatibility endpoint returns thinking as `reasoning_content`, and streaming deltas as `delta.reasoning_content`. The native endpoint returns thinking content as elements in a `delta.content` array, such as `{"type":"thinking", ...}`; the plugin splits those into `reasoning_content`. Both paths hand CPA the same output format, which CPA then converts into the field the client protocol uses:

| Client protocol | Thinking field |
| --- | --- |
| OpenAI Chat Completions | `reasoning_content` |
| Claude | `thinking` |
| Responses | `reasoning` |
| Gemini | `thought: true` |

Thinking is triggered by preserving `reasoning_effort`. OpenAI Chat requests prefer `OriginalRequest` because CPA's `Payload` can drop the field during preprocessing. When a mapping has a non-empty `reasoning`, the plugin overrides the client's setting.

## Credential scheduling and cooldown

Credentials are ordered by `priority` from high to low. Healthy credentials come first, cooling credentials come after them, and disabled or keyless credentials are skipped. A request that follows a failure therefore tries the other keys first. If every key is cooling, the highest-priority one is still attempted, so a short outage does not block all traffic.

These responses move to the next credential:

- `401`, `403`: invalid key or missing permission
- `408`, `425`, `429`: timeout, too early, rate limit
- `5xx`: upstream error
- Transport errors, including connection failures and interrupted connections

`400`, `404`, and `422` are not retried because another key would receive the same error. A request tries at most three credentials.

After a failure the cooldown is `cooldown-seconds × 2^(consecutive failures - 1)`, capped at 15 minutes. Any success clears that key's failure count and cooldown. The console also has a Reset cooldown action.

Failover for streaming requests happens while opening the upstream connection. Once the upstream returns `200` and starts streaming, a later disconnect is logged and the stream ends. The plugin does not replay a partial response.

## Model discovery and tests

Check under Credentials calls `GET /models`, confirms the key works, and reports the model count and latency.

Fetch upstream models under Models uses the same endpoint and lists the upstream catalog. Select models and add them in one batch. Upstream IDs that already have a mapping are marked as added. The client name defaults to the upstream ID and can be changed later.

Test sends a non-streaming request for one mapping with `max_tokens: 64` and the mapping's reasoning setting. It checks the key, model ID, and parameters together and counts toward your Mistral bill. Test requests are logged with a `probe` flag and excluded from statistics.

## Quota monitoring

The plugin reads the same billing endpoints the official Vibe client uses:

```text
GET {base-url}/billing/subscription
GET {base-url}/billing/budget
GET {console-url}/api/vibe/whoami
```

`/billing/subscription` names the plan (`EDU`, `INDIVIDUAL`, `TEAM`, ...) and whether pay-as-you-go is on. `/billing/budget` returns the two included monthly allowances:

- `api_budget`: ordinary API and Studio spend.
- `vibe_budget`: Vibe Code spend.

Which one a key uses is not a property of the model: Mistral ties the billing category to the key scope. A Vibe-scope key (`plan_type` `CHAT` or `MISTRAL_CODE` in `/whoami`) charges the Vibe Code allowance for every request, including normal chat models; a Studio API-scope key (`plan_type` `API`) charges the API allowance. The plugin reads `/api/vibe/whoami` with the same key to learn that scope.

Both allowances are stored per credential and shown under Credentials (used / included, a progress bar, the plan name, the key scope, and the reset time) and as a compact summary on Overview. The applicable allowance is marked. The page repaints from the cached figures every 30 seconds, the background check refreshes billing on `quota-refresh-seconds`, and the Refresh allowance buttons read billing immediately.

A credential is taken out of rotation when its applicable allowance reaches 100% and pay-as-you-go is off. This pauses the whole key, not one model category, because the key cannot move its traffic to the other allowance. Nothing is written to the CPA config: a manual disable stays manual, and the automatic pause is derived from the last billing read. Once `reset_at` passes, the credential is eligible again and the next background check confirms the new allowance. When the scope cannot be determined (whoami unreachable, or both allowances present with no scope), the plugin keeps serving; if only one allowance is present it falls back to that one. A billing read failure or a key without permission to see billing never disables anything.

The working state and the last read per credential are cached in:

```text
/CLIProxyAPI/plugin-data/mistral/quota.json
```

so a CPA restart starts from the previous figures. Background checks do not use the management session, so they connect directly; the console refresh uses the host bridge and follows `ignore-proxy` like every other plugin call. If the deployment cannot reach Mistral without CPA's proxy, the automatic part still runs whenever a client request or the console triggers a refresh.

## Request logs and statistics

Logs are written inside the container to:

```text
/CLIProxyAPI/plugin-data/mistral/requests.jsonl
```

Each entry contains time, client model, upstream model, credential actually used, status, attempt count, first-byte time, total time, input tokens, output tokens, cached tokens, output speed, and error. Older entries without the newer fields read as zero.

The Logs tab shows the latest 50, 100, or 200 entries, filters by status, and lists input, output, and cached tokens for each request. Times are shown in seconds with one decimal. Statistics cover real client traffic only and exclude management tests. Output speed is completion tokens divided by generation time. Cache hit rate is cached tokens divided by input tokens. Both are left empty when Mistral does not return usage on a stream.

## Stream cutoff

When `stream-idle-seconds` is above 0, the plugin opens a cancellable upstream operation and checks the time of the last received byte every second. If the threshold passes with no new bytes, it cancels the upstream request and closes the downstream stream.

The first packet for the `max` setting often takes more than 20 seconds. Do not set this value too low. 0 disables the cutoff, which is the default.

## Build from source

You need Go 1.26 and a C compiler. Linux amd64 can use the repository's Dockerfile:

```bash
docker build --platform linux/amd64 -f Dockerfile.build --output type=local,dest=dist .
```

The result is `dist/mistral.so`. Other platforms are built by the [release workflow](.github/workflows/release.yml).

## Release

The version in `marketplace/registry.json` under `plugins[0].version` is authoritative, and it must equal `pluginVersion` in `cmd/mistral/version.go`.

Push a tag named `v` plus that version, for example `v0.3.0`. Actions will:

1. Run tests and build the shared library on each platform.
2. Package linux amd64, linux arm64, darwin arm64, darwin amd64, and windows amd64.
3. Write `checksums.txt`, create or update the GitHub release, and mark it latest.

A tag that does not match the version fails the workflow before anything is published.

## License

[MIT License](LICENSE).

# commandcode-bridge

OpenAI-compatible API bridge for **Command Code** (commandcode.ai) — a CLI coding agent service. Reverse-engineered from `command-code` CLI v1.38.2 (`dist/cli.mjs`, npm `command-code`).

Exposes `/v1/chat/completions` (stream + non-stream) and `/v1/models` backed by Command Code's `/alpha/generate` gateway API, so any OpenAI-SDK client (Hermes Agent, Continue, curl, …) can use its 62-model catalog.

## Features

| Feature | Notes |
|---|---|
| OpenAI Chat Completions | stream (SSE) + non-stream |
| Tool calling | OpenAI `tools[]` ↔ CC wire `tools[]` / `tool-call` events |
| `/v1/models` | 60 models from `cmd --list-models` (v1.38.2) |
| System prompts | all `system`/`developer` messages joined → `params.system` (never dropped) |
| Error mapping | upstream 400 insufficient-credits → HTTP 402; 401/429 mapped |
| accept_any_model | unknown model ids pass through to upstream |
| Single binary | pure Go, no deps |

## Usage

```bash
CMDC_API_KEY="user_xxx" ./commandcode-bridge
# listening on 0.0.0.0:18766

curl http://127.0.0.1:18766/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"minimax/minimax-m3-free","messages":[{"role":"user","content":"hi"}]}'
```

### Env vars

| Var | Default | Notes |
|---|---|---|
| `CMDC_API_KEY` | (required) | Command Code API key (`cmd login` → paste key) |
| `CMDC_BASE_URL` | `https://api.commandcode.ai` | API root |
| `CMDC_LISTEN` | `0.0.0.0:18766` | listen address |
| `CMDC_CLI_VERSION` | `1.38.2` | sent as `x-command-code-version` |
| `CMDC_WORKDIR` | `/home/agentuser` | sent in `config.workingDir` |

## Upstream protocol (RE notes)

- `GET /alpha/whoami` — auth check, Bearer key
- `GET /alpha/billing/credits` — plan/credit status
- `POST /alpha/generate` — chat completion, NDJSON response stream

Request body (wire format):

```json
{
  "config": {"workingDir":"…","date":"YYYY-MM-DD","environment":"linux",
             "structure":[],"isGitRepo":false,"currentBranch":"",
             "mainBranch":"main","gitStatus":"…","recentCommits":[]},
  "memory": null, "taste": null, "skills": null,
  "permissionMode": "standard", "mode": "agent",
  "params": {"model":"…","messages":[…],"tools":[],
             "system":"…","max_tokens":64000,"stream":true}
}
```

Headers: `Authorization: Bearer <key>`, `x-command-code-version`, `x-cli-environment: production`.

Stream events (one JSON per line): `text-delta`, `reasoning-start/-delta/-end`, `tool-call`, `tool-result`, `finish` (with `totalUsage`), `error`, `abort`.

## Pitfalls

- **`params.tools` must be `[]`, never `null`** — upstream 400 validation error otherwise.
- **`threadId` must be omitted entirely** — `null` is rejected (expected string).
- **`config` fields are all required** (strings non-null, `structure` array, `isGitRepo` bool) — the 400 error enumerates exactly which.
- **Free models still require credits** — an account with zero credits gets `400 insufficient credits` even for `-free` models. Minimum plan: Go ($1/mo).
- Auth via `COMMAND_CODE_API_KEY` env var works for the CLI too — the `cmd login` TUI hangs under non-pty submit, so env-var auth is the reliable path.

## License

MIT

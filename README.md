# claude-mimir

Run Cubbit's **Mimir** models as real [Claude Code](https://claude.com/claude-code) subagents, while the main conversation stays on Claude and on your Anthropic subscription.

Claude Code talks to a single `ANTHROPIC_BASE_URL`. `claude-mimir` puts a small local gateway (`it-router`, one Go binary, standard library only) at that URL. The gateway routes each request by its `model`:

```
Claude Code --> it-router :4000 --+--> cubbit/*        --> Mimir's Anthropic-compatible endpoint ($MIMIR_ANTHROPIC_URL)
                                  +--> anything else  --> api.anthropic.com, byte for byte
```

The Claude side is never touched. The Mimir side gets the handful of fixes it needs to behave like the Anthropic API Claude Code expects.

## Why a gateway

Mimir has an Anthropic-compatible endpoint, but Claude Code can't use it as is. Claude Code sends fields and message shapes Mimir rejects, Mimir streams events Claude Code rejects, and a session can only have one base URL. Without a gateway you would have to choose between Claude and Mimir for the whole session. With it, a Claude session can hand work to Mimir subagents, and those subagents can use every tool (read, edit, bash, …).

## Features

- **Per-request routing** by model name: `cubbit/*` to Mimir, everything else to Anthropic.
- **Claude leg untouched:** same headers (including your subscription login), same body, response streamed back undecoded. Nothing is rewritten, retried or buffered.
- **Mimir leg shims**, each found by probing Mimir with real Claude Code requests (details in [Notes on the upstream API](#notes-on-the-upstream-api)):
  - credential swap: your Claude login never reaches Mimir;
  - mid-conversation system messages folded into user turns;
  - fields Mimir rejects dropped;
  - stream repairs;
  - a pinned reasoning effort for `mimir-pro`;
  - a local `count_tokens`;
  - error messages rewritten into the shapes Claude Code acts on.
- **Metadata-only request log** (`it-router.jsonl`): route, model, status, timings, message and tool counts, repairs applied. Never prompt text or credentials.
- **Fail-fast config:** it refuses to start without the key or the endpoint, rather than failing on the first request.

## Quick start

Requires Go (to build) and Claude Code. The deploy scripts target macOS (launchd); on Linux, run `it-router` directly or under systemd.

```sh
git clone https://github.com/didimelli/claude-mimir && cd claude-mimir

# 1. Configuration: the key and the endpoint
mkdir -p ~/.config/it-gateway
cp .env.example ~/.config/it-gateway/env && chmod 600 ~/.config/it-gateway/env
$EDITOR ~/.config/it-gateway/env          # set MIMIR_API_KEY and MIMIR_ANTHROPIC_URL

# 2. Build and install it-router and run.sh into ~/.config/it-gateway/
deploy/deploy.sh --dry-run && deploy/deploy.sh

# 3. Keep it running with launchd
sed "s|@HOME@|$HOME|g" deploy/io.cubbit.it-router.plist.template > ~/Library/LaunchAgents/io.cubbit.it-router.plist
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.cubbit.it-router.plist
curl -s http://127.0.0.1:4000/router/health   # {"anthropic":"…","mimir":"direct (…)","router":"ok"}

# 4. The launcher
cp deploy/claude-mimir ~/.local/bin/ && chmod +x ~/.local/bin/claude-mimir

# 5. The subagents
cp agents/mimir*.md ~/.claude/agents/
```

## Usage

**1. Start Claude Code with `claude-mimir` instead of `claude`.** It is `claude` with `ANTHROPIC_BASE_URL` pointed at the gateway, and it accepts the same arguments (`claude-mimir -c`, `claude-mimir -p "…"`, …). Plain `claude` keeps going straight to Anthropic, so only the sessions where you want Mimir subagents depend on the gateway.

> Don't put `ANTHROPIC_BASE_URL` in the `env` block of `~/.claude/settings.json`. That block overrides the process environment, so it would route every session through the gateway.

**2. Have Mimir subagents.** A subagent runs on Mimir when its `model:` is a `cubbit/` model. The three in `agents/` cover the common cases:

| Agent | Model | Use it for |
|---|---|---|
| `mimir-pro` | `cubbit/mimir-pro` | hard reasoning, large-context analysis, substantial multi-step work (~900k context) |
| `mimir` | `cubbit/mimir` | routine delegated work: reading code, summarising, small edits (~900k context) |
| `mimir-slim` | `cubbit/mimir-slim` | small, fast tasks: lookups, short summaries, quick checks (~229k context) |

A minimal agent definition, for writing your own (`~/.claude/agents/<name>.md`):

```markdown
---
name: mimir-pro
description: Delegate hard reasoning or large multi-step work to Cubbit's Mimir Pro model.
model: cubbit/mimir-pro
---

You are a subagent running on Mimir Pro, delegated a task by Claude Code. Work autonomously,
verify what you claim, and finish with a concise report.
```

Restart Claude Code once after adding agents, so a running session sees them.

**3. Delegate to them.** Ask for it in plain words ("use the mimir-pro agent to read the storage package and summarise how compaction works"), or let Claude pick an agent from its description. The main conversation stays on Claude, while every turn of the subagent (its reasoning and its tool calls) runs on Mimir. Subagents can run in parallel and in the background like any other subagent.

**4. Check it is really Mimir.** `tail -f ~/Library/Logs/it-router.jsonl` while a subagent works shows lines with `"route":"mimir"`, `"model":"cubbit/…"` and `"status":200`. The main conversation's lines say `"route":"anthropic"`.

**What to expect**

- Mimir returns no thinking blocks and does no prompt caching, so long subagent conversations are slower than on Claude.
- Mimir streams report no token usage, so Claude Code's usage and cost figures don't include Mimir turns.
- `mimir-pro` sometimes spends a whole turn reasoning without producing anything. Claude Code then reports "Output token limit hit" and continues; see the notes below.
- A bigger pattern that works well: a Mimir implementer subagent that writes the code test-first, and fresh Claude subagents that review it in rounds. Mimir does the volume of the work, and Claude keeps the judgement.

## Configuration

Everything comes from the environment. `deploy/run.sh` (what launchd runs) first reads `~/.config/it-gateway/env` if it exists, because launchd passes almost no environment.

| Variable | Required | Meaning |
|---|---|---|
| `MIMIR_API_KEY` | yes | Mimir key, sent as `x-api-key`. Without it, `run.sh` reads `MIMIR_API_KEY_FILE` (default `~/.config/opencode/mimir-key`) |
| `MIMIR_ANTHROPIC_URL` | yes | Mimir's Anthropic-compatible endpoint, for example `https://<mimir host>/anthropic` (`--mimir-url` overrides it) |
| `IT_ROUTER_PORT` | no | listen port, default `4000` (the listener binds to `127.0.0.1` only) |
| `IT_ROUTER_LOG` | no | request log, default `~/Library/Logs/it-router.jsonl` |

`it-router` flags:
- `--port`, `--host`, `--log`, `--mimir-url`;
- `--anthropic`, the upstream for non-Mimir models;
- `--prefix`, the model prefix routed to Mimir (repeatable, default `cubbit/`);
- `--mimir-effort model=effort`, see below.

## Operating

- **Health:** `curl -s http://127.0.0.1:4000/router/health`. Liveness: `GET /health/liveliness`.
- **Deploy:** `deploy/deploy.sh` builds and installs `it-router` and `run.sh`. It backs up what it replaces to `~/.config/it-gateway/backup-<timestamp>/`, `--dry-run` shows what would change, and it never restarts anything.
- **Restart:** `launchctl bootout gui/$(id -u)/io.cubbit.it-router`, then **wait until `launchctl print gui/$(id -u)/io.cubbit.it-router` fails**, then `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.cubbit.it-router.plist`. Bootstrapping too early fails with an I/O error. The gap is about 2 seconds, and sessions retry across it.
- **Rollback:** restore `it-router` and `run.sh` from the latest backup directory, then restart.
- **Logs:**
  - `~/Library/Logs/it-router.log` is the process output.
  - `~/Library/Logs/it-router.jsonl` has one line per `/v1/messages` request: route, model, status, `upstream_host`, ttfb and total ms, message and tool counts, `orphan_tool_results`, `first_hash`, the upstream `request_id`. On the Mimir leg it also has `folded`, `dropped_fields`, `effort` and `stream_fixes`.
  - `error: client went away` means the client left before the response was complete.

## Notes on the upstream API

What the Mimir leg changes, and why. Each item was found by probing Mimir with real Claude Code requests.

- **Credentials and headers.** The Claude login is dropped and `MIMIR_API_KEY` is sent as `x-api-key`. Only `content-type`, `accept`, `anthropic-version` and `user-agent` are forwarded.
- **Mid-conversation system messages.** Claude Code sends `role: system` entries mid-conversation, which `mimir-slim` rejects ("System message must be at the beginning").
  - They become user messages **in place**: text only, wrapped in `<system-reminder>`, with their `tool_addition` blocks dropped.
  - No earlier message changes, so the prompt prefix stays stable.
  - An entry wedged between a `tool_use` and its `tool_result` moves after the result.
- **Dropped fields:** `metadata` (Anthropic account identifiers), `context_management`, `thinking` and `output_config`. Effort is validated per model and differently for each (`slim` rejects `high`/`max`, `mimir` rejects `medium`, `thinking: adaptive` alone fails on `slim`).
- **Pinned effort for `mimir-pro`.** Left to its default, `mimir-pro` sometimes reasons until `max_tokens` and returns an empty turn after about 3 minutes.
  - On a probe prompt, sending nothing gave 4 runaways in 7 runs. Adaptive thinking with effort `low` or `high` gave 0. `budget_tokens` and `thinking: disabled` made no difference.
  - So the gateway sends `thinking: {type: adaptive}` with `output_config: {effort: high}` for `cubbit/mimir-pro`.
  - `--mimir-effort model=effort` changes it (repeatable), and `--mimir-effort none` turns it off.
  - On long agent contexts it reduces runaways but doesn't remove them.
- **Stream repairs.** Mimir's SSE streams have three kinds of defect that Claude Code rejects, either silently (it retries the turn without streaming, paying twice) or as "The response stream was malformed" with a lost tool call. The gateway fixes them and counts each fix in `stream_fixes`.
  - **Reasoning and stop reason:** `thinking_delta` events arrive inside `text` blocks, so they are dropped. `stop_reason` is `end_turn` even when a `tool_use` was streamed, so it is corrected to `tool_use`.
  - **Parallel tool calls:** blocks interleave, one block is stopped twice while another is never stopped, and late deltas arrive for closed blocks. The gateway closes the right block, adds missing stops and drops late deltas.
  - **Truncated streams:** at `max_tokens` the stream ends without `message_delta`/`message_stop`. On a clean end the gateway synthesizes them with `stop_reason: max_tokens`. An upstream error is never turned into a clean end.
- **`count_tokens`** is not supported by Mimir (500), so the gateway answers it locally with an estimate.
- **Errors Claude Code acts on are rewritten:**
  - an unknown model (`500`, which Claude Code would retry) becomes `404`;
  - a context overflow becomes Anthropic's `prompt is too long: M tokens > N maximum`;
  - an empty `401` gets a message.
- **Other Mimir behavior:** it sends no `ping` events, and streams carry no token usage.

## Security notes

- The listener binds `127.0.0.1` only.
- Your Anthropic login goes only to Anthropic. The Mimir leg never forwards it.
- The Mimir key comes only from the environment (or a key file). It is never logged, and `env` is git-ignored; keep it mode 600.
- The request log holds metadata only: no message text, no tool input or output, no credentials.

## Development

```sh
go test -race ./...    # fold table/property tests, fuzz target FuzzFold, Mimir leg against fake upstreams,
                       # stream repair (including random-stream properties), fail-fast config, clean SIGTERM exit
```

`FOLD_REAL_BODIES=<dir of captured *.body files> go test -run TestFoldOnRealBodies` checks the fold on real captured requests. Don't commit those files, since they contain real prompts.

`tools/`:
- **`devrouter.sh`** runs dev builds on spare ports 4010–4019, tracked by pid file. It never touches the gateway on 4000.
- **`validate_stream.py`** is a strict checker for Anthropic-format SSE streams.
- **`effort_probe.py`** measures how much Mimir Pro reasons under each reasoning setting.

CI runs `gofmt`, `go vet`, `go test -race` and syntax checks of the scripts.

## History

This started in a private dotfiles repository and moved here with its history. The earlier gateway was LiteLLM plus a Python router. It occasionally sent Anthropic a request with the front of the conversation cut off (`unexpected tool_use_id found in tool_result blocks`), and that led to this design: never touch the Claude leg.

## License

MIT, see [LICENSE](LICENSE).

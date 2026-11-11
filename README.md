# claude-mimir

Run Cubbit's **Mimir** models as real [Claude Code](https://claude.com/claude-code) subagents, while the main conversation stays on Claude and on your Anthropic subscription.

Claude Code talks to a single `ANTHROPIC_BASE_URL`. `claude-mimir` puts a small local gateway (`it-router`, one Go binary, standard library only) at that URL. The gateway routes each request by its `model`:

```
Claude Code --> it-router :4000 --+--> cubbit/*        --> Mimir: /v1 chat completions, translated (MIMIR_API=openai)
                                  |                        or /anthropic, passed through and repaired (MIMIR_API=anthropic)
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
  - an explicit reasoning effort per model;
  - earlier reasoning replayed to `mimir` and `mimir-pro` (`openai` mode);
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
$EDITOR ~/.config/it-gateway/env          # set MIMIR_API_KEY, MIMIR_API and the endpoint for it

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
| `mimir-pro` | `cubbit/mimir-pro` | hard reasoning, large-context analysis, substantial multi-step work (1M context) |
| `mimir` | `cubbit/mimir` | routine delegated work: reading code, summarising, small edits (1M context) |
| `mimir-slim` | `cubbit/mimir-slim` | small, fast tasks: lookups, short summaries, quick checks (262k context) |

The windows are what Mimir's `/v1` reports (`max_model_len`); `/anthropic` rejects somewhat earlier (about 900k and 229k). Claude Code doesn't know these models, so on its own it assumes 200k and compacts a Mimir subagent there. `claude-mimir` sets `CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576`, which applies only to model IDs Claude Code doesn't recognise, so Mimir subagents get the full window and Claude models keep theirs. `mimir-slim` overflowing its smaller window is fine: the gateway turns Mimir's rejection into `prompt is too long`, and Claude Code compacts and retries.

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

## Headroom (optional, experimental)

[Headroom](https://github.com/headroomlabs-ai/headroom) is a local proxy that compresses prompts (mostly large tool results) before they reach the model. It works in front of both plain `claude` and `claude-mimir`. Nothing in this repo depends on it and nothing here starts it: you run it yourself, and only sessions you point at it use it.

```
plain claude  --> headroom :8787 --> api.anthropic.com
claude-mimir  --> headroom :8788 --> it-router :4000 --+--> cubbit/*   --> Mimir
                                                       +--> the rest  --> api.anthropic.com
```

For `claude-mimir`, Headroom sits in front of the gateway, so it compresses Mimir requests too. It works on the Anthropic-format request, before the gateway translates it for `/v1` (`MIMIR_API=openai`).

**Install**

```sh
uv tool install --python 3.13 "headroom-ai[all]"
headroom --version
```

**Launch the two proxies**

```sh
export HEADROOM_TELEMETRY=off HEADROOM_BEACON=off DO_NOT_TRACK=1

# plain claude: Headroom straight to Anthropic
nohup headroom proxy --port 8787 --no-subscription-tracking \
  --protect-tool-results Read,Edit,Write,Bash > ~/Library/Logs/headroom-8787.log 2>&1 &

# claude-mimir: Headroom in front of it-router
ANTHROPIC_TARGET_API_URL=http://127.0.0.1:4000 \
nohup headroom proxy --port 8788 --no-subscription-tracking \
  --protect-tool-results Read,Edit,Write,Bash > ~/Library/Logs/headroom-8788.log 2>&1 &

curl -s http://127.0.0.1:8787/livez; curl -s http://127.0.0.1:8788/livez
```

- `ANTHROPIC_TARGET_API_URL` is how the second proxy forwards to the gateway instead of `api.anthropic.com`. It is read from the environment, and `headroom proxy --help` does not list it.
- `--protect-tool-results` keeps compression away from file reads and command output, which an agent that edits code needs exact. `--no-subscription-tracking` stops Headroom's own usage polling.
- The proxies bind `127.0.0.1` only. They are plain background processes: nothing restarts them and they do not survive a reboot. The gateway must already be running for the `8788` chain.
- Stop them with `pkill -f "headroom.cli proxy"`. `pkill -f "headroom proxy"` matches nothing, because the process runs as `python -m headroom.cli proxy`.

**Opt in per session**

Set the base URL on the command line:

```sh
ANTHROPIC_BASE_URL=http://127.0.0.1:8787 claude
```

For `claude-mimir`, the launcher hardcodes `:4000`, so its `export` would override a variable set on the command line. Run Claude Code directly with the same settings the launcher uses:

```sh
env -u ANTHROPIC_CUSTOM_HEADERS \
  ANTHROPIC_BASE_URL=http://127.0.0.1:8788 \
  CLAUDE_CODE_MAX_CONTEXT_TOKENS=1048576 \
  claude
```

As in [Usage](#usage), don't put `ANTHROPIC_BASE_URL` in the `env` block of `~/.claude/settings.json`. Sessions started without the variable are unaffected.

**Check it**

- `headroom savings`, or `PERF` lines (`tok_before`, `tok_after`, `tok_saved`) in the proxy logs.
- `~/Library/Logs/it-router.jsonl` still shows `"route":"mimir"` for subagents and `"route":"anthropic"` for the main conversation.

**What was tested, and what was not.** Checked with Headroom 0.44.0: a non-streaming Mimir request with a large tool result (about 23k to 17k tokens), a streamed tool call and a streamed follow-up on `/v1`, and a `claude -p` turn with the Claude subscription login through both chains. Not tested: a long agent session, whether `--protect-tool-results` keeps an implementer's file reads exact, and Headroom's extra tool injection (a small request cost about 130 more input tokens through it). Compression also adds latency, several seconds on large payloads.

## Configuration

Everything comes from the environment. `deploy/run.sh` (what launchd runs) first reads `~/.config/it-gateway/env` if it exists, because launchd passes almost no environment.

| Variable | Required | Meaning |
|---|---|---|
| `MIMIR_API_KEY` | yes | Mimir key, sent as `x-api-key`. Without it, `run.sh` reads `MIMIR_API_KEY_FILE` (default `~/.config/opencode/mimir-key`) |
| `MIMIR_API` | no | `anthropic` (default for now) or `openai`; see [Two ways to reach Mimir](#two-ways-to-reach-mimir) (`--mimir-api`) |
| `MIMIR_OPENAI_URL` | with `openai` | Mimir's OpenAI-compatible base URL, for example `https://<mimir host>/v1` (`--mimir-url` overrides it) |
| `MIMIR_ANTHROPIC_URL` | with `anthropic` | Mimir's Anthropic-compatible endpoint, for example `https://<mimir host>/anthropic` (`--mimir-url` overrides it) |
| `IT_ROUTER_PORT` | no | listen port, default `4000` (the listener binds to `127.0.0.1` only) |
| `IT_ROUTER_LOG` | no | request log, default `~/Library/Logs/it-router.jsonl` |
| `IT_RUNAWAY_GUARD` | no | `log` (default) records runaway replies in the request log, `off` turns that off (`--runaway-guard`); see [Runaway replies](#runaway-replies) |
| `IT_MAX_REPLY_TEXT` | no | visible reply bytes from which a reply counts as a runaway, default `24576` (`--max-reply-text`) |

`it-router` flags:
- `--port`, `--host`, `--log`, `--mimir-url`;
- `--anthropic`, the upstream for non-Mimir models;
- `--prefix`, the model prefix routed to Mimir (repeatable, default `cubbit/`);
- `--mimir-effort model=effort`, see below;
- `--runaway-guard log|off`, `--max-reply-text BYTES`, see [Runaway replies](#runaway-replies).

## Operating

- **Health:** `curl -s http://127.0.0.1:4000/router/health`. Liveness: `GET /health/liveliness`.
- **Deploy:** `deploy/deploy.sh` builds and installs `it-router` and `run.sh`. It backs up what it replaces to `~/.config/it-gateway/backup-<timestamp>/`, `--dry-run` shows what would change, and it never restarts anything.
- **Restart:** `launchctl bootout gui/$(id -u)/io.cubbit.it-router`, then **wait until `launchctl print gui/$(id -u)/io.cubbit.it-router` fails**, then `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/io.cubbit.it-router.plist`. Bootstrapping too early fails with an I/O error. The gap is about 2 seconds, and sessions retry across it.
- **Rollback:** restore `it-router` and `run.sh` from the latest backup directory, then restart.
- **Logs:**
  - `~/Library/Logs/it-router.log` is the process output.
  - `~/Library/Logs/it-router.jsonl` has one line per `/v1/messages` request: `ts` (local time, no offset) and `ts_utc` (UTC, RFC 3339), route, model, status, `upstream_host`, ttfb and total ms, message and tool counts, `orphan_tool_results`, `first_hash`, the upstream `request_id`. On the Mimir leg it also has `folded`, `dropped_fields`, `effort` and `stream_fixes`; with `openai`, also `mimir_api`, `input_tokens`, `output_tokens` (real, from Mimir), `tool_calls`, `reasoning_chunks`, `replayed_thinking`, `reasoning_cached` and `reasoning_injected`; for streamed replies also `text_chars`, `reasoning_chars`, `text_max_repeat`, `reasoning_max_repeat` and, when the reply looks like a runaway, `runaway`.
  - `error: client went away` means the client left before the response was complete.

## Runaway replies

Sometimes Mimir produces a reply that never ends: tens of thousands of tokens of text instead of a tool call, either one line repeated over and over or long unproductive deliberation. With the `openai` API the router measures every streamed reply, for the visible text and for the reasoning (which is never forwarded, so this is the only place it shows), and writes numbers to the request log. It changes nothing in what the client receives, and it never logs text.

- `text_chars`, `reasoning_chars`: bytes of each.
- `text_max_repeat`, `reasoning_max_repeat`: the highest number of times one identical line appeared inside any 60 consecutive non-empty lines. A loop that interleaves a few filler lines still shows up.
- `runaway`: present only when the reply looks like one. A comma-separated list of `text_budget` (visible text of `--max-reply-text` bytes or more, default 24 KiB, about 6k tokens; normal replies are far smaller), `text_repeat` and `reasoning_repeat` (one line filling 30 of the last 60 lines).

```bash
jq 'select(.runaway)' ~/Library/Logs/it-router.jsonl
```

`--runaway-guard off` stops the measuring and the extra fields.

## Two ways to reach Mimir

Mimir serves the same models on two endpoints, and the gateway can use either (`MIMIR_API`):

- **`openai`**: the gateway translates each Anthropic Messages request into a Chat Completions request for `/v1`, and the reply (streamed or not) back into the Anthropic format. It writes the Anthropic stream itself, so the stream is well-formed by construction. Token usage is real, and `reasoning_effort` (`none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`) works on every model.
  - Request: the system prompt becomes a system message; mid-conversation system entries are folded as on `/anthropic`; text and images become content parts; `tool_use` blocks become `tool_calls` and `tool_result` blocks become `role: tool` messages; tools, `tool_choice`, `max_tokens`, `stop_sequences`, `temperature` and `top_p` map across.
  - Earlier reasoning: for `cubbit/mimir` and `cubbit/mimir-pro`, the `thinking` blocks of each assistant turn are joined with newlines and sent back as that message's `reasoning_content`, and the router adds `chat_template_kwargs` that make the chat template keep them: `preserve_thinking: true` for `mimir` (Qwen), `clear_thinking: false` for `mimir-pro` (GLM, which needs the reasoning exactly as generated). Keys the client already sent in `chat_template_kwargs` are kept. `cubbit/mimir-slim` (DeepSeek) gets no replay and its thinking is dropped as before, because it loops less without earlier reasoning. `redacted_thinking` and server tool blocks are always dropped. Against the same backend, replaying reasoning to Qwen with `preserve_thinking` cut run time about 2x. The log counts replayed blocks in `replayed_thinking`.
  - Reasoning cache: Claude Code never sees Mimir's reasoning (replies carry none), so its history usually has no `thinking` to replay. For `mimir` and `mimir-pro` the gateway keeps the reasoning of every complete reply that called tools, keyed by the tool call ids, which Claude Code sends back as `tool_use` ids. On a later request, an assistant turn with no `thinking` whose `tool_use` id is in the cache gets that reasoning as `reasoning_content`. The cache lives in memory only: at most 2,000 ids, 2 hours each, least recently used evicted first, and reasoning over 200 KB is not kept. A restart empties it, and the model then reasons again. Text-only final turns are not kept, since nothing comes back to key them by. The log has `reasoning_cached` (this reply's reasoning was kept) and `reasoning_injected` (turns given cached reasoning).
  - Reply: text deltas, tool calls and their argument fragments become Anthropic blocks in order; `finish_reason` maps to `stop_reason` (`stop` → `end_turn`, `length` → `max_tokens`, `tool_calls` → `tool_use`); reasoning is dropped; errors are converted, with the same rewrites as below.
  - `--mimir-effort model=effort` sets the reasoning effort per model. Claude Code's own effort is never forwarded; each model gets an explicit level it accepts: `medium` for `cubbit/mimir`, `high` for `cubbit/mimir-pro` (GLM accepts only `low` and `high` and turns `medium` or anything else into `max`) and `high` for `cubbit/mimir-slim` (DeepSeek rejects `medium`). The probe below predates that finding: its `medium` runs on `mimir-pro` were probably running at `max`. On a deliberately hard probe prompt (7 runs each): no effort sent gave 1 runaway, a median of 201 s and up to 14k reasoning chunks; `none` gave 0, 88 s and up to 5.6k; `low` gave 0, 53 s and up to 7.1k; `medium` gave 0, 158 s and up to 10k; `high` gave 1 runaway in 14 runs over two batches. Set `low` if `mimir-pro` turns are too slow.
- **`anthropic`**: the request goes to `/anthropic` nearly as is, and the gateway repairs what that endpoint gets wrong (the notes below).

Why `openai` exists: on `/anthropic`, `mimir-pro` sometimes reasons until `max_tokens` and returns an empty turn after about 3 minutes, and none of `thinking`, `output_config.effort` or `budget_tokens` stops it. On a probe prompt, 4 of 7 runs did that on `/anthropic`, and 0 of 7 on `/v1` with effort `low` or `medium`. See issue #4.

## Notes on the upstream API

These apply to the `anthropic` mode unless they say otherwise.

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
- **`count_tokens`** is not supported by Mimir (500), so the gateway answers it locally with an estimate (both modes).
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

# Design: jev (Mimir SystemOne) as a routed model

Date: 2026-11-17
Status: proposed (awaiting approval)

## Intent

Claude Code talks to one base URL: the it-router. The main conversation can
already delegate work to Mimir chat models (`cubbit/mimir`, `-pro`, `-slim`).
Mimir also serves a second kind of endpoint — SystemOne ("jev"): a typed
yes/no judge that returns calibrated probabilities instead of prose. The
marmos91/skills repo shows its value as a per-copy judge gate
(`jev ask/doubt`, fixed question + evidence text -> verdict), used to settle
split review findings and verify fixes. The skills reach SystemOne directly
from bash, so they do not need this change; this design makes the same judge
reachable as the model `cubbit/mimir-jev`, so the main Claude conversation
can consult it mid-work through a subagent, like the other Mimir models.

## Goals

- `cubbit/mimir-jev` is selectable like the other Mimir models; the router
  translates a Messages request into one `/systemone` call and the answer back
  into a Messages reply the caller reads as text.
- The reply is the same JSON the jev CLI prints, so conventions transfer:
  `{"verdict": "yes"|"no", "probabilities": {"yes", "no"}, "confidence"}`.
- Works for both skills-style uses: a question about evidence text
  (multi-message request) and a bare claim to judge (single-message request).
- Every jev request leaves the same metadata-only log line as the other
  routes, plus `jev_verdict`, `jev_p_yes`, `jev_confidence`.

## Non-goals

- No passthrough of `/v1/systemone` itself (option B, dropped: no consumer
  needs it; the jev CLI has its own creds path).
- No multi-question batches, no confidence gating in the router (the caller
  reads `confidence` from the JSON and decides), no streaming from SystemOne
  (the answer is one JSON blob; streaming is synthesized from it).
- No support in `anthropic` mode (`MIMIR_API=anthropic`): the configured
  endpoint is `/anthropic`, not `/v1`; a jev request there gets a clear 400.
- No change to effort maps, reasoning replay, or the runaway guard; none of
  those apply to SystemOne.

## Anti-goals

- jev in a Messages loop must never be used as a generator. The agent file
  and README say what it is for; the model name and one-line reply make
  loops unattractive, and no work is done to make multi-turn use convenient.

## Constraints

- Go standard library only, like the rest of it-router.
- One attempt per request, like the reverse-proxy legs: a failed SystemOne
  call is translated into an Anthropic error; retry policy stays with the
  caller. Handler-decides classification per the repo's error-handling rules.
- Metadata-only log: never prompt text, keys, or credentials.

## Approaches considered

1. **Routed model + subagent (chosen).** `cubbit/mimir-jev` rides the
   existing `cubbit/` prefix, so routing config does not change. A branch in
   `ServeHTTP` (next to the existing `count_tokens` special case) handles it
   instead of `toChatCompletions`; new file `jev.go` holds the translation.
   An `agents/mimir-jev.md` makes it delegable like `mimir-pro`.
2. **Passthrough `/v1/systemone` only.** ~20 lines, but nothing in Claude
   Code can call it and the jev CLI already reaches Mimir directly. Rejected
   as the main goal; kept as a possible later add-on.
3. **Do nothing.** True for the skills repo's own workflows, but the user's
   goal is in-conversation access; rejected.

## The how, decided

- **Model name**: `cubbit/mimir-jev`. Matches the prefix and the family
  naming; no new flags, hardcoded like the effort-map defaults.
- **Request mapping** (the one real design choice; follows jev's contract
  in the skills repo — a fixed question phrased so yes = "act", plus
  evidence text):
  - The **last user message's text** becomes the noul question
    (`criteria.true.text`).
  - All other messages (roles rendered as `role: text`) become
    `state.text` — the thing being judged. `tool_result` blocks render as
    text; `tool_use`, images and other non-text blocks are dropped;
    the system prompt is excluded (it is Claude Code boilerplate, and
    jev's benchmark shows dilution hurts calibration).
  - If **no other text exists** — the normal fresh-subagent case — the
    message itself is the claim: `state.text` = the question text, the
    `doubt`-style single-input mode.
  - Empty question text is a local 400 (never sent upstream).
- **Wire format** (`probe of {MIMIR_OPENAI_URL}/systemone`, per the skills
  repo): `POST {"state": {"text": ...}, "questions": {"q": {"type": "noul",
  "criteria": {"true": {"text": <question>}, "false": null}}}}`, answer read
  from `answers.q.{noul, probabilities, confidence}`.
- **Response mapping**: one text content block with the JSON object above;
  `verdict` = `yes` when `p_yes >= 0.5`. `stream: true` gets a synthesized
  SSE (message_start, one content_block_delta, end_turn, message_stop)
  reusing the event shapes `openai.go` already emits; `stream: false` gets
  the plain JSON body. Usage fields are zero.
- **Errors**: a non-2xx or malformed SystemOne answer goes through the
  existing `translateOpenAIError` path (sanitized Anthropic error, 502 for
  a bad body) and is logged with its status.
- **Log**: the standard fields for the request, plus the three jev fields.
- **Operator default**: 30s timeout on the SystemOne call, matching jev.py.

## Left to the builder

- Exact placement of the branch in `ServeHTTP` and the small refactor (if
  any) needed to synthesize SSE events for a locally-produced reply.
- Tests: a fake SystemOne server (httptest, like the suite's existing
  upstreams) covering ask-with-evidence, single-message doubt, both stream
  modes, upstream error, `anthropic`-mode rejection, empty question, and the
  log fields. Black-box through the gateway, per the repo's test rules.

## Success measurement

- Baseline: none — new path. After deploy, the metadata log is the evidence:
  jev requests appear with status 200, verdict/p_yes/confidence recorded,
  and no runtime-config changes needed for existing models (one request via
  a live router before merge is the check).
- Guardrail: the other routes' behavior and their existing tests unchanged.

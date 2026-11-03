#!/usr/bin/env python3
"""Measure how much Mimir Pro reasons under different reasoning controls, straight against Mimir's /anthropic endpoint.

Usage: MIMIR_API_KEY=... MIMIR_ANTHROPIC_URL=... effort_probe.py OUT.jsonl RUNS VARIANT...   (variants: none, disabled, low, medium, high, budget2k, budget8k)
Writes one JSON line per run: variant, seconds, thinking events/chars (any block), text chars, tool calls, stop reason, status.
"""
import concurrent.futures as cf
import json
import os
import sys
import time
import urllib.request

# Where to send the requests: the gateway (http://127.0.0.1:4000/v1/messages) or Mimir directly
# ($MIMIR_ANTHROPIC_URL/v1/messages, which is the default).
URL = os.environ.get("PROBE_URL") or os.environ["MIMIR_ANTHROPIC_URL"].rstrip("/") + "/v1/messages"
PROMPT = ("A warehouse has 7 robots on a 6x6 grid. Each robot moves one cell per tick, never onto a cell another robot "
          "occupies at that tick, and robots may not swap places. Robots start at (0,0),(0,5),(5,0),(5,5),(2,2),(3,3),(2,3) "
          "and must reach (5,5),(5,0),(0,5),(0,0),(3,3),(2,2),(3,2) respectively. Find the minimum number of ticks and give "
          "a valid schedule. Then call the tool `submit` with the tick count.")
TOOLS = [{"name": "submit", "description": "Submit the answer", "input_schema": {"type": "object", "properties": {"ticks": {"type": "integer"}}, "required": ["ticks"]}}]

VARIANTS = {
    "none": {},
    "disabled": {"thinking": {"type": "disabled"}},
    "low": {"thinking": {"type": "adaptive"}, "output_config": {"effort": "low"}},
    "medium": {"thinking": {"type": "adaptive"}, "output_config": {"effort": "medium"}},
    "high": {"thinking": {"type": "adaptive"}, "output_config": {"effort": "high"}},
    "budget2k": {"thinking": {"type": "enabled", "budget_tokens": 2000}},
    "budget8k": {"thinking": {"type": "enabled", "budget_tokens": 8000}},
}


def run(variant: str) -> dict:
    body = {"model": "cubbit/mimir-pro", "max_tokens": 32000, "stream": True, "tools": TOOLS,
            "messages": [{"role": "user", "content": PROMPT}], **VARIANTS[variant]}
    req = urllib.request.Request(URL, data=json.dumps(body).encode(), method="POST", headers={
        "x-api-key": os.environ["MIMIR_API_KEY"], "content-type": "application/json", "anthropic-version": "2023-06-01"})
    r = {"variant": variant, "think_events": 0, "think_chars": 0, "text_chars": 0, "tools": 0, "stop": None, "status": None}
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=600) as resp:
            r["status"] = resp.status
            for raw in resp:
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data:"):
                    continue
                try:
                    d = json.loads(line[5:])
                except ValueError:
                    continue
                t = d.get("type")
                if t == "content_block_delta":
                    delta = d.get("delta", {})
                    if delta.get("type") == "thinking_delta":
                        r["think_events"] += 1
                        r["think_chars"] += len(delta.get("thinking", ""))
                    elif delta.get("type") == "text_delta":
                        r["text_chars"] += len(delta.get("text", ""))
                elif t == "content_block_start" and d.get("content_block", {}).get("type") == "tool_use":
                    r["tools"] += 1
                elif t == "message_delta":
                    r["stop"] = d.get("delta", {}).get("stop_reason")
    except urllib.error.HTTPError as e:
        r["status"] = e.code
        r["error"] = e.read()[:300].decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001 - a probe records every failure
        r["error"] = f"{e.__class__.__name__}: {e}"
    r["seconds"] = round(time.monotonic() - t0, 1)
    return r


def main() -> None:
    out, runs, variants = sys.argv[1], int(sys.argv[2]), sys.argv[3:]
    jobs = [v for v in variants for _ in range(runs)]
    with cf.ThreadPoolExecutor(max_workers=6) as ex, open(out, "a") as f:
        for r in ex.map(run, jobs):
            f.write(json.dumps(r) + "\n")
            f.flush()
            print(json.dumps(r), flush=True)


if __name__ == "__main__":
    main()

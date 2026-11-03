#!/usr/bin/env python3
"""Strict structural validator for an Anthropic-format SSE stream (what Claude Code's stream parser expects)."""
import json
OKPAIR = {"text": {"text_delta"}, "tool_use": {"input_json_delta"}, "thinking": {"thinking_delta", "signature_delta"}}
def validate(raw: bytes):
    v, info = [], []
    txt = raw.decode("utf-8", "replace")
    if "\r\n" in txt: info.append("crlf")
    events = [e for e in txt.split("\n\n")]
    if events and events[-1].strip() == "": events.pop()
    elif events: v.append("trailing_partial_event")
    state = {"started": False, "stopped": False, "blocks": {}, "next": 0, "open": set(), "tool": False, "stop_reason": None, "partial": {}}
    for n, ev in enumerate(events):
        name = data = None; extra = []
        for l in ev.split("\n"):
            if l.startswith("event:"): name = l[6:].strip()
            elif l.startswith("data:"): data = l[5:].strip()
            elif l.startswith(":") or l == "": pass
            else: extra.append(l[:30])
        if extra: v.append("unknown_line:" + extra[0])
        if state["stopped"]: v.append("events_after_message_stop"); break
        if data is None:
            v.append("event_without_data:" + str(name)); continue
        try: d = json.loads(data)
        except Exception: v.append("bad_json:" + str(name)); continue
        t = d.get("type")
        if name != t: v.append(f"event_name_mismatch:{name}/{t}")
        if n == 0 and t != "message_start": v.append("first_event_not_message_start")
        if t == "message_start":
            state["started"] = True
            m = d.get("message") or {}
            if not m.get("id") or m.get("role") != "assistant": v.append("message_start_incomplete")
        elif not state["started"]: v.append("event_before_message_start:" + str(t))
        if t == "content_block_start":
            i = d.get("index"); cb = d.get("content_block") or {}
            if state["open"]: info.append("interleaved_blocks")
            if i != state["next"]: v.append(f"block_index_not_sequential:{i}!={state['next']}")
            state["blocks"][i] = cb.get("type"); state["open"].add(i); state["next"] = (i if isinstance(i, int) else state["next"]) + 1
            state["partial"][i] = ""
            if cb.get("type") == "tool_use":
                state["tool"] = True
                if not cb.get("id") or not cb.get("name"): v.append("tool_use_missing_id_or_name")
        elif t == "content_block_delta":
            i = d.get("index"); dt = (d.get("delta") or {}).get("type")
            if i not in state["blocks"]: v.append("delta_for_unknown_block")
            elif i not in state["open"]: v.append("delta_for_closed_block")
            else:
                bt = state["blocks"][i]
                if dt not in OKPAIR.get(bt, set()): v.append(f"delta_type_mismatch:{bt}/{dt}")
                if dt == "input_json_delta": state["partial"][i] += d["delta"].get("partial_json", "")
        elif t == "content_block_stop":
            i = d.get("index")
            if i not in state["open"]: v.append("block_stop_without_open_block")
            else:
                if state["blocks"][i] == "tool_use" and state["partial"][i].strip():
                    try: json.loads(state["partial"][i])
                    except Exception: v.append("tool_input_invalid_json")
                state["open"].discard(i)
        elif t == "message_delta":
            if state["open"]: v.append("message_delta_while_block_open")
            sr = (d.get("delta") or {}).get("stop_reason"); state["stop_reason"] = sr
            if sr is None: v.append("message_delta_without_stop_reason")
            if "usage" not in d: info.append("no_usage_in_message_delta")
        elif t == "message_stop":
            state["stopped"] = True
        elif t == "ping": pass
        elif t == "error": v.append("error_event")
        elif t not in ("message_start", "content_block_start", "content_block_delta", "content_block_stop"): v.append("unknown_event:" + str(t))
    if not state["stopped"]: v.append("no_message_stop")
    if state["tool"] and state["stop_reason"] not in ("tool_use", None): v.append("stop_reason_mismatch:" + str(state["stop_reason"]))
    return sorted(set(v)), sorted(set(info))
if __name__ == "__main__":
    import sys
    for f in sys.argv[1:]: print(f, *validate(open(f, "rb").read()))

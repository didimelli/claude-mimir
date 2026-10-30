package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// A message is kept as raw JSON fields so everything the fold does not touch passes through exactly.
type msg = map[string]json.RawMessage

type foldStats struct {
	Folded        int // system entries turned into user messages
	DroppedBlocks int // non-text blocks removed from system entries (Claude Code's tool_addition)
	Skipped       int // system entries without any text
	Hoisted       int // leading system entries moved into the top-level system field
}

const reminderOpen = "<system-reminder>"

// marshal encodes without HTML escaping, so "<system-reminder>" stays readable and sizes stay small.
func marshal(v any) json.RawMessage {
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

type textBlockJSON struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textBlock(s string) json.RawMessage { return marshal(textBlockJSON{"text", s}) }

func roleOf(m msg) string {
	var s string
	_ = json.Unmarshal(m["role"], &s)
	return s
}

// contentBlocks returns the blocks of a content value; a plain string is one text block.
func contentBlocks(c json.RawMessage) []json.RawMessage {
	c = bytes.TrimSpace(c)
	if len(c) == 0 {
		return nil
	}
	switch c[0] {
	case '"':
		var s string
		_ = json.Unmarshal(c, &s)
		return []json.RawMessage{textBlock(s)}
	case '[':
		var bl []json.RawMessage
		_ = json.Unmarshal(c, &bl)
		return bl
	}
	return nil
}

func hasToolUse(m msg) bool {
	for _, b := range contentBlocks(m["content"]) {
		var t struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(b, &t) == nil && t.Type == "tool_use" {
			return true
		}
	}
	return false
}

// systemTexts keeps the text of a system entry. Other block types are dropped: they are not valid in a user turn.
func systemTexts(content json.RawMessage) (texts []string, dropped, skipped int) {
	for _, b := range contentBlocks(content) {
		var t struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(b, &t)
		switch {
		case t.Type != "text":
			dropped++
		case strings.TrimSpace(t.Text) == "":
			skipped++
		default:
			texts = append(texts, t.Text)
		}
	}
	return
}

func wrapReminder(t string) string {
	if strings.HasPrefix(strings.TrimSpace(t), reminderOpen) {
		return t
	}
	return reminderOpen + "\n" + t + "\n</system-reminder>"
}

func userMsg(blocks []json.RawMessage) msg {
	return msg{"role": marshal("user"), "content": marshal(blocks)}
}

// withBlocks returns a copy of a user message with extra blocks appended after its own.
func withBlocks(m msg, extra []json.RawMessage) msg {
	out := make(msg, len(m))
	for k, v := range m {
		out[k] = v
	}
	blocks := append(append([]json.RawMessage{}, contentBlocks(m["content"])...), extra...)
	out["content"] = marshal(blocks)
	return out
}

// foldMessages removes every role:"system" entry from a message list, for upstreams that only accept a system
// prompt at the start. Rules:
//   - an entry becomes a user message in place (text blocks only, wrapped in <system-reminder>), so its position
//     in the conversation is kept and no earlier message is ever changed, which keeps the prompt prefix stable;
//   - an entry right after an assistant message that has a tool_use is appended, after its blocks, to the user
//     turn that follows (the one holding the tool_result), because the result must directly follow the tool_use;
//   - entries before the first non-system message go into the top-level system field.
//
// The input is never modified.
func foldMessages(in []msg, system json.RawMessage) ([]msg, json.RawMessage, foldStats) {
	var st foldStats
	out := make([]msg, 0, len(in))
	var pending, hoist []json.RawMessage
	seenNonSystem := false
	flush := func() {
		if len(pending) > 0 {
			out = append(out, userMsg(pending))
			pending = nil
		}
	}
	for _, m := range in {
		if roleOf(m) != "system" {
			seenNonSystem = true
			if len(pending) > 0 {
				if roleOf(m) == "user" {
					m = withBlocks(m, pending)
					pending = nil
				} else {
					flush()
				}
			}
			out = append(out, m)
			continue
		}
		texts, dropped, _ := systemTexts(m["content"])
		st.DroppedBlocks += dropped
		if len(texts) == 0 {
			st.Skipped++
			continue
		}
		if !seenNonSystem {
			for _, t := range texts {
				hoist = append(hoist, textBlock(t))
			}
			st.Hoisted++
			continue
		}
		blocks := make([]json.RawMessage, len(texts))
		for i, t := range texts {
			blocks[i] = textBlock(wrapReminder(t))
		}
		st.Folded++
		if n := len(out); n > 0 && roleOf(out[n-1]) == "assistant" && hasToolUse(out[n-1]) {
			pending = append(pending, blocks...)
			continue
		}
		flush()
		out = append(out, userMsg(blocks))
	}
	flush()
	if len(hoist) > 0 {
		system = marshal(append(append([]json.RawMessage{}, contentBlocks(system)...), hoist...))
	}
	return out, system, st
}

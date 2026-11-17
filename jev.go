package main

// The jev leg: cubbit/mimir-jev answers typed yes/no questions on Mimir's /systemone endpoint, not chat
// completions. The last user message becomes the noul question; everything said before it becomes the
// text being judged. The answer comes back as one JSON line in the shape the jev CLI prints.

import (
	"encoding/json"
	"errors"
	"strings"
)

const jevModel = "cubbit/mimir-jev"

// toSystemOne pulls the (question, state) pair out of a Messages request. The system prompt is never
// judged on: it is Claude Code boilerplate, and jev's calibration dilutes fast.
func toSystemOne(body []byte) (question, state string, err error) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", err
	}
	msgs := make([]msg, 0, len(req.Messages))
	for _, raw := range req.Messages {
		var m msg
		if json.Unmarshal(raw, &m) == nil {
			msgs = append(msgs, m)
		}
	}
	last := -1
	for i, m := range msgs {
		if roleOf(m) == "user" {
			last = i
		}
	}
	if last < 0 {
		return "", "", errors.New("no user message to ask a question of")
	}
	var qs []string
	for _, b := range blocksOfContent(msgs[last]["content"]) {
		if b.Type == "text" && b.Text != "" {
			qs = append(qs, b.Text)
		}
	}
	question = strings.Join(qs, "\n\n")
	var ev []string
	for _, m := range msgs[:last] {
		var ts []string
		for _, b := range blocksOfContent(m["content"]) {
			switch b.Type {
			case "text":
				if b.Text != "" {
					ts = append(ts, b.Text)
				}
			case "tool_result":
				if s, _ := toolResultText(b); s != "" {
					ts = append(ts, s)
				}
			}
		}
		if len(ts) > 0 {
			ev = append(ev, roleOf(m)+": "+strings.Join(ts, "\n\n"))
		}
	}
	state = strings.Join(ev, "\n\n")
	if question == "" {
		return "", "", errors.New("the last user message has no text to ask")
	}
	if state == "" { // a fresh single-message conversation: the claim is judged on its own
		state = question
	}
	return question, state, nil
}

package main

// The jev leg: cubbit/mimir-jev answers typed yes/no questions on Mimir's /systemone endpoint, not chat
// completions. The last user message becomes the noul question; everything said before it becomes the
// text being judged. The answer comes back as one JSON line in the shape the jev CLI prints.

import (
	"bytes"
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

// ---- replies -------------------------------------------------------------------------------------------------

// parseSystemOne reads a SystemOne answer: {"answers":{"q":{"probabilities":{"yes","no"},"confidence"}}}. The
// same [0,1] bounds jev.py checks keep fabricated numbers out of the log.
func parseSystemOne(body []byte) (pYes, pNo, confidence float64, err error) {
	var r struct {
		Answers struct {
			Q *struct {
				Probabilities struct {
					Yes float64 `json:"yes"`
					No  float64 `json:"no"`
				} `json:"probabilities"`
				Confidence float64 `json:"confidence"`
			} `json:"q"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, 0, 0, err
	}
	if r.Answers.Q == nil {
		return 0, 0, 0, errors.New("no answers.q in the SystemOne reply")
	}
	a := r.Answers.Q
	if pYes, pNo, confidence = a.Probabilities.Yes, a.Probabilities.No, a.Confidence; pYes < 0 || pYes > 1 || pNo < 0 || pNo > 1 || confidence < 0 || confidence > 1 {
		return 0, 0, 0, errors.New("SystemOne probabilities or confidence outside [0, 1]")
	}
	return pYes, pNo, confidence, nil
}

func jevEvent(b *bytes.Buffer, name string, data any) {
	b.WriteString("event: " + name + "\ndata: ")
	b.Write(marshal(data))
	b.WriteString("\n\n")
}

// jevReply builds the whole response body: the jev JSON line as one text block, as a Messages object or as a
// synthesized SSE stream with the house event shapes. Usage is zero: SystemOne reports no tokens.
func jevReply(model string, pYes, pNo, confidence float64, stream bool) []byte {
	verdict := "no"
	if pYes >= 0.5 {
		verdict = "yes"
	}
	line := marshal(map[string]any{
		"verdict": verdict, "probabilities": map[string]float64{"yes": pYes, "no": pNo}, "confidence": confidence,
	})
	usage := map[string]int{"input_tokens": 0, "output_tokens": 0}
	if stream {
		var b bytes.Buffer
		jevEvent(&b, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": "msg_jev", "type": "message", "role": "assistant", "model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil, "usage": usage,
		}})
		jevEvent(&b, "content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		jevEvent(&b, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": string(line)}})
		jevEvent(&b, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		jevEvent(&b, "message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": usage})
		jevEvent(&b, "message_stop", map[string]string{"type": "message_stop"})
		return b.Bytes()
	}
	return marshal(map[string]any{
		"id": "msg_jev", "type": "message", "role": "assistant", "model": model,
		"content":          []any{map[string]any{"type": "text", "text": string(line)}},
		"stop_reason":      "end_turn", "stop_sequence": nil, "usage": usage,
	})
}

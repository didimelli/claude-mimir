package main

// The jev leg: cubbit/mimir-jev answers typed yes/no questions on Mimir's /systemone endpoint, not chat
// completions. The last user message becomes the noul question; everything said before it becomes the
// text being judged. The answer comes back as one JSON line in the shape the jev CLI prints.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const jevModel = "cubbit/mimir-jev"

var jevTimeout = 30 * time.Second

// jevVerdict is the one place the decision rule lives: P(yes) >= 0.5 -> "yes"; the reply and the log share it.
func jevVerdict(pYes float64) string {
	if pYes >= 0.5 {
		return "yes"
	}
	return "no"
}

// isReminder reports a text that is entirely a Claude Code system-reminder wrapper. The harness injects them
// into user turns (CLAUDE.md, hooks output, attached context); judging them as the question or as evidence would
// dilute the calibration the way the system prompt does, which toSystemOne already excludes.
func isReminder(s string) bool {
	t := strings.TrimSpace(s)
	return strings.HasPrefix(t, reminderOpen) && strings.HasSuffix(t, "</system-reminder>")
}

// toSystemOne pulls the (question, state, trailing) triple out of a Messages request. The system prompt is
// never judged on: it is Claude Code boilerplate, and jev's calibration dilutes fast.
func toSystemOne(body []byte) (question, state string, trailing int, err error) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "", "", 0, err
	}
	msgs := make([]msg, 0, len(req.Messages))
	for _, raw := range req.Messages {
		var m msg
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", "", 0, fmt.Errorf("a message could not be read: %w", err)
		}
		msgs = append(msgs, m)
	}
	last := -1
	for i, m := range msgs {
		if roleOf(m) == "user" {
			last = i
		}
	}
	if last < 0 {
		return "", "", 0, errors.New("no user message to ask a question of")
	}
	trailing = len(msgs) - 1 - last // prefill turn and anything else after the question
	var qs []string
	for _, b := range blocksOfContent(msgs[last]["content"]) {
		if b.Type == "text" && b.Text != "" && !isReminder(b.Text) {
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
				if b.Text != "" && !isReminder(b.Text) {
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
		return "", "", 0, errors.New("the last user message has no text to ask")
	}
	if state == "" { // a fresh single-message conversation: the claim is judged on its own
		state = question
	}
	return question, state, trailing, nil
}

// ---- replies -------------------------------------------------------------------------------------------------

// parseSystemOne reads a SystemOne answer: {"answers":{"q":{"probabilities":{"yes","no"},"confidence"}}}. The
// same [0,1] bounds jev.py checks keep fabricated numbers out of the log.
func parseSystemOne(body []byte) (pYes, pNo, confidence float64, err error) {
	var r struct {
		Answers struct {
			Q *struct {
				Probabilities struct {
					Yes *float64 `json:"yes"`
					No  *float64 `json:"no"`
				} `json:"probabilities"`
				Confidence *float64 `json:"confidence"`
			} `json:"q"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, 0, 0, err
	}
	a := r.Answers.Q
	if a == nil || a.Probabilities.Yes == nil || a.Probabilities.No == nil || a.Confidence == nil {
		return 0, 0, 0, errors.New("no answers.q in the SystemOne reply")
	}
	if pYes, pNo, confidence = *a.Probabilities.Yes, *a.Probabilities.No, *a.Confidence; pYes < 0 || pYes > 1 || pNo < 0 || pNo > 1 || confidence < 0 || confidence > 1 {
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
	line := marshal(map[string]any{
		"verdict": jevVerdict(pYes), "probabilities": map[string]float64{"yes": pYes, "no": pNo}, "confidence": confidence,
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

// ---- the gateway branch --------------------------------------------------------------------------------------

// serveJev answers one cubbit/mimir-jev request itself: one SystemOne call, one reply, one log line. It never
// retries; a failed call is classified here, at the handler, per the repo's error-handling rules.
func (g *gateway) serveJev(w http.ResponseWriter, r *http.Request, ri *reqInfo, src []byte) {
	ri.ctx = r.Context() // everything below checks client cancellation through reqInfo
	if g.cfg.mimirAPI != "openai" {
		ri.status, ri.errMsg = http.StatusBadRequest, "jev needs the openai Mimir API"
		writeError(w, http.StatusBadRequest, "router: cubbit/mimir-jev needs the openai Mimir API (MIMIR_API=openai)")
		ri.finish()
		return
	}
	question, state, trailing, err := toSystemOne(src)
	if err != nil {
		ri.status, ri.errMsg = http.StatusBadRequest, err.Error()
		writeError(w, http.StatusBadRequest, "router: cubbit/mimir-jev needs a text question in the last user message: "+err.Error())
		ri.finish()
		return
	}
	if trailing > 0 {
		ri.rec["jev_trailing_turns"] = trailing
	}
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(src, &req)
	call := marshal(map[string]any{
		"state": map[string]string{"text": state},
		"questions": map[string]any{"q": map[string]any{
			"type": "noul",
			"criteria": map[string]any{
				"true":  map[string]string{"text": question},
				"false": nil,
			},
		}},
	})
	hreq, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		strings.TrimRight(g.mimirU.String(), "/")+"/systemone", bytes.NewReader(call))
	if err != nil { // unreachable: the endpoint passed parseConfig
		ri.status, ri.errMsg = http.StatusInternalServerError, err.Error()
		writeError(w, http.StatusInternalServerError, "router: bad Mimir endpoint: "+err.Error())
		ri.finish()
		return
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+g.cfg.mimirKey)
	resp, err := (&http.Client{Transport: g.transport, Timeout: jevTimeout}).Do(hreq)
	if err != nil {
		if errors.Is(err, context.Canceled) { // the client is gone; writing to the ResponseWriter is wasted
			ri.errMsg = "client went away"
			ri.finish()
			return
		}
		var ne net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
			ri.errMsg = err.Error() // the full cause, endpoint included, stays in the log
			st, eb := translateOpenAIError(http.StatusGatewayTimeout, marshal(map[string]any{
				"error": map[string]string{"message": "Mimir's SystemOne did not answer in " + jevTimeout.String()},
			}))
			g.jevRespond(w, ri, st, eb, ri.errMsg)
			return
		}
		g.jevFail(w, ri, http.StatusBadGateway, "SystemOne is unreachable: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		g.jevFailStatus(w, ri, resp.StatusCode, payload)
		return
	}
	pYes, pNo, conf, err := parseSystemOne(payload)
	if err != nil {
		g.jevFail(w, ri, http.StatusBadGateway, "could not read the SystemOne reply: "+err.Error())
		return
	}
	ri.rec["jev_verdict"], ri.rec["jev_p_yes"], ri.rec["jev_confidence"] = jevVerdict(pYes), pYes, conf
	ri.status = http.StatusOK
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	body := &countingBody{rc: io.NopCloser(bytes.NewReader(jevReply(jevModel, pYes, pNo, conf, req.Stream))), ri: ri}
	_, _ = io.Copy(w, body)
	_ = body.Close() // closes the log line, as on the proxied legs
}

// jevRespond writes an error body we translated ourselves and closes the log line with logMsg.
func (g *gateway) jevRespond(w http.ResponseWriter, ri *reqInfo, st int, eb []byte, logMsg string) {
	ri.status, ri.errMsg = st, logMsg
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	_, _ = w.Write(eb)
	ri.finish()
}

// jevFailStatus translates an upstream error into a sanitized Anthropic error; the raw payload never reaches
// the client, and only the translated status plus the upstream's message are logged.
func (g *gateway) jevFailStatus(w http.ResponseWriter, ri *reqInfo, status int, payload []byte) {
	st, eb := translateOpenAIError(status, payload)
	g.jevRespond(w, ri, st, eb, errorMessage(eb, "")) // structured and bounded, exactly like the proxied legs
}

func (g *gateway) jevFail(w http.ResponseWriter, ri *reqInfo, status int, msg string) {
	st, eb := translateOpenAIError(status, marshal(map[string]any{"error": map[string]string{"message": msg}}))
	g.jevRespond(w, ri, st, eb, msg)
}

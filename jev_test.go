package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- toSystemOne: question and state extracted from a Messages request ---------------------------------------

func TestToSystemOneQuestionFromLastUserMessage(t *testing.T) {
	body := []byte(`{
		"model": "cubbit/mimir-jev",
		"system": [{"type": "text", "text": "system prompt text"}],
		"messages": [
			{"role": "assistant", "content": "some answer"},
			{"role": "user", "content": "u1"},
			{"role": "user", "content": [
				{"type": "tool_use", "id": "tu1", "name": "query_retro", "input": {"q": "x"}},
				{"type": "text", "text": "is u1 true of the earlier turns?"}
			]}
		]
	}`)
	question, state, _, err := toSystemOne(body)
	if err != nil {
		t.Fatal(err)
	}
	if question != "is u1 true of the earlier turns?" {
		t.Errorf("question %q", question)
	}
	if state != "assistant: some answer\n\nuser: u1" {
		t.Errorf("state %q", state)
	}
	if strings.Contains(state, "system prompt text") {
		t.Error("the system prompt leaked into the state")
	}
	if strings.Contains(state, "query_retro") {
		t.Error("a tool_use name leaked into the state")
	}
}

func TestToSystemOneDoubtFallback(t *testing.T) {
	question, state, _, err := toSystemOne([]byte(`{"messages": [{"role": "user", "content": "The Earth is flat."}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if question != "The Earth is flat." || state != "The Earth is flat." {
		t.Errorf("question %q state %q", question, state)
	}
}

func TestToSystemOneToolResultIsEvidence(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu1", "is_error": true,
				 "content": [{"type": "text", "text": "output text"}]}
			]},
			{"role": "user", "content": "did the job fail?"}
		]
	}`)
	question, state, _, err := toSystemOne(body)
	if err != nil {
		t.Fatal(err)
	}
	if question != "did the job fail?" {
		t.Errorf("question %q", question)
	}
	if !strings.Contains(state, "user: Error: output text") {
		t.Errorf("state %q", state)
	}
}

func TestToSystemOneRejectsEmptyQuestion(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "earlier text"},
			{"role": "user", "content": [
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "xx"}}
			]}
		]
	}`)
	question, _, _, err := toSystemOne(body)
	if err == nil {
		t.Fatal("an image-only question turn must be rejected")
	}
	if question != "" {
		t.Errorf("question %q", question)
	}
}

// ---- parseSystemOne and jevReply: the jev leg's reply side ---------------------------------------------------

func TestParseSystemOne(t *testing.T) {
	pYes, pNo, confidence, err := parseSystemOne([]byte(`{"answers":{"q":{"noul":0.9,"probabilities":{"yes":0.9,"no":0.1},"confidence":0.83}}}`))
	if err != nil || pYes != 0.9 || pNo != 0.1 || confidence != 0.83 {
		t.Fatalf("%v %v %v %v", pYes, pNo, confidence, err)
	}
	for name, body := range map[string]string{
		"missing answers.q": `{}`,
		"confidence out of range": `{"answers":{"q":{"probabilities":{"yes":0.9,"no":0.1},"confidence":1.5}}}`,
		"empty q object":        `{"answers":{"q":{}}}`,
		"noul only":              `{"answers":{"q":{"noul":0.9}}}`,
	} {
		if _, _, _, err := parseSystemOne([]byte(body)); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
}

// jevText unmarshals the JSON line a reply's single text block carries.
func jevText(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return m
}

func TestJevReplyNotStreamed(t *testing.T) {
	out := jevReply(jevModel, 0.3, 0.7, 0.9, false)
	m := jevText(t, out)
	if m["type"] != "message" || m["role"] != "assistant" || m["stop_reason"] != "end_turn" {
		t.Fatalf("%s", out)
	}
	if fmt.Sprint(m["usage"]) != "map[input_tokens:0 output_tokens:0]" {
		t.Errorf("usage %v", m["usage"])
	}
	content, _ := m["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content %v", content)
	}
	line := jevText(t, []byte(content[0].(map[string]any)["text"].(string)))
	if line["verdict"] != "no" {
		t.Errorf("verdict %v", line["verdict"])
	}
	probs, _ := line["probabilities"].(map[string]any)
	if probs["yes"] != 0.3 || probs["no"] != 0.7 || line["confidence"] != 0.9 {
		t.Errorf("line %v", line)
	}
}

func TestJevReplyVerdictBoundary(t *testing.T) {
	out := jevReply(jevModel, 0.5, 0.5, 1, false)
	m := jevText(t, out)
	content := m["content"].([]any)
	if jevText(t, []byte(content[0].(map[string]any)["text"].(string)))["verdict"] != "yes" {
		t.Errorf("p_yes 0.5 must be yes: %s", out)
	}
}

func TestJevReplyStreamed(t *testing.T) {
	out := jevReply(jevModel, 0.3, 0.7, 0.9, true)
	res := checkAnthStream(t, out)
	if len(res.blocks) != 1 || res.stopReason != "end_turn" || fmt.Sprint(res.usage) != "map[input_tokens:0 output_tokens:0]" {
		t.Fatalf("blocks %v stop %q usage %v", res.blocks, res.stopReason, res.usage)
	}
	line := jevText(t, []byte(res.blocks[0]["text"].(string)))
	if line["verdict"] != "no" {
		t.Errorf("verdict %v", line)
	}
	probs, _ := line["probabilities"].(map[string]any)
	if probs["yes"] != 0.3 || probs["no"] != 0.7 || line["confidence"] != 0.9 {
		t.Errorf("line %v", line)
	}
}

// ---- through the gateway -------------------------------------------------------------------------------------

const systemOneAnswer = `{"answers":{"q":{"noul":0.1,"probabilities":{"yes":0.1,"no":0.9},"confidence":0.7}}}`

func jevConvo(stream bool) []byte {
	s := "false"
	if stream {
		s = "true"
	}
	return []byte(`{"model":"cubbit/mimir-jev","max_tokens":10,"stream":` + s + `,
		"system":[{"type":"text","text":"system prompt text"}],
		"messages":[
			{"role":"assistant","content":"` + secretText + ` evidence"},
			{"role":"user","content":"` + secretText + ` evidence too"},
			{"role":"user","content":"does the earlier turn hold?"}
		]}`)
}

func TestGatewayJevStreamRequest(t *testing.T) {
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(systemOneAnswer))
	})
	resp, b := r.post(t, "/v1/messages", jevConvo(true))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	res := checkAnthStream(t, b)
	if len(res.blocks) != 1 {
		t.Fatalf("blocks %v", res.blocks)
	}
	line := jevText(t, []byte(res.blocks[0]["text"].(string)))
	if line["verdict"] != "no" {
		t.Errorf("verdict %v", line)
	}
	got := r.mimir.last(t)
	if got.path != "/v1/systemone" {
		t.Errorf("upstream request %s", got.path)
	}
	if got.header.Get("Authorization") != "Bearer mimir-test-key" {
		t.Errorf("authorization %v", got.header)
	}
	var sent map[string]any
	_ = json.Unmarshal(got.body, &sent)
	q := sent["questions"].(map[string]any)["q"].(map[string]any)
	if q["type"] != "noul" {
		t.Errorf("question %v", q)
	}
	cr := q["criteria"].(map[string]any)
	if cr["true"].(map[string]any)["text"] != "does the earlier turn hold?" || cr["false"] != nil {
		t.Errorf("criteria %v", cr)
	}
	wantState := "assistant: " + secretText + " evidence\n\nuser: " + secretText + " evidence too"
	if sent["state"].(map[string]any)["text"] != wantState {
		t.Errorf("state %v", sent["state"])
	}
	lg := r.logs(t)
	if len(lg) != 1 || lg[0]["model"] != "cubbit/mimir-jev" || lg[0]["status"] != 200.0 ||
		lg[0]["jev_verdict"] != "no" || lg[0]["jev_p_yes"] != 0.1 || lg[0]["jev_confidence"] != 0.7 {
		t.Fatalf("log %v", lg)
	}
	if lg[0]["n_messages"] != 3.0 {
		t.Errorf("the jev log line carries no standard request facts: %v", lg[0])
	}
	if strings.Contains(string(mustRead(t, r.logPath)), secretText) {
		t.Fatal("prompt text reached the log")
	}
}

func TestGatewayJevNonStreamRequest(t *testing.T) {
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(systemOneAnswer))
	})
	resp, b := r.post(t, "/v1/messages", jevConvo(false))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	m := jevText(t, b)
	if m["type"] != "message" || m["stop_reason"] != "end_turn" {
		t.Fatalf("%s", b)
	}
	line := jevText(t, []byte(m["content"].([]any)[0].(map[string]any)["text"].(string)))
	if line["verdict"] != "no" || line["probabilities"].(map[string]any)["yes"] != 0.1 || line["confidence"] != 0.7 {
		t.Errorf("line %v", line)
	}
}

func TestGatewayJevAnthropicModeRefused(t *testing.T) {
	var called atomic.Bool
	r := newOpenAIRigWith(t, func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
	}, func(c *config) { c.mimirAPI = "anthropic" })
	resp, b := r.post(t, "/v1/messages", jevConvo(false))
	if resp.StatusCode != 400 || !strings.Contains(string(b), "mimir-jev") || !strings.Contains(string(b), "openai") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if called.Load() {
		t.Error("the upstream was reached in anthropic mode")
	}
}

func TestGatewayJevUpstreamErrorIsTranslated(t *testing.T) {
	longError := "<html>502 bad gateway " + strings.Repeat("x", 400) + " " + secretText + "</html>"
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(longError))
	})
	resp, b := r.post(t, "/v1/messages", jevConvo(false))
	if resp.StatusCode != 503 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	e := jevText(t, b)
	if e["type"] != "error" || e["error"].(map[string]any)["type"] != "api_error" {
		t.Fatalf("%s", b)
	}
	lg := r.logs(t)
	if len(lg) != 1 || lg[0]["status"] != 503.0 || lg[0]["error"] == nil {
		t.Fatalf("log %v", lg)
	}
	if strings.Contains(string(mustRead(t, r.logPath)), secretText) {
		t.Fatal("prompt text reached the log from the error path")
	}
}

func TestGatewayJevTimeoutIs504AndSanitized(t *testing.T) {
	old := jevTimeout
	jevTimeout = 150 * time.Millisecond
	t.Cleanup(func() { jevTimeout = old })
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) { time.Sleep(time.Second) })
	resp, b := r.post(t, "/v1/messages", jevConvo(false))
	if resp.StatusCode != 504 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e := jevText(t, b); e["error"].(map[string]any)["type"] != "api_error" {
		t.Fatalf("%s", b)
	}
	if strings.Contains(string(b), r.mimir.URL) || strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("the client saw the endpoint: %s", b)
	}
	lg := r.logs(t)
	if len(lg) != 1 || lg[0]["status"] != 504.0 || lg[0]["error"] == nil {
		t.Fatalf("log %v", lg)
	}
}

func TestGatewayJevLogsTrailingTurns(t *testing.T) {
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(systemOneAnswer))
	})
	body := []byte(`{"model":"cubbit/mimir-jev","max_tokens":10,"stream":false,"messages":[
		{"role":"assistant","content":"evidence"},
		{"role":"user","content":"is it green?"},
		{"role":"assistant","content":"prefill"}
	]}`)
	resp, b := r.post(t, "/v1/messages", body)
	if resp.StatusCode != 200 || strings.Contains(string(b), "prefill") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	lg := r.logs(t)
	if len(lg) != 1 || lg[0]["jev_trailing_turns"] != 1.0 {
		t.Fatalf("log %v", lg)
	}
}

func TestGatewayJevBadAnswerIs502(t *testing.T) {
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	resp, b := r.post(t, "/v1/messages", jevConvo(false))
	if got := r.mimir.last(t).path; got != "/v1/systemone" {
		t.Fatalf("upstream request %s", got)
	}
	if resp.StatusCode != 502 || strings.Contains(string(b), "{}") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if e := jevText(t, b); e["error"].(map[string]any)["type"] != "api_error" {
		t.Fatalf("%s", b)
	}
	lg := r.logs(t)
	if len(lg) != 1 || lg[0]["error"] == nil {
		t.Fatalf("log %v", lg)
	}
}

func TestGatewayJevEmptyQuestionIs400(t *testing.T) {
	var called atomic.Bool
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
	})
	body := []byte(`{"model":"cubbit/mimir-jev","max_tokens":10,"messages":[
		{"role":"user","content":"earlier text"},
		{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"xx"}}]}
	]}`)
	resp, b := r.post(t, "/v1/messages", body)
	if resp.StatusCode != 400 || !strings.Contains(string(b), "question") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if called.Load() {
		t.Error("the upstream was reached with an empty question")
	}
}

func TestToSystemOneDropsReminderBlocks(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "<system-reminder>CLAUDE.md says: follow global instructions</system-reminder>"},
				{"type": "text", "text": "earlier"}
			]},
			{"role": "user", "content": [
				{"type": "text", "text": "<system-reminder>the task: judge the release</system-reminder>"},
				{"type": "text", "text": "is it green?"}
			]}
		]
	}`)
	question, state, _, err := toSystemOne(body)
	if err != nil {
		t.Fatal(err)
	}
	if question != "is it green?" {
		t.Errorf("question %q", question)
	}
	if state != "user: earlier" {
		t.Errorf("state %q", state)
	}
}

func TestToSystemOneRejectsMalformedMessage(t *testing.T) {
	_, _, _, err := toSystemOne([]byte(`{"messages": [{"role": "user", "content": "fine"}, "oops"]}`))
	if err == nil {
		t.Fatal("a malformed message must be an error, not a silent skip")
	}
}

func TestToSystemOneReportsTrailingTurns(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "user", "content": "earlier"},
			{"role": "user", "content": "is it green?"},
			{"role": "assistant", "content": "a prefill turn"}
		]
	}`)
	question, state, trailing, err := toSystemOne(body)
	if err != nil {
		t.Fatal(err)
	}
	if question != "is it green?" || state != "user: earlier" || trailing != 1 {
		t.Errorf("question %q state %q trailing %d", question, state, trailing)
	}
}

func TestGatewayJevSubagentRemindersDropped(t *testing.T) {
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(systemOneAnswer))
	})
	body := []byte(`{"model":"cubbit/mimir-jev","max_tokens":10,"stream":false,"messages":[
		{"role":"user","content":[
			{"type":"text","text":"<system-reminder>CLAUDE.md and the harness context for the agent</system-reminder>"},
			{"type":"text","text":"does the release pass its tests?"}
		]}
	]}`)
	resp, b := r.post(t, "/v1/messages", body)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	got := r.mimir.last(t)
	if got.path != "/v1/systemone" {
		t.Fatalf("upstream request %s", got.path)
	}
	var sent map[string]any
	_ = json.Unmarshal(got.body, &sent)
	q := sent["questions"].(map[string]any)["q"].(map[string]any)
	if q["criteria"].(map[string]any)["true"].(map[string]any)["text"] != "does the release pass its tests?" {
		t.Errorf("question %v", q["criteria"])
	}
	if sent["state"].(map[string]any)["text"] != "does the release pass its tests?" {
		t.Errorf("state %v", sent["state"])
	}
}

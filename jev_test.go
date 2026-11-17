package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
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
	question, state, err := toSystemOne(body)
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
	question, state, err := toSystemOne([]byte(`{"messages": [{"role": "user", "content": "The Earth is flat."}]}`))
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
	question, state, err := toSystemOne(body)
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
	question, _, err := toSystemOne(body)
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

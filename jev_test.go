package main

import (
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

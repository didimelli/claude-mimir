package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func topOf(t *testing.T, b []byte) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, b)
	}
	return m
}

const sampleBody = `{"model":"cubbit/mimir-pro","max_tokens":32000,"stream":true,
 "metadata":{"user_id":"acct"},"context_management":{"edits":[{"type":"clear_thinking_20251015","keep":"all"}]},
 "thinking":{"type":"adaptive","display":"omitted"},"output_config":{"effort":"high"},
 "custom_field":{"keep":"me","n":1.50},"tools":[{"name":"Read","description":"a <b> & c","input_schema":{"type":"object"}}],
 "system":[{"type":"text","text":"you are an agent"}],
 "messages":[{"role":"user","content":[{"type":"text","text":"task"}]},
  {"role":"system","content":[{"type":"text","text":"tools available"},{"type":"tool_addition","tool":{"type":"tool_definition"}}]},
  {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok","cache_control":{"type":"ephemeral"}}]},
  {"role":"system","content":"MCP needs auth"}]}`

func TestShapeMimirBody(t *testing.T) {
	out, info, err := shapeMimirBody([]byte(sampleBody), "")
	if err != nil {
		t.Fatal(err)
	}
	in, got := topOf(t, []byte(sampleBody)), topOf(t, out)
	for _, k := range mimirDropFields {
		if _, ok := got[k]; ok {
			t.Errorf("%s was forwarded", k)
		}
	}
	if len(info.Dropped) != 4 {
		t.Errorf("dropped %v", info.Dropped)
	}
	// everything else is forwarded exactly as received
	for _, k := range []string{"model", "max_tokens", "stream", "custom_field", "tools", "system"} {
		var a, b strings.Builder
		a.Write(compact(t, in[k]))
		b.Write(compact(t, got[k]))
		if a.String() != b.String() {
			t.Errorf("%s changed:\n in=%s\nout=%s", k, a.String(), b.String())
		}
	}
	var msgs []msg
	_ = json.Unmarshal(got["messages"], &msgs)
	if roles(msgs) != "uuauu" {
		t.Errorf("roles %s", roles(msgs))
	}
	if info.Folded != 2 || info.DroppedBlocks != 1 {
		t.Errorf("stats %+v", info.foldStats)
	}
	if strings.Contains(string(out), "\\u003c") || !strings.Contains(string(out), "<system-reminder>") {
		t.Errorf("HTML characters were escaped: %s", out)
	}
	if !strings.Contains(string(out), `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("a block of an untouched message lost its cache_control")
	}
}

func compact(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	return marshal(json.RawMessage(raw))
}

func TestShapeMimirBodyErrors(t *testing.T) {
	for _, bad := range []string{`{nope`, `{"messages":"x"}`, `{"messages":[1]}`} {
		if _, _, err := shapeMimirBody([]byte(bad), ""); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	// no messages at all is fine: nothing to fold
	if _, _, err := shapeMimirBody([]byte(`{"model":"x"}`), ""); err != nil {
		t.Errorf("no messages: %v", err)
	}
}

func TestRewriteMimirError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   int
		typ    string
		msg    string
	}{
		{"empty 401", 401, "", 401, "authentication_error", "MIMIR_API_KEY"},
		{"unknown model is not retryable", 500, `{"type":"error","error":{"type":"api_error","message":"no keys found that support model/deployment: nope"}}`, 404, "not_found_error", "no keys found"},
		{"context overflow", 400, `{"type":"error","error":{"type":"BadRequestError","message":"This model's maximum context length is 262144 tokens. However, your prompt contains at least 262145 input tokens, which exceeds this limit even without any output tokens."}}`, 400, "invalid_request_error", "prompt is too long: 262145 tokens > 262144 maximum"},
		{"context overflow with output tokens", 400, `{"type":"error","error":{"type":"BadRequestError","message":"This model's maximum context length is 917504 tokens. However, you requested 32000 output tokens and your prompt contains at least 900000 input tokens, for a total of at least 932000 tokens."}}`, 400, "invalid_request_error", "prompt is too long: 900000 tokens > 917504 maximum"},
	}
	for _, c := range cases {
		st, body, changed := rewriteMimirError(c.status, []byte(c.body))
		if !changed || st != c.want {
			t.Errorf("%s: changed=%v status=%d", c.name, changed, st)
			continue
		}
		var e struct {
			Error struct{ Type, Message string }
		}
		_ = json.Unmarshal(body, &e)
		if e.Error.Type != c.typ || !strings.Contains(e.Error.Message, c.msg) {
			t.Errorf("%s: %s", c.name, body)
		}
	}
	for _, c := range []struct {
		status int
		body   string
	}{
		{400, `{"type":"error","error":{"type":"BadRequestError","message":"something else"}}`},
		{500, `{"type":"error","error":{"type":"api_error","message":"boom"}}`},
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`},
		{502, `not json`},
		{401, `{"error":{"type":"authentication_error","message":"bad"}}`},
	} {
		if st, body, changed := rewriteMimirError(c.status, []byte(c.body)); changed || st != c.status || string(body) != c.body {
			t.Errorf("status %d %q was changed", c.status, c.body)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if estimateTokens(nil) != 1 || estimateTokens(make([]byte, 4000)) != 1000 {
		t.Fatal("estimate")
	}
}

func TestShapeMimirBodyPinsEffort(t *testing.T) {
	out, info, err := shapeMimirBody([]byte(sampleBody), "high")
	if err != nil {
		t.Fatal(err)
	}
	top := topOf(t, out)
	if string(top["thinking"]) != `{"type":"adaptive"}` || string(top["output_config"]) != `{"effort":"high"}` {
		t.Errorf("thinking %s output_config %s", top["thinking"], top["output_config"])
	}
	if info.Effort != "high" {
		t.Errorf("effort %q", info.Effort)
	}
	if _, ok := top["metadata"]; ok {
		t.Errorf("metadata forwarded")
	}
	// Claude Code asked for other values: the configured effort replaces them, and the rest of the body is as before.
	in := strings.Replace(sampleBody, `"effort":"high"`, `"effort":"medium"`, 1)
	out, _, _ = shapeMimirBody([]byte(in), "low")
	if got := string(topOf(t, out)["output_config"]); got != `{"effort":"low"}` {
		t.Errorf("output_config %s", got)
	}
	// A request without thinking fields gets them too.
	out, _, _ = shapeMimirBody([]byte(`{"model":"cubbit/mimir-pro","messages":[]}`), "high")
	if got := string(topOf(t, out)["output_config"]); got != `{"effort":"high"}` {
		t.Errorf("output_config %s", got)
	}
}

func TestShapeMimirBodyWithoutEffortDropsReasoningFields(t *testing.T) {
	out, info, _ := shapeMimirBody([]byte(sampleBody), "")
	top := topOf(t, out)
	if _, ok := top["thinking"]; ok {
		t.Errorf("thinking forwarded")
	}
	if _, ok := top["output_config"]; ok || info.Effort != "" {
		t.Errorf("output_config forwarded or effort set: %q", info.Effort)
	}
}

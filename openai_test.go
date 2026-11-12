package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- a strict check of an Anthropic Messages SSE stream (what Claude Code's parser expects) --------------------

type anthEvent struct {
	name string
	data map[string]any
}

func parseAnthSSE(t *testing.T, raw []byte) []anthEvent {
	t.Helper()
	var evs []anthEvent
	for _, ev := range strings.Split(strings.TrimSuffix(string(raw), "\n\n"), "\n\n") {
		if ev == "" {
			continue
		}
		var name, data string
		for _, l := range strings.Split(ev, "\n") {
			switch {
			case strings.HasPrefix(l, "event: "):
				name = l[7:]
			case strings.HasPrefix(l, "data: "):
				data = l[6:]
			default:
				t.Fatalf("unexpected line %q", l)
			}
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			t.Fatalf("bad JSON in %s: %v: %s", name, err, data)
		}
		if d["type"] != name {
			t.Fatalf("event %s carries type %v", name, d["type"])
		}
		evs = append(evs, anthEvent{name, d})
	}
	return evs
}

type streamResult struct {
	blocks     []map[string]any // content_block_start blocks, in order, with accumulated text / input JSON
	stopReason string
	usage      map[string]any
}

// checkAnthStream fails on anything a strict Anthropic client would reject and returns what the stream carried.
func checkAnthStream(t *testing.T, raw []byte) streamResult {
	t.Helper()
	evs := parseAnthSSE(t, raw)
	var res streamResult
	if len(evs) < 3 || evs[0].name != "message_start" || evs[len(evs)-1].name != "message_stop" {
		t.Fatalf("stream must start with message_start and end with message_stop: %s", raw)
	}
	open := -1
	acc := map[int]*strings.Builder{}
	sawDelta := false
	for _, e := range evs[1 : len(evs)-1] {
		if sawDelta {
			t.Fatalf("%s after message_delta", e.name)
		}
		switch e.name {
		case "content_block_start":
			i := int(e.data["index"].(float64))
			if open != -1 || i != len(res.blocks) {
				t.Fatalf("block %d started while %d open, or out of order", i, open)
			}
			open = i
			res.blocks = append(res.blocks, e.data["content_block"].(map[string]any))
			acc[i] = &strings.Builder{}
		case "content_block_delta":
			i := int(e.data["index"].(float64))
			if i != open {
				t.Fatalf("delta for block %d while %d open", i, open)
			}
			d := e.data["delta"].(map[string]any)
			switch res.blocks[i]["type"] {
			case "text":
				if d["type"] != "text_delta" {
					t.Fatalf("%v in a text block", d["type"])
				}
				acc[i].WriteString(d["text"].(string))
			case "tool_use":
				if d["type"] != "input_json_delta" {
					t.Fatalf("%v in a tool_use block", d["type"])
				}
				acc[i].WriteString(d["partial_json"].(string))
			}
		case "content_block_stop":
			i := int(e.data["index"].(float64))
			if i != open {
				t.Fatalf("stop for block %d while %d open", i, open)
			}
			if res.blocks[i]["type"] == "tool_use" && acc[i].Len() > 0 && !json.Valid([]byte(acc[i].String())) {
				t.Fatalf("tool input is not JSON: %s", acc[i].String())
			}
			open = -1
		case "message_delta":
			if open != -1 {
				t.Fatalf("message_delta while block %d open", open)
			}
			sawDelta = true
			res.stopReason, _ = e.data["delta"].(map[string]any)["stop_reason"].(string)
			res.usage, _ = e.data["usage"].(map[string]any)
		default:
			t.Fatalf("unexpected event %s", e.name)
		}
	}
	if !sawDelta {
		t.Fatal("no message_delta")
	}
	for i, b := range res.blocks {
		if b["type"] == "text" {
			b["text"] = acc[i].String()
		} else {
			b["input_json"] = acc[i].String()
		}
	}
	return res
}

// ---- OpenAI stream fixtures ----------------------------------------------------------------------------------

func oaiChunk(delta string, finish string) string {
	f := "null"
	if finish != "" {
		f = `"` + finish + `"`
	}
	return fmt.Sprintf(`data: {"id":"c1","model":"mimir-pro","choices":[{"index":0,"finish_reason":%s,"delta":%s}],"usage":null}`+"\n\n", f, delta)
}

const oaiUsageChunk = `data: {"id":"c1","model":"","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":178,"completion_tokens":35}}` + "\n\n"

// parallelToolStream is the shape Mimir's /v1 sends for three parallel tool calls (captured, reduced).
func parallelToolStream() string {
	var b strings.Builder
	b.WriteString(oaiChunk(`{"role":"assistant","content":""}`, ""))
	b.WriteString(oaiChunk(`{"reasoning":"I should read the three files."}`, ""))
	for i, f := range []string{"a", "b", "c"} {
		b.WriteString(oaiChunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"type":"function","id":"call-%s","function":{"name":"read_file","arguments":""}}]}`, i, f), ""))
		b.WriteString(oaiChunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"name":null,"arguments":"{\"path\": \"%s"}}]}`, i, f), ""))
		fin := ""
		if i == 2 {
			fin = "tool_calls"
		}
		b.WriteString(oaiChunk(fmt.Sprintf(`{"tool_calls":[{"index":%d,"function":{"name":null,"arguments":".txt\"}"}}]}`, i), fin))
	}
	b.WriteString(oaiUsageChunk)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func translateStream(t *testing.T, in io.Reader) ([]byte, *oaiStats, error) {
	t.Helper()
	st := &oaiStats{}
	out, err := io.ReadAll(newChatStream(io.NopCloser(in), st))
	return out, st, err
}

func TestChatStreamParallelToolCalls(t *testing.T) {
	out, st, err := translateStream(t, strings.NewReader(parallelToolStream()))
	if err != nil {
		t.Fatal(err)
	}
	res := checkAnthStream(t, out)
	if len(res.blocks) != 3 || res.stopReason != "tool_use" {
		t.Fatalf("blocks %v stop %q", res.blocks, res.stopReason)
	}
	for i, f := range []string{"a", "b", "c"} {
		b := res.blocks[i]
		if b["type"] != "tool_use" || b["id"] != "call-"+f || b["name"] != "read_file" || b["input_json"] != `{"path": "`+f+`.txt"}` {
			t.Errorf("block %d: %v", i, b)
		}
	}
	if res.usage["input_tokens"] != 178.0 || res.usage["output_tokens"] != 35.0 {
		t.Errorf("usage %v", res.usage)
	}
	if !st.HaveUsage || st.ToolCalls != 3 || st.ReasoningChunks != 1 || strings.Contains(string(out), "three files") {
		t.Errorf("stats %+v, or reasoning leaked", st)
	}
}

func TestChatStreamTextThenToolAndFinishes(t *testing.T) {
	for _, c := range []struct {
		name, finish, want string
		tool               bool
	}{
		{"text end", "stop", "end_turn", false},
		{"length", "length", "max_tokens", false},
		{"tool with finish stop", "stop", "tool_use", true},
		{"content filter", "content_filter", "refusal", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString(oaiChunk(`{"content":"Hel"}`, ""))
			b.WriteString(oaiChunk(`{"content":"lo"}`, ""))
			if c.tool {
				b.WriteString(oaiChunk(`{"tool_calls":[{"index":0,"id":"x","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}]}`, ""))
			}
			b.WriteString(oaiChunk(`{}`, c.finish))
			b.WriteString("data: [DONE]\n\n")
			out, _, err := translateStream(t, strings.NewReader(b.String()))
			if err != nil {
				t.Fatal(err)
			}
			res := checkAnthStream(t, out)
			if res.stopReason != c.want || res.blocks[0]["text"] != "Hello" {
				t.Fatalf("stop %q blocks %v", res.stopReason, res.blocks)
			}
		})
	}
}

func TestChatStreamCleanEndWithoutDone(t *testing.T) {
	in := oaiChunk(`{"content":"partial"}`, "")
	out, st, err := translateStream(t, strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if res := checkAnthStream(t, out); res.stopReason != "max_tokens" || !st.Truncated {
		t.Fatalf("stop %q truncated %v", res.stopReason, st.Truncated)
	}
}

type failingReader struct {
	r   io.Reader
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, f.err
	}
	return n, err
}

func TestChatStreamUpstreamErrorIsNotACleanEnd(t *testing.T) {
	boom := errors.New("connection reset")
	out, _, err := translateStream(t, &failingReader{strings.NewReader(oaiChunk(`{"content":"half"}`, "")), boom})
	if !errors.Is(err, boom) {
		t.Fatalf("want the upstream error, got %v", err)
	}
	if strings.Contains(string(out), "message_stop") {
		t.Fatalf("a broken stream must not end cleanly: %s", out)
	}
}

func TestChatStreamErrorChunkAndEmptyStream(t *testing.T) {
	out, st, _ := translateStream(t, strings.NewReader(oaiChunk(`{"content":"x"}`, "")+`data: {"error":{"message":"engine died"}}`+"\n\n"))
	if !strings.Contains(string(out), "event: error") || strings.Contains(string(out), "message_stop") || st.StreamError != "engine died" {
		t.Fatalf("%s %+v", out, st)
	}
	out, _, _ = translateStream(t, strings.NewReader(""))
	if !strings.Contains(string(out), "event: error") {
		t.Fatalf("an empty upstream stream must produce an error event: %q", out)
	}
}

func TestChatStreamLateFragmentDropped(t *testing.T) {
	var b strings.Builder
	b.WriteString(oaiChunk(`{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":"{\"x\":1}"}}]}`, ""))
	b.WriteString(oaiChunk(`{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":"{}"}}]}`, ""))
	b.WriteString(oaiChunk(`{"tool_calls":[{"index":0,"function":{"arguments":"garbage"}}]}`, "tool_calls"))
	b.WriteString("data: [DONE]\n\n")
	out, st, _ := translateStream(t, strings.NewReader(b.String()))
	res := checkAnthStream(t, out)
	if len(res.blocks) != 2 || st.LateFragments != 1 || res.blocks[0]["input_json"] != `{"x":1}` {
		t.Fatalf("%v %+v", res.blocks, st)
	}
}

// smallReader hands out the input in random small pieces, to prove the output does not depend on chunking.
type smallReader struct {
	b   []byte
	rng *rand.Rand
}

func (s *smallReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := 1 + s.rng.Intn(7)
	n = min(n, len(p), len(s.b))
	copy(p, s.b[:n])
	s.b = s.b[n:]
	return n, nil
}

func TestChatStreamIndependentOfChunking(t *testing.T) {
	in := []byte(parallelToolStream())
	want, _, _ := translateStream(t, bytes.NewReader(in))
	for seed := int64(0); seed < 200; seed++ {
		got, _, err := translateStream(t, &smallReader{append([]byte(nil), in...), rand.New(rand.NewSource(seed))})
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("seed %d: output differs (%v)", seed, err)
		}
	}
}

// ---- request translation -------------------------------------------------------------------------------------

func chatReq(t *testing.T, body string, effort string) (map[string]any, oaiInfo) {
	t.Helper()
	return chatReqReplay(t, body, effort, nil)
}

func chatReqReplay(t *testing.T, body string, effort string, replay map[string]any) (map[string]any, oaiInfo) {
	t.Helper()
	out, info, err := toChatCompletions([]byte(body), effort, replay, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m, info
}

func TestToChatCompletionsConversation(t *testing.T) {
	body := `{"model":"cubbit/mimir-pro","max_tokens":32000,"stream":true,"metadata":{"user_id":"x"},
	 "thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"context_management":{},
	 "system":[{"type":"text","text":"you are an agent","cache_control":{"type":"ephemeral"}}],
	 "tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],
	 "tool_choice":{"type":"any","disable_parallel_tool_use":true},"stop_sequences":["END"],"temperature":0.2,
	 "messages":[
	  {"role":"user","content":"task"},
	  {"role":"system","content":[{"type":"text","text":"tools available"}]},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"s"},{"type":"text","text":"reading"},{"type":"tool_use","id":"t1","name":"Read","input":{"path":"a"}},{"type":"tool_use","id":"t2","name":"Read","input":{"path":"b"}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"A"},{"type":"tool_result","tool_use_id":"t2","is_error":true,"content":[{"type":"text","text":"no such file"}]},{"type":"text","text":"go on"}]},
	  {"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Read","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t3","content":[{"type":"text","text":"see image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]}]}
	 ]}`
	m, info := chatReq(t, body, "low")
	for _, k := range []string{"metadata", "thinking", "output_config", "context_management", "max_tokens", "system", "stop_sequences"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s forwarded", k)
		}
	}
	if m["max_completion_tokens"] != 32000.0 || m["reasoning_effort"] != "low" || m["temperature"] != 0.2 || m["tool_choice"] != "required" || m["parallel_tool_calls"] != false {
		t.Errorf("fields: %v", m)
	}
	if so := m["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream_options %v", so)
	}
	if stop := m["stop"].([]any); stop[0] != "END" {
		t.Errorf("stop %v", stop)
	}
	tools := m["tools"].([]any)
	if len(tools) != 1 || info.DroppedTools != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "Read" {
		t.Errorf("tools %v dropped %d", tools, info.DroppedTools)
	}
	var roles []string
	msgs := m["messages"].([]any)
	for _, x := range msgs {
		roles = append(roles, x.(map[string]any)["role"].(string))
	}
	want := "system user user assistant tool tool user assistant tool user"
	if strings.Join(roles, " ") != want {
		t.Fatalf("roles %v, want %s", roles, want)
	}
	if msgs[0].(map[string]any)["content"] != "you are an agent" {
		t.Errorf("system %v", msgs[0])
	}
	if c := msgs[2].(map[string]any)["content"].(string); !strings.Contains(c, "<system-reminder>") || !strings.Contains(c, "tools available") {
		t.Errorf("folded system entry %q", c)
	}
	a := msgs[3].(map[string]any)
	tcs := a["tool_calls"].([]any)
	if a["content"] != "reading" || len(tcs) != 2 || tcs[1].(map[string]any)["id"] != "t2" ||
		tcs[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"path":"a"}` || info.DroppedBlocks != 1 {
		t.Errorf("assistant %v dropped %d", a, info.DroppedBlocks)
	}
	if t2 := msgs[5].(map[string]any); t2["tool_call_id"] != "t2" || t2["content"] != "Error: no such file" {
		t.Errorf("error tool result %v", t2)
	}
	if u := msgs[6].(map[string]any); u["content"] != "go on" {
		t.Errorf("text after tool results %v", u)
	}
	if a2 := msgs[7].(map[string]any); a2["content"] != nil {
		t.Errorf("tool-only assistant turn should have null content: %v", a2)
	}
	last := msgs[9].(map[string]any)["content"].([]any)
	if img := last[0].(map[string]any); img["type"] != "image_url" || img["image_url"].(map[string]any)["url"] != "data:image/png;base64,AAAA" {
		t.Errorf("image from a tool result %v", last)
	}
	if tr := msgs[8].(map[string]any); tr["content"] != "see image" {
		t.Errorf("tool result text %v", tr)
	}
}

func TestToChatCompletionsToolChoiceAndDefaults(t *testing.T) {
	base := `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]`
	for choice, want := range map[string]any{`"auto"`: "auto", `"none"`: "none", `"tool","name":"f"`: map[string]any{"type": "function", "function": map[string]any{"name": "f"}}} {
		m, _ := chatReq(t, base+`,"tool_choice":{"type":`+choice+`}}`, "")
		if fmt.Sprint(m["tool_choice"]) != fmt.Sprint(want) {
			t.Errorf("%s -> %v", choice, m["tool_choice"])
		}
	}
	m, _ := chatReq(t, `{"model":"m","messages":[{"role":"user","content":"x"}]}`, "")
	for _, k := range []string{"reasoning_effort", "stream_options", "tools", "max_completion_tokens"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s set without being asked for", k)
		}
	}
}

const thinkingTurn = `{"model":"cubbit/mimir","messages":[
	  {"role":"user","content":"task"},
	  {"role":"assistant","content":[{"type":"thinking","thinking":"first","signature":"s"},{"type":"redacted_thinking","data":"x"},
	   {"type":"thinking","thinking":"second","signature":"s"},{"type":"text","text":"reading"},{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
	  {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"A"}]}]`

func TestToChatCompletionsReplaysThinking(t *testing.T) {
	m, info := chatReqReplay(t, thinkingTurn+`}`, "", map[string]any{"preserve_thinking": true})
	a := m["messages"].([]any)[1].(map[string]any)
	if a["reasoning_content"] != "first\nsecond" || a["content"] != "reading" || len(a["tool_calls"].([]any)) != 1 {
		t.Errorf("assistant %v", a)
	}
	if info.Replayed != 2 || info.DroppedBlocks != 1 { // redacted_thinking is still dropped
		t.Errorf("replayed %d dropped %d", info.Replayed, info.DroppedBlocks)
	}
	if fmt.Sprint(m["chat_template_kwargs"]) != "map[preserve_thinking:true]" {
		t.Errorf("kwargs %v", m["chat_template_kwargs"])
	}

	// mimir-slim: no replay, thinking dropped as before.
	m, info = chatReq(t, thinkingTurn+`}`, "high")
	a = m["messages"].([]any)[1].(map[string]any)
	if _, ok := a["reasoning_content"]; ok || info.Replayed != 0 || info.DroppedBlocks != 3 {
		t.Errorf("slim assistant %v replayed %d dropped %d", a, info.Replayed, info.DroppedBlocks)
	}
	if _, ok := m["chat_template_kwargs"]; ok {
		t.Errorf("kwargs sent without replay: %v", m["chat_template_kwargs"])
	}
}

func TestToChatCompletionsMergesTemplateKwargs(t *testing.T) {
	body := thinkingTurn + `,"chat_template_kwargs":{"enable_thinking":true,"clear_thinking":true}}`
	m, _ := chatReqReplay(t, body, "", map[string]any{"clear_thinking": false, "extra": 1})
	if fmt.Sprint(m["chat_template_kwargs"]) != "map[clear_thinking:true enable_thinking:true extra:1]" {
		t.Errorf("kwargs %v", m["chat_template_kwargs"])
	}
}

func TestGatewayOpenAIReplaysThinkingPerModel(t *testing.T) {
	r := newOpenAIRigWith(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"x","model":"m","choices":[{"finish_reason":"stop","message":{"content":"ok"}}]}`))
	}, func(c *config) {
		c.effort = map[string]string{"cubbit/mimir": "medium", "cubbit/mimir-slim": "high"}
		c.replay = map[string]map[string]any{"cubbit/mimir": {"preserve_thinking": true}}
	})
	for _, model := range []string{"cubbit/mimir", "cubbit/mimir-slim"} {
		r.post(t, "/v1/messages", []byte(strings.Replace(thinkingTurn+`}`, "cubbit/mimir", model, 1)))
		var sent map[string]any
		_ = json.Unmarshal(r.mimir.last(t).body, &sent)
		a := sent["messages"].([]any)[1].(map[string]any)
		replayed := a["reasoning_content"] == "first\nsecond"
		if replayed != (model == "cubbit/mimir") || sent["reasoning_effort"] != map[string]string{"cubbit/mimir": "medium", "cubbit/mimir-slim": "high"}[model] {
			t.Errorf("%s sent %v", model, sent)
		}
	}
	lg := r.logs(t)
	if len(lg) != 2 || lg[0]["replayed_thinking"] != 2.0 || lg[0]["dropped_blocks"] != 1.0 || lg[1]["replayed_thinking"] != nil || lg[1]["dropped_blocks"] != 3.0 {
		t.Errorf("log %v", lg)
	}
}

// ---- non-streamed replies and errors -------------------------------------------------------------------------

func TestFromChatCompletion(t *testing.T) {
	var st oaiStats
	out, err := fromChatCompletion([]byte(`{"id":"c9","model":"mimir-slim","choices":[{"finish_reason":"tool_calls","message":{"content":"ok","reasoning":"secret","tool_calls":[{"id":"x1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}},{"id":"","type":"function","function":{"name":"g","arguments":"not json"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":4}}`), &st)
	u := st.Usage
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	c := m["content"].([]any)
	if m["stop_reason"] != "tool_use" || len(c) != 3 || u.PromptTokens != 10 || strings.Contains(string(out), "secret") {
		t.Fatalf("%s", out)
	}
	if fmt.Sprint(c[1].(map[string]any)["input"]) != "map[a:1]" || fmt.Sprint(c[2].(map[string]any)["input"]) != "map[]" {
		t.Errorf("inputs %v %v", c[1], c[2])
	}
	if id := c[2].(map[string]any)["id"]; id != "toolu_c9_1" || fmt.Sprint(st.ThoughtIDs) != "[x1 toolu_c9_1]" { // as the stream path names it
		t.Errorf("empty id became %v, cached under %v", id, st.ThoughtIDs)
	}
}

func TestTranslateOpenAIError(t *testing.T) {
	for _, c := range []struct {
		name     string
		status   int
		body     string
		want     int
		wantType string
		wantMsg  string
	}{
		{"unknown model", 400, `{"is_bifrost_error":false,"error":{"error":"no keys found that support model/deployment: nope","message":"no keys found that support model/deployment: nope"}}`, 404, "not_found_error", "no keys found"},
		{"empty 401", 401, ``, 401, "authentication_error", "MIMIR_API_KEY"},
		{"context, vllm requested", 400, `{"error":{"message":"This model's maximum context length is 262144 tokens. However, you requested 300000 tokens (290000 in the messages, 10000 in the completion)."}}`, 400, "invalid_request_error", "prompt is too long: 300000 tokens > 262144 maximum"},
		{"context, prompt contains", 400, `{"error":{"message":"maximum context length is 1000 tokens. However, your prompt contains at least 1200 input tokens"}}`, 400, "invalid_request_error", "prompt is too long: 1200 tokens > 1000 maximum"},
		{"rate limit", 429, `{"error":{"message":"slow down"}}`, 429, "rate_limit_error", "slow down"},
		{"server", 503, `upstream down`, 503, "api_error", "upstream down"},
	} {
		t.Run(c.name, func(t *testing.T) {
			st, b := translateOpenAIError(c.status, []byte(c.body))
			var e struct {
				Type  string `json:"type"`
				Error struct{ Type, Message string }
			}
			_ = json.Unmarshal(b, &e)
			if st != c.want || e.Type != "error" || e.Error.Type != c.wantType || !strings.Contains(e.Error.Message, c.wantMsg) {
				t.Fatalf("%d %s", st, b)
			}
		})
	}
}

// ---- through the gateway -------------------------------------------------------------------------------------

func newOpenAIRig(t *testing.T, mimirH http.HandlerFunc) *rig {
	t.Helper()
	return newOpenAIRigWith(t, mimirH, nil)
}

// newOpenAIRigWith is newOpenAIRig with a chance to change the gateway's configuration.
func newOpenAIRigWith(t *testing.T, mimirH http.HandlerFunc, tweak func(*config)) *rig {
	t.Helper()
	r := &rig{anth: newFake(t, nil), mimir: newFake(t, mimirH), logPath: filepath.Join(t.TempDir(), "log.jsonl")}
	cfg := config{anthropic: r.anth.URL, logPath: r.logPath, prefix: []string{"cubbit/"},
		mimirURL: r.mimir.URL + "/v1", mimirKey: "mimir-test-key", mimirAPI: "openai", effort: map[string]string{"cubbit/mimir-pro": "low"},
		runaway: "log", maxReplyText: 24 << 10}
	if tweak != nil {
		tweak(&cfg)
	}
	g, err := newGateway(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.g, r.srv = g, httptest.NewServer(g)
	t.Cleanup(r.srv.Close)
	return r
}

func TestGatewayOpenAIStream(t *testing.T) {
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream-Only", "1")
		_, _ = w.Write([]byte(parallelToolStream()))
	})
	resp, b := r.post(t, "/v1/messages?beta=true", convo(true))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("X-Upstream-Only") != "" {
		t.Fatalf("%d %v", resp.StatusCode, resp.Header)
	}
	if res := checkAnthStream(t, b); len(res.blocks) != 3 {
		t.Fatalf("%v", res.blocks)
	}
	got := r.mimir.last(t)
	if got.path != "/v1/chat/completions" || got.header.Get("Authorization") != "Bearer mimir-test-key" || got.header.Get("X-Api-Key") != "" || got.header.Get("Anthropic-Beta") != "" {
		t.Fatalf("upstream request %s %v", got.path, got.header)
	}
	var sent map[string]any
	_ = json.Unmarshal(got.body, &sent)
	if sent["reasoning_effort"] != "low" || sent["stream"] != true || strings.Contains(string(got.body), secretText) == false {
		t.Errorf("translated body %s", got.body)
	}
	lg := r.logs(t)
	if len(lg) != 1 || lg[0]["mimir_api"] != "openai" || lg[0]["input_tokens"] != 178.0 || lg[0]["output_tokens"] != 35.0 || lg[0]["tool_calls"] != 3.0 || lg[0]["error"] != nil {
		t.Fatalf("log %v", lg)
	}
	if strings.Contains(string(mustRead(t, r.logPath)), secretText) {
		t.Fatal("prompt text reached the log")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGatewayOpenAINonStreamAndErrors(t *testing.T) {
	unknownModel := false // the fake upstream has already read the body, so the test tells the handler what to do
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if unknownModel {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"error":"no keys found that support model/deployment: nope","message":"no keys found that support model/deployment: nope"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c1","model":"mimir-slim","choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`))
	})
	resp, b := r.post(t, "/v1/messages", []byte(`{"model":"cubbit/mimir-slim","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`))
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if resp.StatusCode != 200 || m["type"] != "message" || m["stop_reason"] != "end_turn" || fmt.Sprint(m["content"]) != "[map[text:hi type:text]]" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	unknownModel = true
	resp, b = r.post(t, "/v1/messages", []byte(`{"model":"cubbit/nope","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`))
	if resp.StatusCode != 404 || !strings.Contains(string(b), "not_found_error") {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	unknownModel = false
	// count_tokens stays local; the Claude leg is untouched in openai mode
	if resp, b := r.post(t, "/v1/messages/count_tokens", []byte(`{"model":"cubbit/mimir-slim","messages":[{"role":"user","content":"hello"}]}`)); resp.StatusCode != 200 || !strings.Contains(string(b), "input_tokens") {
		t.Fatalf("count_tokens %d %s", resp.StatusCode, b)
	}
	claude := []byte(strings.Replace(string(convo(true)), "cubbit/mimir-pro", "claude-sonnet-5-5", 1))
	r.post(t, "/v1/messages?beta=true", claude)
	if got := r.anth.last(t); !bytes.Equal(got.body, claude) || got.path != "/v1/messages?beta=true" {
		t.Fatalf("Claude leg changed: %s", got.path)
	}
}

func TestParseConfigMimirAPI(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cfg, err := parseConfig([]string{"--mimir-api", "openai"}, env(map[string]string{"MIMIR_API_KEY": "k", "MIMIR_OPENAI_URL": "https://m/v1/"}))
	if err != nil || cfg.mimirAPI != "openai" || cfg.mimirURL != "https://m/v1" ||
		fmt.Sprint(cfg.effort) != "map[cubbit/mimir:medium cubbit/mimir-pro:high cubbit/mimir-slim:high]" ||
		fmt.Sprint(cfg.replay) != "map[cubbit/mimir:map[preserve_thinking:true] cubbit/mimir-pro:map[clear_thinking:false]]" {
		t.Fatalf("%+v %v", cfg, err)
	}
	cfg, err = parseConfig([]string{"--mimir-effort", "cubbit/mimir-pro=none"}, env(map[string]string{"MIMIR_API": "openai", "MIMIR_API_KEY": "k", "MIMIR_OPENAI_URL": "https://m/v1"}))
	if err != nil || cfg.effort["cubbit/mimir-pro"] != "none" {
		t.Fatalf("openai accepts effort none: %+v %v", cfg, err)
	}
	if _, err := parseConfig([]string{"--mimir-effort", "cubbit/mimir-pro=none"}, env(map[string]string{"MIMIR_API_KEY": "k", "MIMIR_ANTHROPIC_URL": "https://m/anthropic"})); err == nil {
		t.Error("anthropic must reject effort none")
	}
	if _, err := parseConfig([]string{"--mimir-api", "openai"}, env(map[string]string{"MIMIR_API_KEY": "k", "MIMIR_ANTHROPIC_URL": "https://m/anthropic"})); err == nil || !strings.Contains(err.Error(), "MIMIR_OPENAI_URL") {
		t.Errorf("openai without its URL: %v", err)
	}
	if _, err := parseConfig([]string{"--mimir-api", "grpc"}, env(map[string]string{"MIMIR_API_KEY": "k"})); err == nil {
		t.Error("unknown API accepted")
	}
}

// ---- runaway telemetry ---------------------------------------------------------------------------------------

// loopStream is a reply that deliberates in the reasoning, then prints one line over and over.
func loopStream(reasoningLines, textLines int, line string) string {
	var b strings.Builder
	b.WriteString(oaiChunk(`{"role":"assistant","content":""}`, ""))
	for i := 0; i < reasoningLines; i++ {
		b.WriteString(oaiChunk(`{"reasoning":"hmm\n"}`, ""))
	}
	for i := 0; i < textLines; i++ {
		b.WriteString(oaiChunk(fmt.Sprintf(`{"content":%q}`, line+"\n"), ""))
	}
	b.WriteString(oaiChunk(`{}`, "length"))
	b.WriteString(oaiUsageChunk)
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func TestChatStreamMeasuresTextAndReasoning(t *testing.T) {
	out, st, err := translateStream(t, strings.NewReader(loopStream(35, 40, "---")))
	if err != nil {
		t.Fatal(err)
	}
	checkAnthStream(t, out)
	if !st.Measured || st.Text.Chars != 160 || st.Text.MaxRepeat != 40 || st.Reasoning.Chars != 35*4 || st.Reasoning.MaxRepeat != 35 {
		t.Fatalf("measured %v text %d/%d reasoning %d/%d", st.Measured, st.Text.Chars, st.Text.MaxRepeat, st.Reasoning.Chars, st.Reasoning.MaxRepeat)
	}
	if got := st.runaway(1 << 20); got != "text_repeat,reasoning_repeat" {
		t.Fatalf("runaway %q", got)
	}
	if got := st.runaway(100); got != "text_budget,text_repeat,reasoning_repeat" {
		t.Fatalf("runaway with a 100-byte budget %q", got)
	}
	_, ok, _ := translateStream(t, strings.NewReader(parallelToolStream()))
	if got := ok.runaway(1 << 20); got != "" || ok.Text.Chars != 0 {
		t.Fatalf("a healthy reply was flagged: %q", got)
	}
}

func TestChatStreamMeasuresIndependentlyOfChunking(t *testing.T) {
	in := []byte(loopStream(35, 40, "---"))
	want, _, _ := translateStream(t, bytes.NewReader(in))
	for seed := int64(0); seed < 50; seed++ {
		got, st, err := translateStream(t, &smallReader{append([]byte(nil), in...), rand.New(rand.NewSource(seed))})
		if err != nil || !bytes.Equal(got, want) || st.Text.MaxRepeat != 40 || st.Reasoning.Chars != 140 {
			t.Fatalf("seed %d: %v %d %d", seed, err, st.Text.MaxRepeat, st.Reasoning.Chars)
		}
	}
}

func TestGatewayLogsRunawayWithoutText(t *testing.T) {
	const loopLine = "LOOP-LINE-do-not-log"
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(loopStream(0, 45, loopLine)))
	})
	if resp, b := r.post(t, "/v1/messages", convo(true)); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	lg := r.logs(t)
	if len(lg) != 1 || lg[0]["runaway"] != "text_repeat" || lg[0]["text_max_repeat"] != 45.0 || lg[0]["text_chars"] != float64(45*(len(loopLine)+1)) || lg[0]["reasoning_chars"] != 0.0 {
		t.Fatalf("log %v", lg)
	}
	if strings.Contains(string(mustRead(t, r.logPath)), loopLine) {
		t.Fatal("reply text reached the log")
	}

	healthy := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(parallelToolStream()))
	})
	healthy.post(t, "/v1/messages", convo(true))
	if lg := healthy.logs(t); len(lg) != 1 || lg[0]["runaway"] != nil || lg[0]["text_chars"] != 0.0 {
		t.Fatalf("healthy log %v", lg)
	}
}

func TestGatewayRunawayBudgetAndOff(t *testing.T) {
	long := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		var b strings.Builder
		b.WriteString(oaiChunk(`{"role":"assistant","content":""}`, ""))
		for i := 0; i < 30; i++ { // varied lines: no repetition, only length
			b.WriteString(oaiChunk(fmt.Sprintf(`{"content":"line %d of a long deliberation\n"}`, i), ""))
		}
		b.WriteString(oaiChunk(`{}`, "stop") + oaiUsageChunk + "data: [DONE]\n\n")
		_, _ = w.Write([]byte(b.String()))
	}
	r := newOpenAIRigWith(t, long, func(c *config) { c.maxReplyText = 200 })
	r.post(t, "/v1/messages", convo(true))
	if lg := r.logs(t); len(lg) != 1 || lg[0]["runaway"] != "text_budget" || lg[0]["text_max_repeat"] != 1.0 {
		t.Fatalf("budget log %v", lg)
	}
	off := newOpenAIRigWith(t, long, func(c *config) { c.runaway = "off"; c.maxReplyText = 200 })
	off.post(t, "/v1/messages", convo(true))
	lg := off.logs(t)
	if len(lg) != 1 || lg[0]["runaway"] != nil || lg[0]["text_chars"] != nil || lg[0]["reasoning_chars"] != nil {
		t.Fatalf("off must add nothing to the log: %v", lg)
	}
}

func TestParseConfigRunaway(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	base := map[string]string{"MIMIR_API_KEY": "k", "MIMIR_ANTHROPIC_URL": "https://m/anthropic"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	cfg, err := parseConfig(nil, env(base))
	if err != nil || cfg.runaway != "log" || cfg.maxReplyText != 24<<10 {
		t.Fatalf("defaults: %q %d %v", cfg.runaway, cfg.maxReplyText, err)
	}
	cfg, err = parseConfig(nil, env(with(map[string]string{"IT_RUNAWAY_GUARD": "off", "IT_MAX_REPLY_TEXT": "1000"})))
	if err != nil || cfg.runaway != "off" || cfg.maxReplyText != 1000 {
		t.Fatalf("environment: %q %d %v", cfg.runaway, cfg.maxReplyText, err)
	}
	cfg, err = parseConfig([]string{"--runaway-guard", "log", "--max-reply-text", "500"}, env(with(map[string]string{"IT_RUNAWAY_GUARD": "off"})))
	if err != nil || cfg.runaway != "log" || cfg.maxReplyText != 500 {
		t.Fatalf("flags beat the environment: %q %d %v", cfg.runaway, cfg.maxReplyText, err)
	}
	for _, bad := range [][]string{{"--runaway-guard", "cut"}, {"--runaway-guard", "banana"}, {"--max-reply-text", "0"}, {"--max-reply-text", "-5"}} {
		if _, err := parseConfig(bad, env(base)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := parseConfig(nil, env(with(map[string]string{"IT_MAX_REPLY_TEXT": "lots"}))); err == nil {
		t.Error("a non-numeric IT_MAX_REPLY_TEXT was accepted")
	}
}

func TestChatStreamCountsATrailingLineWithoutNewline(t *testing.T) {
	var b strings.Builder
	b.WriteString(oaiChunk(`{"role":"assistant","content":""}`, ""))
	for i := 0; i < 29; i++ {
		b.WriteString(oaiChunk(`{"content":"same\n"}`, ""))
	}
	b.WriteString(oaiChunk(`{"content":"same"}`, "stop")) // the reply ends in the middle of the 30th line
	b.WriteString(oaiUsageChunk + "data: [DONE]\n\n")
	_, st, err := translateStream(t, strings.NewReader(b.String()))
	if err != nil || st.Text.MaxRepeat != 30 {
		t.Fatalf("repeat %d, want 30 (%v)", st.Text.MaxRepeat, err)
	}
}

func TestLogCarriesLocalAndUTCTimestamps(t *testing.T) {
	r := newOpenAIRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(parallelToolStream()))
	})
	r.post(t, "/v1/messages", convo(true))
	lg := r.logs(t)
	if len(lg) != 1 {
		t.Fatalf("log %v", lg)
	}
	utc, err := time.Parse(time.RFC3339, fmt.Sprint(lg[0]["ts_utc"]))
	if err != nil || !strings.HasSuffix(fmt.Sprint(lg[0]["ts_utc"]), "Z") {
		t.Fatalf("ts_utc %v: %v", lg[0]["ts_utc"], err)
	}
	if time.Since(utc) > time.Minute || time.Until(utc) > time.Minute {
		t.Fatalf("ts_utc %v is not now", utc)
	}
	if want := utc.In(time.Local).Format("2006-01-02T15:04:05"); lg[0]["ts"] != want {
		t.Fatalf("ts %v is not ts_utc in local time (%s)", lg[0]["ts"], want)
	}
}

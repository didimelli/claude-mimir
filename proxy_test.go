package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretText = "SECRET-PROMPT-TEXT-do-not-log"

type seenReq struct {
	path   string
	header http.Header
	body   []byte
}

type fakeUp struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenReq
}

func newFake(t *testing.T, h http.HandlerFunc) *fakeUp {
	t.Helper()
	f := &fakeUp{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, seenReq{r.URL.RequestURI(), r.Header.Clone(), b})
		f.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUp) last(t *testing.T) seenReq {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		t.Fatal("upstream saw no request")
	}
	return f.seen[len(f.seen)-1]
}

func (f *fakeUp) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.seen) }

type rig struct {
	srv         *httptest.Server
	anth, mimir *fakeUp
	logPath     string
	g           *gateway
}

func newRig(t *testing.T, mimirH, anthH http.HandlerFunc) *rig {
	t.Helper()
	r := &rig{anth: newFake(t, anthH), mimir: newFake(t, mimirH), logPath: filepath.Join(t.TempDir(), "log.jsonl")}
	g, err := newGateway(config{anthropic: r.anth.URL, litellm: "http://127.0.0.1:1", litellmKey: "k", logPath: r.logPath,
		prefix: []string{"cubbit/"}, mimirURL: r.mimir.URL + "/anthropic", mimirKey: "mimir-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	r.g, r.srv = g, httptest.NewServer(g)
	t.Cleanup(r.srv.Close)
	return r
}

var claudeHeaders = map[string]string{
	"Authorization": "Bearer sk-ant-oat01-FAKE", "Anthropic-Beta": "oauth-2025-04-20,claude-code-20250219", "Anthropic-Version": "2023-06-01",
	"Content-Type": "application/json", "X-Claude-Code-Session-Id": "sess-1", "X-Litellm-Api-Key": "Bearer sk-local-gateway",
	"X-Stainless-Lang": "js", "Accept-Encoding": "gzip, br",
}

func (r *rig) post(t *testing.T, path string, body []byte) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest("POST", r.srv.URL+path, bytes.NewReader(body))
	for k, v := range claudeHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (r *rig) logs(t *testing.T) []map[string]any {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	data, _ := os.ReadFile(r.logPath)
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("bad log line %q", l)
		}
		out = append(out, m)
	}
	return out
}

func convo(more bool) []byte {
	m := `[{"role":"user","content":[{"type":"text","text":"` + secretText + `"}]},
	 {"role":"system","content":[{"type":"text","text":"tools available"},{"type":"tool_addition","tool":{"type":"tool_definition"}}]},
	 {"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}`
	if more {
		m += `,{"role":"system","content":"MCP needs auth"},{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Read","input":{}}]},
		 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"ok"}]}`
	}
	return []byte(`{"model":"cubbit/mimir-pro","max_tokens":32000,"stream":true,"metadata":{"user_id":"acct"},
	 "context_management":{"edits":[]},"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},
	 "custom_field":{"keep":"me"},"tools":[{"name":"Read"}],"messages":` + m + `]}`)
}

func TestDirectMimirRequest(t *testing.T) {
	r := newRig(t, nil, nil)
	resp, _ := r.post(t, "/v1/messages?beta=true", convo(false))
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	got := r.mimir.last(t)
	if got.path != "/anthropic/v1/messages?beta=true" {
		t.Errorf("path %s", got.path)
	}
	if got.header.Get("X-Api-Key") != "mimir-test-key" {
		t.Errorf("x-api-key not set")
	}
	for _, h := range []string{"Authorization", "Anthropic-Beta", "X-Claude-Code-Session-Id", "X-Litellm-Api-Key", "X-Stainless-Lang", "Accept-Encoding"} {
		if got.header.Get(h) != "" {
			t.Errorf("%s was forwarded to Mimir", h)
		}
	}
	if got.header.Get("Anthropic-Version") != "2023-06-01" || got.header.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", got.header)
	}
	top := topOf(t, got.body)
	for _, k := range mimirDropFields {
		if _, ok := top[k]; ok {
			t.Errorf("%s forwarded", k)
		}
	}
	if string(top["custom_field"]) != `{"keep":"me"}` || string(top["tools"]) != `[{"name":"Read"}]` {
		t.Errorf("untouched fields changed: %s %s", top["custom_field"], top["tools"])
	}
	if strings.Contains(string(got.body), `"role":"system"`) || strings.Contains(string(got.body), "tool_addition") {
		t.Errorf("system entry or tool_addition forwarded: %s", got.body)
	}
	if r.anth.count() != 0 {
		t.Errorf("Anthropic saw a Mimir request")
	}
}

func TestClaudeLegUntouchedInDirectMode(t *testing.T) {
	r := newRig(t, nil, nil)
	body := []byte(strings.Replace(string(convo(true)), "cubbit/mimir-pro", "claude-sonnet-5-5", 1))
	r.post(t, "/v1/messages?beta=true", body)
	got := r.anth.last(t)
	if sha256.Sum256(got.body) != sha256.Sum256(body) {
		t.Errorf("Claude body changed")
	}
	if got.header.Get("Authorization") != "Bearer sk-ant-oat01-FAKE" || got.header.Get("Anthropic-Beta") == "" || got.header.Get("X-Litellm-Api-Key") != "" {
		t.Errorf("Claude headers wrong: %v", got.header)
	}
	if r.mimir.count() != 0 {
		t.Errorf("Mimir saw a Claude request")
	}
}

func TestDirectStreamingNotBuffered(t *testing.T) {
	r := newRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
			fl.Flush()
			time.Sleep(300 * time.Millisecond)
		}
	}, nil)
	req, _ := http.NewRequest("POST", r.srv.URL+"/v1/messages", bytes.NewReader(convo(false)))
	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	_, _ = resp.Body.Read(buf)
	first := time.Since(t0)
	_, _ = io.Copy(io.Discard, resp.Body)
	if first > 250*time.Millisecond || time.Since(t0) < 800*time.Millisecond {
		t.Errorf("first chunk after %v, total %v: the stream was buffered", first, time.Since(t0))
	}
}

func TestCountTokensIsAnsweredLocally(t *testing.T) {
	r := newRig(t, nil, nil)
	resp, b := r.post(t, "/v1/messages/count_tokens", convo(false))
	var out struct {
		InputTokens int `json:"input_tokens"`
	}
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode != 200 || out.InputTokens < 1 || r.mimir.count() != 0 {
		t.Fatalf("status %d body %s mimir calls %d", resp.StatusCode, b, r.mimir.count())
	}
	if lg := r.logs(t); len(lg) != 1 || lg[0]["estimated"] != true {
		t.Errorf("log %v", lg)
	}
}

func TestMimirErrorsAreRewritten(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
		msg    string
	}{
		{401, "", 401, "MIMIR_API_KEY"},
		{500, `{"type":"error","error":{"type":"api_error","message":"no keys found that support model/deployment: nope"}}`, 404, "no keys found"},
		{400, `{"type":"error","error":{"type":"BadRequestError","message":"maximum context length is 100 tokens. However, your prompt contains at least 101 input tokens"}}`, 400, "prompt is too long: 101 tokens > 100 maximum"},
		{429, `{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`, 429, "slow"},
	}
	for _, c := range cases {
		c := c
		r := newRig(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Request-Id", "req_x")
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}, nil)
		resp, b := r.post(t, "/v1/messages", convo(false))
		if resp.StatusCode != c.want || !strings.Contains(string(b), c.msg) {
			t.Errorf("%d -> %d %s", c.status, resp.StatusCode, b)
		}
		if resp.Header.Get("Request-Id") != "req_x" {
			t.Errorf("request id header lost")
		}
	}
}

func TestMimirDownGives502(t *testing.T) {
	r := newRig(t, nil, nil)
	r.mimir.Close()
	resp, b := r.post(t, "/v1/messages", convo(false))
	var e struct{ Type string }
	_ = json.Unmarshal(b, &e)
	if resp.StatusCode != 502 || e.Type != "error" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
}

func TestHealthDirect(t *testing.T) {
	r := newRig(t, nil, nil)
	resp, err := http.Get(r.srv.URL + "/router/health")
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if h["router"] != "ok" || h["litellm"] != "not used" || !strings.HasPrefix(h["mimir"], "direct") {
		t.Fatalf("%v", h)
	}
	if resp, _ := http.Get(r.srv.URL + "/health/liveliness"); resp.StatusCode != 200 {
		t.Fatal("liveliness")
	}
}

func TestLogHoldsMetadataOnly(t *testing.T) {
	r := newRig(t, nil, nil)
	r.post(t, "/v1/messages", convo(true))
	r.post(t, "/v1/messages", []byte(strings.Replace(string(convo(false)), "cubbit/mimir-pro", "claude-haiku-5-5", 1)))
	data, _ := os.ReadFile(r.logPath)
	for _, leak := range []string{secretText, "mimir-test-key", "sk-ant-oat01", "tools available", "MCP needs auth"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("the log contains %q", leak)
		}
	}
	lg := r.logs(t)
	if len(lg) != 2 || lg[0]["route"] != "mimir" || lg[1]["route"] != "anthropic" {
		t.Fatalf("log %v", lg)
	}
	m := lg[0]
	if m["upstream_host"] != strings.TrimPrefix(r.mimir.URL, "http://") || m["folded"].(float64) != 2 || m["orphan_tool_results"].(float64) != 0 {
		t.Errorf("mimir record %v", m)
	}
	if dropped, _ := m["dropped_fields"].([]any); len(dropped) != 4 {
		t.Errorf("dropped_fields %v", m["dropped_fields"])
	}
}

// The prompt prefix Mimir sees must not change as a conversation grows (that is what keeps its prefix cache valid).
func TestFirstMessageStableAcrossAGrowingConversation(t *testing.T) {
	r := newRig(t, nil, nil)
	r.post(t, "/v1/messages", convo(false))
	r.post(t, "/v1/messages", convo(true))
	first := func(b []byte) string {
		var msgs []json.RawMessage
		_ = json.Unmarshal(topOf(t, b)["messages"], &msgs)
		return string(msgs[0]) + string(msgs[1]) + string(msgs[2]) + string(msgs[3])
	}
	r.mimir.mu.Lock()
	a, b := first(r.mimir.seen[0].body), first(r.mimir.seen[1].body)
	r.mimir.mu.Unlock()
	if a != b {
		t.Errorf("the shared prefix changed:\n%s\n%s", a, b)
	}
	lg := r.logs(t)
	if lg[0]["first_hash"] != lg[1]["first_hash"] {
		t.Errorf("first_hash %v vs %v", lg[0]["first_hash"], lg[1]["first_hash"])
	}
}

func TestParseConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := parseConfig([]string{"--mimir-url", "https://m/anthropic"}, env(nil)); err == nil || !strings.Contains(err.Error(), "MIMIR_API_KEY") {
		t.Errorf("a missing key must fail at start: %v", err)
	}
	cfg, err := parseConfig([]string{"--mimir-url", "https://m/anthropic/", "--port", "4015"}, env(map[string]string{"MIMIR_API_KEY": "k"}))
	if err != nil || cfg.mimirURL != "https://m/anthropic" || cfg.mimirKey != "k" || cfg.port != 4015 || cfg.prefix[0] != "cubbit/" {
		t.Errorf("%+v %v", cfg, err)
	}
	cfg, err = parseConfig([]string{"--litellm", "http://x:1/", "--litellm-key", "sk-test", "--prefix", "a/", "--prefix", "b/"}, env(nil))
	if err != nil || cfg.mimirURL != "" || cfg.litellm != "http://x:1" || len(cfg.prefix) != 2 {
		t.Errorf("litellm mode: %+v %v", cfg, err)
	}
	if _, err := parseConfig([]string{"--nope"}, env(nil)); err == nil {
		t.Errorf("unknown flag must fail")
	}
}

func sseHandler(events []string, gap time.Duration, tailWait time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, e := range events {
			_, _ = w.Write([]byte(e))
			fl.Flush()
			time.Sleep(gap)
		}
		time.Sleep(tailWait)
	}
}

func readUntil(t *testing.T, r *rig, needle string) {
	t.Helper()
	resp, err := http.Post(r.srv.URL+"/v1/messages", "application/json", bytes.NewReader(convo(false)))
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	buf := make([]byte, 256)
	for !strings.Contains(string(got), needle) {
		n, err := resp.Body.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			break
		}
	}
	_ = resp.Body.Close() // leave while the upstream is still open
}

func TestClientLeavingAfterMessageStopIsNotAnError(t *testing.T) {
	ev := []string{"event: message_start\ndata: {}\n\n", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"}
	r := newRig(t, sseHandler(ev, 10*time.Millisecond, 600*time.Millisecond), nil)
	readUntil(t, r, "message_stop")
	time.Sleep(900 * time.Millisecond)
	if lg := r.logs(t); len(lg) != 1 || lg[0]["error"] != nil || lg[0]["status"].(float64) != 200 {
		t.Fatalf("log %v", lg)
	}
}

func TestClientLeavingMidStreamIsLogged(t *testing.T) {
	ev := []string{"event: message_start\ndata: {}\n\n", "event: content_block_delta\ndata: {}\n\n", "event: content_block_delta\ndata: {}\n\n"}
	r := newRig(t, sseHandler(ev, 200*time.Millisecond, 0), nil)
	readUntil(t, r, "message_start")
	time.Sleep(900 * time.Millisecond)
	if lg := r.logs(t); len(lg) != 1 || lg[0]["error"] != "client went away" {
		t.Fatalf("log %v", lg)
	}
}

func TestGatewayRepairsMimirStreamAndLogsIt(t *testing.T) {
	r := newRig(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for _, e := range strings.SplitAfter(mimirStream, "\n\n") {
			if e != "" {
				_, _ = w.Write([]byte(e))
				fl.Flush()
			}
		}
	}, nil)
	_, b := r.post(t, "/v1/messages", convo(false))
	if strings.Contains(string(b), "thinking_delta") || !strings.Contains(string(b), `"stop_reason":"tool_use"`) {
		t.Fatalf("stream not repaired:\n%s", b)
	}
	lg := r.logs(t)
	fx, _ := lg[0]["stream_fixes"].(map[string]any)
	if len(lg) != 1 || fx["thinking_dropped"].(float64) != 2 || fx["stop_reason_fixed"].(float64) != 1 || lg[0]["error"] != nil {
		t.Errorf("log %v", lg)
	}
}

func TestClaudeStreamIsNeverTouched(t *testing.T) {
	r := newRig(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(mimirStream)) // even a stream with these patterns passes untouched on the Claude leg
	})
	body := []byte(strings.Replace(string(convo(false)), "cubbit/mimir-pro", "claude-sonnet-5-5", 1))
	_, b := r.post(t, "/v1/messages", body)
	if string(b) != mimirStream {
		t.Errorf("a Claude stream was changed")
	}
}

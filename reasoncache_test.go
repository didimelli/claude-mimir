package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReasonCacheLRUAndTTL(t *testing.T) {
	c := newReasonCache(14, 10, time.Hour) // an entry costs len("m\x00"+id) + len(text)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	c.put("m", []string{"a", "b"}, "ab")
	c.put("m", []string{"c"}, "c")
	c.get("m", "a") // a is now the most recent; b the least
	c.put("m", []string{"d"}, "d")
	if _, ok := c.get("m", "b"); ok {
		t.Error("least recently used entry not evicted")
	}
	for _, id := range []string{"a", "c", "d"} {
		if _, ok := c.get("m", id); !ok {
			t.Errorf("%s evicted", id)
		}
	}
	if c.put("m", []string{"e"}, "longer than ten") || c.put("m", nil, "x") || c.put("m", []string{"f"}, "") {
		t.Error("oversized, unkeyed or empty reasoning stored")
	}
	now = now.Add(time.Hour + time.Second)
	if _, ok := c.get("m", "a"); ok || c.order.Len() != 2 || c.size != 8 {
		t.Errorf("expired entry served or kept: %d left, %d bytes", c.order.Len(), c.size)
	}
}

func TestReasonCacheByteBudget(t *testing.T) {
	c := newReasonCache(20, 10, time.Hour)
	c.put("m", []string{"a", "b", "c"}, "12345") // three ids share one string: 3 x 8 bytes, over 20
	if _, ok := c.get("m", "a"); ok || c.size != 16 {
		t.Fatalf("budget not kept: a still in, %d bytes", c.size)
	}
	c.put("m", []string{"b"}, "x") // replacing an entry frees its old bytes
	if c.size != 12 || c.order.Len() != 2 {
		t.Fatalf("replaced entry miscounted: %d bytes, %d entries", c.size, c.order.Len())
	}
}

func TestReasonCacheConcurrent(t *testing.T) {
	c := newReasonCache(50*5, 100, time.Hour)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := fmt.Sprint((g*i)%97, "-", i%13)
				c.put("m", []string{id}, "r")
				c.get("m", id)
			}
		}(g)
	}
	wg.Wait()
	if c.size > c.budget || len(c.items) != c.order.Len() {
		t.Fatalf("size %d bytes, index %d, list %d", c.size, len(c.items), c.order.Len())
	}
}

const toolTurn1 = `{"model":"cubbit/mimir","messages":[{"role":"user","content":"task"}]}`

// toolTurn2 is turn 1 as Claude Code sends it back: no thinking, the tool call, its result.
func toolTurn2(model, id string) []byte {
	return []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"task"},
	 {"role":"assistant","content":[{"type":"tool_use","id":"` + id + `","name":"Read","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id + `","content":"A"}]}]}`)
}

func cacheRig(t *testing.T, stream bool) *rig {
	return newOpenAIRigWith(t, func(w http.ResponseWriter, _ *http.Request) {
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				`data: {"id":"c1","model":"m","choices":[{"delta":{"reasoning_content":"the key is "}}]}` + "\n\n" +
					`data: {"id":"c1","model":"m","choices":[{"delta":{"reasoning_content":"42"}}]}` + "\n\n" +
					`data: {"id":"c1","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{}"}}]}}]}` + "\n\n" +
					`data: {"id":"c1","model":"m","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c1","model":"m","choices":[{"finish_reason":"tool_calls","message":{"content":null,"reasoning":"the key is 42",
		 "tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}]}}]}`))
	}, func(c *config) {
		c.replay = map[string]map[string]any{"cubbit/mimir": {"preserve_thinking": true}, "cubbit/mimir-pro": {"clear_thinking": false}}
	})
}

func sentAssistant(t *testing.T, r *rig) map[string]any {
	t.Helper()
	var sent map[string]any
	if err := json.Unmarshal(r.mimir.last(t).body, &sent); err != nil {
		t.Fatal(err)
	}
	return sent["messages"].([]any)[1].(map[string]any)
}

func TestGatewayCachesReasoningAcrossTurns(t *testing.T) {
	for _, stream := range []bool{false, true} {
		r := cacheRig(t, stream)
		body := []byte(toolTurn1)
		if stream {
			body = []byte(strings.Replace(toolTurn1, `"messages"`, `"stream":true,"messages"`, 1))
		}
		if resp, b := r.post(t, "/v1/messages", body); resp.StatusCode != 200 || strings.Contains(string(b), "42") {
			t.Fatalf("stream=%v: %d, reasoning reached the client: %s", stream, resp.StatusCode, b)
		}
		r.post(t, "/v1/messages", toolTurn2("cubbit/mimir", "call_1"))
		if a := sentAssistant(t, r); a["reasoning_content"] != "the key is 42" {
			t.Errorf("stream=%v: not injected: %v", stream, a)
		}
		r.post(t, "/v1/messages", toolTurn2("cubbit/mimir", "call_other")) // a miss changes nothing
		if a := sentAssistant(t, r); a["reasoning_content"] != nil {
			t.Errorf("stream=%v: miss injected %v", stream, a)
		}
		r.post(t, "/v1/messages", toolTurn2("cubbit/mimir-pro", "call_1")) // same id, another model
		if a := sentAssistant(t, r); a["reasoning_content"] != nil {
			t.Errorf("stream=%v: other model's reasoning injected %v", stream, a)
		}
		lg := r.logs(t)
		if lg[0]["reasoning_cached"] != 1.0 || lg[1]["reasoning_injected"] != 1.0 || lg[2]["reasoning_injected"] != nil || lg[3]["reasoning_injected"] != nil {
			t.Errorf("stream=%v: log %v", stream, lg)
		}
	}
}

func TestGatewaySlimNeverCachesReasoning(t *testing.T) {
	r := cacheRig(t, false)
	r.post(t, "/v1/messages", []byte(strings.Replace(toolTurn1, "cubbit/mimir", "cubbit/mimir-slim", 1)))
	if r.g.reasoning.order.Len() != 0 || r.logs(t)[0]["reasoning_cached"] != nil {
		t.Fatal("slim reply cached")
	}
	r.g.reasoning.put("cubbit/mimir-slim", []string{"call_1"}, "the key is 42")
	r.post(t, "/v1/messages", toolTurn2("cubbit/mimir-slim", "call_1"))
	if a := sentAssistant(t, r); a["reasoning_content"] != nil {
		t.Errorf("slim injected %v", a)
	}
}

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
	c := newReasonCache(3, 10, time.Hour)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }
	c.put([]string{"a", "b"}, "ab")
	c.put([]string{"c"}, "c")
	c.get("a") // a is now the most recent; b the least
	c.put([]string{"d"}, "d")
	if _, ok := c.get("b"); ok {
		t.Error("least recently used entry not evicted")
	}
	for _, id := range []string{"a", "c", "d"} {
		if _, ok := c.get(id); !ok {
			t.Errorf("%s evicted", id)
		}
	}
	if c.put([]string{"e"}, "longer than ten") || c.put(nil, "x") || c.put([]string{"f"}, "") {
		t.Error("oversized, unkeyed or empty reasoning stored")
	}
	now = now.Add(time.Hour + time.Second)
	if _, ok := c.get("a"); ok || c.order.Len() != 2 {
		t.Errorf("expired entry served or kept: %d left", c.order.Len())
	}
}

func TestReasonCacheConcurrent(t *testing.T) {
	c := newReasonCache(50, 100, time.Hour)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				id := fmt.Sprint((g*i)%97, "-", i%13)
				c.put([]string{id}, "r")
				c.get(id)
			}
		}(g)
	}
	wg.Wait()
	if c.order.Len() > 50 || len(c.items) != c.order.Len() {
		t.Fatalf("size %d, index %d", c.order.Len(), len(c.items))
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
		c.replay = map[string]map[string]any{"cubbit/mimir": {"preserve_thinking": true}}
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
		lg := r.logs(t)
		if lg[0]["reasoning_cached"] != 1.0 || lg[1]["reasoning_injected"] != 1.0 || lg[2]["reasoning_injected"] != nil {
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
	r.g.reasoning.put([]string{"call_1"}, "the key is 42")
	r.post(t, "/v1/messages", toolTurn2("cubbit/mimir-slim", "call_1"))
	if a := sentAssistant(t, r); a["reasoning_content"] != nil {
		t.Errorf("slim injected %v", a)
	}
}

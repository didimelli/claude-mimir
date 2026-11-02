package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Fields Claude Code sends that the Mimir leg does not forward:
//   - metadata carries Anthropic account identifiers;
//   - context_management is a Claude-only feature;
//   - thinking and output_config are validated per Mimir model and differently for each (slim rejects effort high,
//     mimir rejects medium, adaptive thinking alone fails on slim), and the LiteLLM path dropped them too.
//     A model with a configured effort (see mimirEffort) gets both set explicitly instead: left to its default,
//     mimir-pro sometimes reasons until max_tokens and returns an empty turn.
var mimirDropFields = []string{"metadata", "context_management", "thinking", "output_config"}

type mimirInfo struct {
	foldStats
	Dropped []string
	Effort  string // reasoning effort sent, "" when none
}

var validEffort = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// shapeMimirBody makes a Claude Code request acceptable to Mimir's Anthropic-compatible endpoint. Only the fields
// named above and the system entries inside messages are touched; everything else is forwarded as received.
// A non-empty effort replaces thinking and output_config with adaptive thinking at that effort.
func shapeMimirBody(body []byte, effort string) ([]byte, mimirInfo, error) {
	var info mimirInfo
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, info, err
	}
	for _, k := range mimirDropFields {
		if _, ok := top[k]; ok {
			delete(top, k)
			info.Dropped = append(info.Dropped, k)
		}
	}
	if effort != "" {
		top["thinking"] = json.RawMessage(`{"type":"adaptive"}`)
		top["output_config"] = marshal(map[string]string{"effort": effort})
		info.Effort = effort
	}
	var raws []json.RawMessage
	if r, ok := top["messages"]; ok {
		if err := json.Unmarshal(r, &raws); err != nil {
			return nil, info, err
		}
	}
	msgs := make([]msg, len(raws))
	for i, r := range raws {
		if err := json.Unmarshal(r, &msgs[i]); err != nil {
			return nil, info, err
		}
	}
	folded, system, st := foldMessages(msgs, top["system"])
	info.foldStats = st
	if len(system) > 0 {
		top["system"] = system
	}
	top["messages"] = marshal(folded)
	return marshal(top), info, nil
}

// estimateTokens is a rough local token count, for /v1/messages/count_tokens, which Mimir does not support.
func estimateTokens(body []byte) int {
	if n := len(body) / 4; n > 1 {
		return n
	}
	return 1
}

var contextLimit = regexp.MustCompile(`(?s)maximum context length is (\d+) tokens.*?your prompt contains at least (\d+) input tokens`)

// rewriteMimirError maps Mimir's errors to the ones Claude Code acts on. It reports whether it changed anything.
//   - an empty 401 gets a message;
//   - an unknown model is a 500 "api_error" upstream, which Claude Code would retry: make it a 404;
//   - a context overflow becomes Anthropic's "prompt is too long: M tokens > N maximum", which Claude Code parses.
func rewriteMimirError(status int, body []byte) (int, []byte, bool) {
	mk := func(typ, msg string) []byte {
		return marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	}
	if status == 401 && strings.TrimSpace(string(body)) == "" {
		return 401, mk("authentication_error", "Mimir rejected the API key (HTTP 401): check MIMIR_API_KEY"), true
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return status, body, false
	}
	switch {
	case status == 500 && strings.Contains(e.Error.Message, "no keys found that support model"):
		return 404, mk("not_found_error", e.Error.Message), true
	case status == 400:
		if m := contextLimit.FindStringSubmatch(e.Error.Message); m != nil {
			limit, _ := strconv.Atoi(m[1])
			prompt, _ := strconv.Atoi(m[2])
			return 400, mk("invalid_request_error", fmt.Sprintf("prompt is too long: %d tokens > %d maximum", prompt, limit)), true
		}
	}
	return status, body, false
}

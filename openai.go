package main

// The Mimir leg in "openai" mode: Claude Code's Anthropic Messages requests are translated to Mimir's
// OpenAI-compatible /v1/chat/completions, and the replies back. Mimir's /anthropic endpoint lets mimir-pro reason
// until max_tokens on long contexts, ignores every reasoning control, and streams malformed events; /v1 bounds the
// reasoning, honors reasoning_effort, reports token usage in streams, and lets the gateway write a well-formed
// Anthropic stream itself.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// Reasoning efforts each API accepts. /anthropic validates them per model (slim rejects high and max, mimir rejects
// medium); /v1 accepts every value on every model.
var (
	anthropicEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}
	openaiEfforts    = map[string]bool{"none": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
)

type oaiInfo struct {
	foldStats
	Effort        string
	DroppedBlocks int // content blocks with no Chat Completions equivalent (thinking, documents, server tools, ...)
	Replayed      int // thinking blocks sent back as reasoning_content
	Injected      int // assistant turns given reasoning from the gateway's cache
	DroppedTools  int // server tools without an input schema
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    any           `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	// ReasoningContent is the turn's earlier reasoning, for chat templates that keep it (Qwen preserve_thinking,
	// GLM clear_thinking=false).
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type anthBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Source    *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
}

func blocksOfContent(c json.RawMessage) []anthBlock {
	var out []anthBlock
	for _, raw := range contentBlocks(c) {
		var b anthBlock
		if json.Unmarshal(raw, &b) == nil {
			out = append(out, b)
		}
	}
	return out
}

func imagePart(b anthBlock) (map[string]any, bool) {
	if b.Source == nil {
		return nil, false
	}
	url := b.Source.URL
	if b.Source.Type == "base64" {
		url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
	}
	if url == "" {
		return nil, false
	}
	return map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}}, true
}

// userContent is a plain string when the turn is text only (what every chat template handles), parts otherwise.
func userContent(texts []string, images []map[string]any) any {
	if len(images) == 0 {
		return strings.Join(texts, "\n\n")
	}
	parts := []any{}
	for _, t := range texts {
		parts = append(parts, map[string]string{"type": "text", "text": t})
	}
	for _, im := range images {
		parts = append(parts, im)
	}
	return parts
}

// toolResultText flattens a tool_result's content to the string a role:tool message carries; images are returned
// separately, to go into the user message that follows.
func toolResultText(b anthBlock) (string, []map[string]any) {
	var texts []string
	var images []map[string]any
	for _, c := range blocksOfContent(b.Content) {
		switch c.Type {
		case "text":
			texts = append(texts, c.Text)
		case "image":
			if p, ok := imagePart(c); ok {
				images = append(images, p)
			}
		}
	}
	s := strings.Join(texts, "\n\n")
	if b.IsError {
		s = "Error: " + s
	}
	return s, images
}

// toChatCompletions translates an Anthropic Messages request into a Chat Completions request. Mid-conversation
// system entries are folded first, exactly as on the /anthropic path (mimir-slim rejects them on /v1 too).
// A non-nil replay sends earlier assistant thinking back as reasoning_content and merges replay into
// chat_template_kwargs; nil drops thinking, as before. A turn without thinking whose tool_use ids are in cache gets
// the reasoning the gateway kept for them.
func toChatCompletions(body []byte, effort string, replay map[string]any, cache *reasonCache) ([]byte, oaiInfo, error) {
	var info oaiInfo
	var req struct {
		Model      string            `json:"model"`
		MaxTokens  *int              `json:"max_tokens"`
		Messages   []json.RawMessage `json:"messages"`
		System     json.RawMessage   `json:"system"`
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice *struct {
			Type                   string `json:"type"`
			Name                   string `json:"name"`
			DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
		} `json:"tool_choice"`
		Stream        bool            `json:"stream"`
		Temperature   json.RawMessage `json:"temperature"`
		TopP          json.RawMessage `json:"top_p"`
		StopSequences []string        `json:"stop_sequences"`
		Kwargs        map[string]any  `json:"chat_template_kwargs"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, info, err
	}
	msgs := make([]msg, len(req.Messages))
	for i, r := range req.Messages {
		if err := json.Unmarshal(r, &msgs[i]); err != nil {
			return nil, info, err
		}
	}
	folded, system, st := foldMessages(msgs, req.System)
	info.foldStats = st

	var out []oaiMessage
	if texts := textsOf(system); len(texts) > 0 {
		out = append(out, oaiMessage{Role: "system", Content: strings.Join(texts, "\n\n")})
	}
	for _, m := range folded {
		blocks := blocksOfContent(m["content"])
		switch roleOf(m) {
		case "assistant":
			am := oaiMessage{Role: "assistant"}
			var texts, thoughts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "thinking":
					if replay == nil {
						info.DroppedBlocks++
						continue
					}
					thoughts = append(thoughts, b.Thinking)
					info.Replayed++
				case "tool_use":
					tc := oaiToolCall{ID: b.ID, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = "{}"
					if len(bytes.TrimSpace(b.Input)) > 0 {
						tc.Function.Arguments = string(b.Input)
					}
					am.ToolCalls = append(am.ToolCalls, tc)
				default: // redacted_thinking, server tool blocks
					info.DroppedBlocks++
				}
			}
			am.ReasoningContent = strings.Join(thoughts, "\n")
			if replay != nil && len(thoughts) == 0 {
				for _, tc := range am.ToolCalls {
					if r, ok := cache.get(tc.ID); ok {
						am.ReasoningContent = r
						info.Injected++
						break
					}
				}
			}
			am.Content = strings.Join(texts, "\n\n")
			if len(texts) == 0 && len(am.ToolCalls) > 0 {
				am.Content = nil
			}
			out = append(out, am)
		default: // user
			var texts []string
			var images []map[string]any
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					s, ims := toolResultText(b)
					out = append(out, oaiMessage{Role: "tool", ToolCallID: b.ToolUseID, Content: s})
					images = append(images, ims...)
				case "text":
					texts = append(texts, b.Text)
				case "image":
					if p, ok := imagePart(b); ok {
						images = append(images, p)
					} else {
						info.DroppedBlocks++
					}
				default:
					info.DroppedBlocks++
				}
			}
			if len(texts) > 0 || len(images) > 0 {
				out = append(out, oaiMessage{Role: "user", Content: userContent(texts, images)})
			}
		}
	}

	creq := map[string]any{"model": req.Model, "messages": out, "stream": req.Stream}
	if req.Stream {
		creq["stream_options"] = map[string]bool{"include_usage": true}
	}
	if req.MaxTokens != nil {
		creq["max_completion_tokens"] = *req.MaxTokens
	}
	if len(req.Temperature) > 0 {
		creq["temperature"] = req.Temperature
	}
	if len(req.TopP) > 0 {
		creq["top_p"] = req.TopP
	}
	if len(req.StopSequences) > 0 {
		creq["stop"] = req.StopSequences
	}
	var tools []any
	for _, raw := range req.Tools {
		var t struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if json.Unmarshal(raw, &t) != nil || len(t.InputSchema) == 0 {
			info.DroppedTools++ // a server tool (web search, ...): nothing on /v1 can run it
			continue
		}
		fn := map[string]any{"name": t.Name, "parameters": t.InputSchema}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		creq["tools"] = tools
		if tc := req.ToolChoice; tc != nil {
			switch tc.Type {
			case "auto":
				creq["tool_choice"] = "auto"
			case "any":
				creq["tool_choice"] = "required"
			case "none":
				creq["tool_choice"] = "none"
			case "tool":
				creq["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}}
			}
			if tc.DisableParallelToolUse {
				creq["parallel_tool_calls"] = false
			}
		}
	}
	if effort != "" {
		creq["reasoning_effort"] = effort
		info.Effort = effort
	}
	if req.Kwargs == nil && replay != nil {
		req.Kwargs = map[string]any{}
	}
	for k, v := range replay {
		if _, set := req.Kwargs[k]; !set { // the client's own value wins
			req.Kwargs[k] = v
		}
	}
	if len(req.Kwargs) > 0 {
		creq["chat_template_kwargs"] = req.Kwargs
	}
	return marshal(creq), info, nil
}

func textsOf(system json.RawMessage) []string {
	var out []string
	for _, b := range blocksOfContent(system) {
		if b.Type == "text" && b.Text != "" {
			out = append(out, b.Text)
		}
	}
	return out
}

// ---- replies -------------------------------------------------------------------------------------------------

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func stopReason(finish string, sawTool bool) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	}
	if sawTool {
		return "tool_use"
	}
	return "end_turn"
}

func toolInput(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage(`{}`)
	}
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(args)
}

// fromChatCompletion translates a non-streamed Chat Completions reply into an Anthropic message. It records the usage
// in st, and the reasoning of a reply that called tools (for the reasoning cache).
func fromChatCompletion(body []byte, st *oaiStats) ([]byte, error) {
	var r struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          *string       `json:"content"`
				Reasoning        *string       `json:"reasoning"`
				ReasoningContent *string       `json:"reasoning_content"`
				ToolCalls        []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage oaiUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if len(r.Choices) == 0 {
		return nil, errors.New("reply has no choices")
	}
	c := r.Choices[0]
	st.Usage, st.HaveUsage = r.Usage, true
	if c.FinishReason != "length" {
		for _, tc := range c.Message.ToolCalls {
			st.ThoughtIDs = append(st.ThoughtIDs, tc.ID)
		}
		if len(st.ThoughtIDs) > 0 {
			st.Thought = firstNonEmpty(c.Message.Reasoning, c.Message.ReasoningContent)
		}
	}
	content := []any{}
	if c.Message.Content != nil && *c.Message.Content != "" {
		content = append(content, map[string]string{"type": "text", "text": *c.Message.Content})
	}
	for _, tc := range c.Message.ToolCalls {
		content = append(content, map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": toolInput(tc.Function.Arguments)})
	}
	msg := map[string]any{
		"id": "msg_" + r.ID, "type": "message", "role": "assistant", "model": r.Model, "content": content,
		"stop_reason": stopReason(c.FinishReason, len(c.Message.ToolCalls) > 0), "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": r.Usage.PromptTokens, "output_tokens": r.Usage.CompletionTokens},
	}
	return marshal(msg), nil
}

// oaiStats is what the stream translator reports to the log line.
type oaiStats struct {
	Usage           oaiUsage
	HaveUsage       bool
	ReasoningChunks int
	Measured        bool       // a streamed reply: Text and Reasoning are meaningful
	Text            lineWindow // the visible reply, measured (runaway.go)
	Reasoning       lineWindow // Mimir's reasoning, never forwarded, measured the same way
	ToolCalls       int
	LateFragments   int // argument fragments for a tool call already closed (dropped)
	Truncated       bool
	StreamError     string   // an error Mimir sent inside the stream
	Thought         string   // the reasoning of a complete reply that called tools
	ThoughtIDs      []string // the ids of those tool calls
	Cached          bool     // Thought went into the reasoning cache
}

func firstNonEmpty(a, b *string) string {
	if a != nil && *a != "" {
		return *a
	}
	if b != nil {
		return *b
	}
	return ""
}

// runaway reports why a finished reply looks like a runaway, or "" when it does not: the visible text went past
// budget bytes, or one line filled runawayThreshold of the last runawayWindow lines of the text or the reasoning.
func (s *oaiStats) runaway(budget int) string {
	var why []string
	if budget > 0 && s.Text.Chars >= budget {
		why = append(why, "text_budget")
	}
	if s.Text.MaxRepeat >= runawayThreshold {
		why = append(why, "text_repeat")
	}
	if s.Reasoning.MaxRepeat >= runawayThreshold {
		why = append(why, "reasoning_repeat")
	}
	return strings.Join(why, ",")
}

func (s *oaiStats) counts() map[string]int {
	m := map[string]int{}
	if s.LateFragments > 0 {
		m["late_tool_fragment_dropped"] = s.LateFragments
	}
	if s.Truncated {
		m["stream_truncated"] = 1
	}
	return m
}

// chatStream turns a Chat Completions SSE stream into an Anthropic Messages SSE stream. Blocks are opened and
// closed strictly in order, so the output is well-formed whatever the order of the input chunks.
type chatStream struct {
	rc    io.ReadCloser
	br    *bufio.Reader
	stats *oaiStats
	out   bytes.Buffer
	done  bool
	err   error

	started  bool
	next     int    // index of the next content block
	open     int    // index of the open block, -1 if none
	openKind string // "text" or "tool"
	openTool int    // OpenAI tool_calls index of the open tool block
	closed   map[int]bool
	finish   string
	sawTool  bool

	onEnd   func()          // non-nil: collect reasoning and tool call ids, set stats.Thought, then call it at the end
	thought strings.Builder // collected reasoning, up to reasonMaxBytes+1
	ids     []string
}

func newChatStream(rc io.ReadCloser, stats *oaiStats) *chatStream {
	stats.Measured = true
	return &chatStream{rc: rc, br: bufio.NewReaderSize(rc, 64*1024), stats: stats, open: -1, closed: map[int]bool{}}
}

func (s *chatStream) emit(event string, data any) {
	s.out.WriteString("event: " + event + "\ndata: ")
	s.out.Write(marshal(data))
	s.out.WriteString("\n\n")
}

func (s *chatStream) start(id, model string) {
	if s.started {
		return
	}
	s.started = true
	s.emit("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + id, "type": "message", "role": "assistant", "model": model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})
}

func (s *chatStream) closeBlock() {
	if s.open < 0 {
		return
	}
	s.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.open})
	if s.openKind == "tool" {
		s.closed[s.openTool] = true
	}
	s.open = -1
}

func (s *chatStream) openBlock(kind string, block map[string]any) {
	s.closeBlock()
	s.open, s.openKind = s.next, kind
	s.next++
	s.emit("content_block_start", map[string]any{"type": "content_block_start", "index": s.open, "content_block": block})
}

type chunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Content          *string `json:"content"`
			Reasoning        *string `json:"reasoning"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      *string `json:"name"`
					Arguments *string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *oaiUsage       `json:"usage"`
	Error json.RawMessage `json:"error"`
}

func (s *chatStream) handle(data []byte) {
	var c chunk
	if json.Unmarshal(data, &c) != nil {
		return
	}
	if len(c.Error) > 0 && string(c.Error) != "null" {
		s.fail(errorText(c.Error))
		return
	}
	s.start(c.ID, c.Model)
	for _, ch := range c.Choices {
		d := ch.Delta
		if r := firstNonEmpty(d.Reasoning, d.ReasoningContent); r != "" {
			s.stats.ReasoningChunks++ // never shown: Mimir's reasoning has no signature Claude Code could replay
			s.stats.Reasoning.feed(r)
			if s.onEnd != nil && s.thought.Len() <= reasonMaxBytes {
				s.thought.WriteString(r)
			}
		}
		if d.Content != nil && *d.Content != "" {
			s.stats.Text.feed(*d.Content)
			if s.openKind != "text" || s.open < 0 {
				s.openBlock("text", map[string]any{"type": "text", "text": ""})
			}
			s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.open, "delta": map[string]string{"type": "text_delta", "text": *d.Content}})
		}
		for _, tc := range d.ToolCalls {
			if s.closed[tc.Index] {
				s.stats.LateFragments++
				continue
			}
			if s.openKind != "tool" || s.open < 0 || s.openTool != tc.Index {
				name := ""
				if tc.Function.Name != nil {
					name = *tc.Function.Name
				}
				id := tc.ID
				if id == "" {
					id = fmt.Sprintf("toolu_%s_%d", c.ID, tc.Index)
				}
				s.openBlock("tool", map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}})
				s.openTool = tc.Index
				s.ids = append(s.ids, id)
				s.sawTool = true
				s.stats.ToolCalls++
			}
			if tc.Function.Arguments != nil && *tc.Function.Arguments != "" {
				s.emit("content_block_delta", map[string]any{"type": "content_block_delta", "index": s.open, "delta": map[string]string{"type": "input_json_delta", "partial_json": *tc.Function.Arguments}})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finish = *ch.FinishReason
		}
	}
	if c.Usage != nil {
		s.stats.Usage, s.stats.HaveUsage = *c.Usage, true
	}
}

// end closes the message: the open block, then message_delta with the stop reason and usage, then message_stop.
func (s *chatStream) end() {
	s.stats.Text.flush()
	s.stats.Reasoning.flush()
	if !s.started {
		return
	}
	// before message_stop goes out, so the client's next request finds it
	if s.onEnd != nil && !s.stats.Truncated && s.finish != "length" && len(s.ids) > 0 && s.thought.Len() <= reasonMaxBytes {
		s.stats.Thought, s.stats.ThoughtIDs = s.thought.String(), s.ids
		s.onEnd()
	}
	s.closeBlock()
	stop := stopReason(s.finish, s.sawTool)
	if s.finish == "" && s.stats.Truncated {
		stop = "max_tokens"
	}
	u := s.stats.Usage
	s.emit("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]int{"input_tokens": u.PromptTokens, "output_tokens": u.CompletionTokens}})
	s.emit("message_stop", map[string]string{"type": "message_stop"})
	s.done = true
}

// fail ends the stream with an Anthropic error event and no message_stop, so the client sees a failed turn.
func (s *chatStream) fail(msg string) {
	s.stats.Text.flush()
	s.stats.Reasoning.flush()
	s.stats.StreamError = msg
	s.emit("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "Mimir: " + msg}})
	s.done = true
}

func (s *chatStream) Read(p []byte) (int, error) {
	for s.out.Len() == 0 {
		if s.done {
			if s.err != nil {
				return 0, s.err
			}
			return 0, io.EOF
		}
		line, err := s.br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); bytes.HasPrefix(t, []byte("data:")) {
			data := bytes.TrimSpace(t[5:])
			if bytes.Equal(data, []byte("[DONE]")) {
				s.end()
			} else {
				s.handle(data)
			}
		}
		if err != nil && !s.done {
			if err == io.EOF {
				// A clean end without [DONE]: close the message so the client gets a valid stream.
				if s.started && s.finish == "" {
					s.stats.Truncated = true
				}
				if s.started {
					s.end()
				} else {
					s.fail("the stream ended before any data")
				}
			} else {
				// An upstream failure is never turned into a clean end: pass the error on after what we have.
				s.err = err
				s.done = true
			}
		}
	}
	return s.out.Read(p)
}

func (s *chatStream) Close() error { return s.rc.Close() }

// ---- errors --------------------------------------------------------------------------------------------------

// errorText reads the message of an OpenAI-style error, including Mimir's nested {"error":{"error":"…","message":"…"}}.
func errorText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var e struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if len(e.Error) > 0 {
			return errorText(e.Error)
		}
	}
	return strings.TrimSpace(string(raw))
}

var (
	ctxRequested = regexp.MustCompile(`(?s)maximum context length is (\d+) tokens.*?(?:prompt contains at least|you requested) (\d+)`)
)

func anthropicErrorType(status int) string {
	switch status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	}
	return "api_error"
}

// translateOpenAIError turns an error reply from /v1 into the Anthropic error Claude Code acts on, applying the same
// rewrites as the /anthropic path (unknown model -> 404, context overflow -> "prompt is too long", empty 401).
func translateOpenAIError(status int, body []byte) (int, []byte) {
	mk := func(st int, typ, msg string) (int, []byte) {
		return st, marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": msg}})
	}
	if status == 401 && strings.TrimSpace(string(body)) == "" {
		return mk(401, "authentication_error", "Mimir rejected the API key (HTTP 401): check MIMIR_API_KEY")
	}
	var top struct {
		Error json.RawMessage `json:"error"`
	}
	msg := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &top) == nil && len(top.Error) > 0 {
		msg = errorText(top.Error)
	}
	if strings.Contains(msg, "no keys found that support model") {
		return mk(404, "not_found_error", msg)
	}
	if status == 400 {
		if m := ctxRequested.FindStringSubmatch(msg); m != nil {
			limit, _ := strconv.Atoi(m[1])
			prompt, _ := strconv.Atoi(m[2])
			return mk(400, "invalid_request_error", fmt.Sprintf("prompt is too long: %d tokens > %d maximum", prompt, limit))
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("Mimir returned HTTP %d", status)
	}
	return mk(status, anthropicErrorType(status), msg)
}

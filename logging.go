package main

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
)

const logMax = 20 << 20

// reqLogger appends one metadata-only JSON line per proxied /v1/messages request.
// It never writes message text or credentials, and never fails a request.
type reqLogger struct {
	mu   sync.Mutex
	path string
}

func (l *reqLogger) write(rec map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, err := os.Stat(l.path); err == nil && st.Size() > logMax {
		_ = os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
}

// requestShape is the part of a /v1/messages body the router reads: never forwarded, only inspected.
type requestShape struct {
	Model             string            `json:"model"`
	Messages          []json.RawMessage `json:"messages"`
	ContextManagement json.RawMessage   `json:"context_management"`
}

type rawMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type blockHdr struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
}

// decodeBody undoes a request Content-Encoding so the body can be inspected.
func decodeBody(body []byte, enc string) []byte {
	var r io.Reader
	var err error
	switch strings.ToLower(strings.TrimSpace(enc)) {
	case "gzip":
		r, err = gzip.NewReader(bytes.NewReader(body))
	case "deflate":
		r, err = zlib.NewReader(bytes.NewReader(body))
	default:
		return body
	}
	if err != nil {
		return nil
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil
	}
	return out
}

// sniff parses a JSON object body; nil when the body is not one.
func sniff(body []byte, enc string) *requestShape {
	raw := decodeBody(body, enc)
	if raw == nil {
		return nil
	}
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return nil
	}
	var s requestShape
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil
	}
	return &s
}

func blocksOf(content json.RawMessage) []blockHdr {
	c := bytes.TrimSpace(content)
	if len(c) == 0 || c[0] != '[' {
		return nil
	}
	var bl []blockHdr
	_ = json.Unmarshal(c, &bl)
	return bl
}

// facts are integrity facts about a message list. An orphan is a tool_result whose tool_use is in no
// earlier message, which is what Anthropic rejects with a 400.
func facts(s *requestShape) map[string]any {
	seen := map[string]bool{}
	toolUse, toolResult, orphans, compaction := 0, 0, 0, 0
	var firstRole any
	firstIsTR, firstHash := false, any(nil)
	for i, mr := range s.Messages {
		var m rawMsg
		_ = json.Unmarshal(mr, &m)
		for _, b := range blocksOf(m.Content) {
			switch b.Type {
			case "tool_use":
				toolUse++
				seen[b.ID] = true
			case "tool_result":
				toolResult++
				if !seen[b.ToolUseID] {
					orphans++
				}
				if i == 0 {
					firstIsTR = true
				}
			case "compaction":
				compaction++
			}
		}
		if i == 0 {
			firstRole = m.Role
			sum := sha1.Sum(mr)
			firstHash = hex.EncodeToString(sum[:])[:8]
		}
	}
	return map[string]any{
		"n_messages": len(s.Messages), "first_role": firstRole, "first_is_tool_result": firstIsTR, "first_hash": firstHash,
		"tool_use": toolUse, "tool_result": toolResult, "orphan_tool_results": orphans, "compaction_blocks": compaction,
		"has_context_management": len(bytes.TrimSpace(s.ContextManagement)) > 0,
	}
}

// errorMessage is the message of an Anthropic-style error body, for the log (error text only).
func errorMessage(payload []byte, enc string) string {
	raw := decodeBody(payload, enc)
	if raw == nil {
		return ""
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message any    `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) != nil || (e.Error.Type == "" && e.Error.Message == nil) {
		return ""
	}
	msg := ""
	if e.Error.Message != nil {
		msg = strings.TrimSpace(strings.Trim(jsonString(e.Error.Message), "\""))
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return e.Error.Type + ": " + msg
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

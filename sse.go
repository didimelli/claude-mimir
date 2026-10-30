package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// Mimir's Anthropic-compatible stream differs from Anthropic's in two ways that make Claude Code abandon the stream
// and retry the turn without streaming (so every turn is paid for twice):
//   - reasoning arrives as thinking_delta events inside a block declared as "text";
//   - message_delta says stop_reason "end_turn" even when the message contains a tool_use block.
// sseFixer repairs both on the way through. Every other event is forwarded byte for byte.

type fixStats struct{ ThinkingDropped, StopFixed int }

type sseState struct {
	blockType map[int]string
	sawTool   bool
}

type sseData struct {
	Type         string `json:"type"`
	Index        *int   `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
	} `json:"content_block"`
	Delta *struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
}

// fixEvent returns the event to forward, or nil to drop it. ev is one complete event including its blank-line end.
func fixEvent(ev []byte, st *sseState, stats *fixStats) []byte {
	var dataLine []byte
	for _, line := range bytes.Split(ev, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			dataLine = bytes.TrimRight(line, "\r")
			break
		}
	}
	if dataLine == nil {
		return ev
	}
	raw := bytes.TrimSpace(dataLine[len("data:"):])
	var d sseData
	if json.Unmarshal(raw, &d) != nil {
		return ev
	}
	switch d.Type {
	case "content_block_start":
		if d.Index != nil && d.ContentBlock != nil {
			if st.blockType == nil {
				st.blockType = map[int]string{}
			}
			st.blockType[*d.Index] = d.ContentBlock.Type
			if d.ContentBlock.Type == "tool_use" {
				st.sawTool = true
			}
		}
	case "content_block_delta":
		if d.Index != nil && d.Delta != nil && (d.Delta.Type == "thinking_delta" || d.Delta.Type == "signature_delta") && st.blockType[*d.Index] != "thinking" {
			stats.ThinkingDropped++
			return nil
		}
	case "message_delta":
		if st.sawTool && d.Delta != nil && d.Delta.StopReason == "end_turn" {
			var top map[string]json.RawMessage
			var delta map[string]json.RawMessage
			if json.Unmarshal(raw, &top) == nil && json.Unmarshal(top["delta"], &delta) == nil {
				delta["stop_reason"] = json.RawMessage(`"tool_use"`)
				top["delta"] = marshal(delta)
				stats.StopFixed++
				var out bytes.Buffer
				for _, line := range bytes.Split(bytes.TrimRight(ev, "\r\n"), []byte("\n")) {
					if bytes.HasPrefix(line, []byte("data:")) {
						out.WriteString("data: ")
						out.Write(marshal(top))
					} else {
						out.Write(line)
					}
					out.WriteString("\n")
				}
				out.WriteString("\n")
				return out.Bytes()
			}
		}
	}
	return ev
}

// sseFixer is an io.ReadCloser that serves the repaired stream as soon as each event is complete.
type sseFixer struct {
	rc    io.ReadCloser
	stats *fixStats
	st    sseState
	in    []byte
	out   []byte
	err   error
}

func eventEnd(b []byte) int {
	i, j := bytes.Index(b, []byte("\n\n")), bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case i >= 0 && (j < 0 || i < j):
		return i + 2
	case j >= 0:
		return j + 4
	}
	return -1
}

func (f *sseFixer) consume() {
	for {
		end := eventEnd(f.in)
		if end < 0 {
			return
		}
		ev := f.in[:end]
		if fixed := fixEvent(ev, &f.st, f.stats); fixed != nil {
			f.out = append(f.out, fixed...)
		}
		f.in = f.in[end:]
	}
}

func (f *sseFixer) Read(p []byte) (int, error) {
	for len(f.out) == 0 {
		if f.err != nil {
			return 0, f.err
		}
		buf := make([]byte, 32<<10)
		n, err := f.rc.Read(buf)
		f.in = append(f.in, buf[:n]...)
		f.consume()
		if err != nil {
			f.err = err
			f.out = append(f.out, f.in...) // a trailing partial event is forwarded as it is
			f.in = nil
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

func (f *sseFixer) Close() error { return f.rc.Close() }

func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")
}

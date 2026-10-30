package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Mimir's Anthropic-compatible stream differs from Anthropic's in ways that make Claude Code abandon the stream and
// retry the turn without streaming (so every turn is paid for twice), or call it malformed and lose a tool call:
//   - reasoning arrives as thinking_delta events inside a block declared as "text";
//   - message_delta says stop_reason "end_turn" even when the message contains a tool_use block;
//   - with parallel tool calls the blocks are interleaved and the closing events are wrong: content_block_stop is
//     sent twice for one index and never for the other (about 1 turn in 100 with a tool-heavy agent);
//   - with parallel tool calls it also keeps sending deltas (newlines) for a block it already closed;
//   - when it runs out of output tokens it just stops: no message_delta, no message_stop.
// sseFixer repairs these on the way through. Every other event is forwarded byte for byte.

type fixStats struct {
	ThinkingDropped, StopFixed, BlockStopFixed, BlockStopAdded, BlockStopDropped, LateDeltaDropped, Truncated int
}

// counts are the non-zero repairs, for the log
func (s *fixStats) counts() map[string]int {
	out := map[string]int{}
	for k, v := range map[string]int{"thinking_dropped": s.ThinkingDropped, "stop_reason_fixed": s.StopFixed, "block_stop_fixed": s.BlockStopFixed, "block_stop_added": s.BlockStopAdded, "block_stop_dropped": s.BlockStopDropped, "late_delta_dropped": s.LateDeltaDropped, "stream_truncated": s.Truncated} {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

type sseState struct {
	blockType map[int]string
	open      map[int]bool
	sawTool   bool
	started   bool // message_start seen: this is a message stream
	sawDelta  bool
	sawStop   bool
}

func (st *sseState) lowestOpen() (int, bool) {
	low, ok := 0, false
	for i := range st.open {
		if !ok || i < low {
			low, ok = i, true
		}
	}
	return low, ok
}

func stopEvent(i int) []byte {
	return []byte(fmt.Sprintf("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":%d}\n\n", i))
}

// closeOpen returns stop events for every block still open, lowest index first.
func (st *sseState) closeOpen(stats *fixStats) []byte {
	var out []byte
	for len(st.open) > 0 {
		i, _ := st.lowestOpen()
		delete(st.open, i)
		stats.BlockStopAdded++
		out = append(out, stopEvent(i)...)
	}
	return out
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
	case "message_start":
		st.started = true
	case "content_block_start":
		if d.Index != nil && d.ContentBlock != nil {
			if st.blockType == nil {
				st.blockType = map[int]string{}
			}
			st.blockType[*d.Index] = d.ContentBlock.Type
			if st.open == nil {
				st.open = map[int]bool{}
			}
			st.open[*d.Index] = true
			if d.ContentBlock.Type == "tool_use" {
				st.sawTool = true
			}
		}
	case "content_block_stop":
		if d.Index == nil {
			return ev
		}
		if st.open[*d.Index] {
			delete(st.open, *d.Index)
			return ev
		}
		// a stop for a block that is not open: Mimir's parallel tool calls close one index twice and the other never.
		if j, ok := st.lowestOpen(); ok {
			delete(st.open, j)
			stats.BlockStopFixed++
			return stopEvent(j)
		}
		stats.BlockStopDropped++
		return nil
	case "content_block_delta":
		if d.Index != nil && !st.open[*d.Index] {
			stats.LateDeltaDropped++ // a delta for a block that is closed (or was never started)
			return nil
		}
		if d.Index != nil && d.Delta != nil && (d.Delta.Type == "thinking_delta" || d.Delta.Type == "signature_delta") && st.blockType[*d.Index] != "thinking" {
			stats.ThinkingDropped++
			return nil
		}
	case "message_stop":
		st.sawStop = true
		if closing := st.closeOpen(stats); len(closing) > 0 {
			return append(closing, ev...)
		}
	case "message_delta":
		st.sawDelta = true
		if closing := st.closeOpen(stats); len(closing) > 0 {
			rest := fixMessageDelta(ev, raw, &d, st, stats)
			return append(closing, rest...)
		}
		return fixMessageDelta(ev, raw, &d, st, stats)
	}
	return ev
}

// fixMessageDelta corrects stop_reason end_turn to tool_use when a tool call was streamed.
func fixMessageDelta(ev, raw []byte, d *sseData, st *sseState, stats *fixStats) []byte {
	if !st.sawTool || d.Delta == nil || d.Delta.StopReason != "end_turn" {
		return ev
	}
	var top, delta map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || json.Unmarshal(top["delta"], &delta) != nil {
		return ev
	}
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
			if err == io.EOF && f.st.started && !f.st.sawStop {
				f.endOfStream()
			} else {
				f.out = append(f.out, f.in...) // a trailing partial event is forwarded as it is
			}
			f.in = nil
		}
	}
	n := copy(p, f.out)
	f.out = f.out[n:]
	return n, nil
}

// endOfStream finishes a stream that ended cleanly without message_stop (Mimir does this when it runs out of output
// tokens): close the open blocks and send the missing message_delta and message_stop. A partial last event is
// unusable and dropped. A broken connection is not a clean end and is never finished this way.
func (f *sseFixer) endOfStream() {
	out := f.st.closeOpen(f.stats)
	if !f.st.sawDelta {
		out = append(out, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\",\"stop_sequence\":null}}\n\n"...)
	}
	out = append(out, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"...)
	f.stats.Truncated++
	f.out = append(f.out, out...)
}

func (f *sseFixer) Close() error { return f.rc.Close() }

func isEventStream(contentType string) bool {
	return strings.HasPrefix(strings.ToLower(contentType), "text/event-stream")
}

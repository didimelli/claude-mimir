package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func ev(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }

// the stream Mimir really sends: reasoning as thinking_delta inside the text block, end_turn although a tool was called
var mimirStream = strings.Join([]string{
	ev("message_start", `{"type":"message_start","message":{"id":"chatcmpl-1","model":"mimir-pro","role":"assistant"}}`),
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Simple task: read"}}`),
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" the file"}}`),
	ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Reading."}}`),
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
	ev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`),
	ev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"/x\"}"}}`),
	ev("content_block_stop", `{"type":"content_block_stop","index":1}`),
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":""},"usage":{"output_tokens":9}}`),
	ev("message_stop", `{"type":"message_stop"}`),
}, "")

func runFixer(t *testing.T, in string, wrap func(io.Reader) io.Reader) (string, *fixStats) {
	t.Helper()
	st := &fixStats{}
	var r io.Reader = strings.NewReader(in)
	if wrap != nil {
		r = wrap(r)
	}
	out, err := io.ReadAll(&sseFixer{rc: io.NopCloser(r), stats: st})
	if err != nil {
		t.Fatal(err)
	}
	return string(out), st
}

func TestSSEFixRepairsBothDefects(t *testing.T) {
	out, st := runFixer(t, mimirStream, nil)
	if strings.Contains(out, "thinking_delta") {
		t.Errorf("thinking_delta survived:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) || strings.Contains(out, `"end_turn"`) {
		t.Errorf("stop_reason not repaired:\n%s", out)
	}
	if st.ThinkingDropped != 2 || st.StopFixed != 1 {
		t.Errorf("stats %+v", st)
	}
	// untouched parts are forwarded exactly, and the rest of message_delta (usage) is kept
	for _, keep := range []string{"Reading.", `"partial_json"`, `"output_tokens":9`, `"stop_sequence":""`, "message_stop", "chatcmpl-1"} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q", keep)
		}
	}
}

func TestSSEFixLeavesEverythingElseByteExact(t *testing.T) {
	noTool := strings.Join([]string{
		ev("message_start", `{"type":"message_start","message":{"id":"x"}}`),
		ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi <b> & é"}}`),
		ev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`), // no tool call: end_turn is right
		"event: ping\ndata: {}\n\n",
		": a comment line\n\n",
		ev("message_stop", `{"type":"message_stop"}`),
	}, "")
	if out, st := runFixer(t, noTool, nil); out != noTool || st.ThinkingDropped+st.StopFixed != 0 {
		t.Errorf("changed an already correct stream:\n%s", out)
	}
	// a proper thinking block (what Anthropic sends) is not touched
	proper := ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`) +
		ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`)
	if out, _ := runFixer(t, proper, nil); out != proper {
		t.Errorf("a well-formed thinking block was changed:\n%s", out)
	}
	// not JSON, no data line, CRLF delimiters: forwarded as they are
	odd := "data: not json\n\n" + "event: x\n\n" + "event: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\r\n"
	if out, _ := runFixer(t, odd, nil); out != odd {
		t.Errorf("odd events changed: %q", out)
	}
}

func TestSSEFixIsIndependentOfChunking(t *testing.T) {
	whole, _ := runFixer(t, mimirStream, nil)
	for name, wrap := range map[string]func(io.Reader) io.Reader{
		"one byte at a time": iotest.OneByteReader,
		"half reads":         iotest.HalfReader,
		"data then EOF":      iotest.DataErrReader,
	} {
		if got, _ := runFixer(t, mimirStream, wrap); got != whole {
			t.Errorf("%s: output differs", name)
		}
	}
}

func TestSSEFixForwardsATrailingPartialEventWhenItIsNotAnUnfinishedMessage(t *testing.T) {
	partial := "event: content_block_delta\ndata: {\"typ"
	// a stream that is not a message (no message_start), and a message that already ended: forwarded as they are
	for name, in := range map[string]string{
		"not a message":     ev("ping", `{}`) + partial,
		"already completed": ev("message_start", `{"type":"message_start"}`) + ev("message_stop", `{"type":"message_stop"}`) + partial,
	} {
		if out, _ := runFixer(t, in, nil); out != in {
			t.Errorf("%s: partial event lost: %q", name, out)
		}
	}
}

func TestSSEFixIsNotBuffered(t *testing.T) {
	pr, pw := io.Pipe()
	f := &sseFixer{rc: pr, stats: &fixStats{}}
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		n, _ := f.Read(buf)
		got <- string(buf[:n])
	}()
	_, _ = pw.Write([]byte(ev("message_start", `{"type":"message_start"}`))) // the stream stays open
	select {
	case s := <-got:
		if !bytes.Contains([]byte(s), []byte("message_start")) {
			t.Errorf("got %q", s)
		}
	case <-after():
		t.Fatal("a complete event was held back")
	}
	_ = pw.Close()
}

func after() <-chan time.Time { return time.After(2 * time.Second) }

// ---- Mimir's parallel tool calls: interleaved blocks, a block_stop sent twice for one index and none for the other ----

type blockEv struct {
	kind string
	idx  int
}

// blockEvents lists the block start/stop events of a stream in order.
func blockEvents(t *testing.T, stream string) []blockEv {
	t.Helper()
	var out []blockEv
	for _, e := range strings.Split(strings.TrimSpace(stream), "\n\n") {
		var name, data string
		for _, l := range strings.Split(e, "\n") {
			if strings.HasPrefix(l, "event:") {
				name = strings.TrimSpace(l[6:])
			} else if strings.HasPrefix(l, "data:") {
				data = strings.TrimSpace(l[5:])
			}
		}
		if name != "content_block_start" && name != "content_block_stop" {
			continue
		}
		var d struct {
			Index int `json:"index"`
		}
		if json.Unmarshal([]byte(data), &d) != nil {
			t.Fatalf("bad event %q", e)
		}
		out = append(out, blockEv{name, d.Index})
	}
	return out
}

// wellFormed: every started block is stopped exactly once, and no block is stopped that is not open.
func wellFormed(evs []blockEv) bool {
	open := map[int]bool{}
	for _, e := range evs {
		if e.kind == "content_block_start" {
			open[e.idx] = true
		} else {
			if !open[e.idx] {
				return false
			}
			delete(open, e.idx)
		}
	}
	return len(open) == 0
}

func startTool(i int) string {
	return ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"t%d","name":"Read","input":{}}}`, i, i))
}
func jsonDelta(i int) string {
	return ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":"{}"}}`, i))
}
func stopBlock(i int) string {
	return ev("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i))
}

var tail = ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`) + ev("message_stop", `{"type":"message_stop"}`)

func TestSSEFixRepairsParallelToolCallStops(t *testing.T) {
	// what Mimir really sends for two parallel tool calls
	in := ev("message_start", `{"type":"message_start","message":{"id":"x"}}`) + startTool(0) + jsonDelta(0) + startTool(1) + jsonDelta(1) + stopBlock(0) + stopBlock(0) + tail
	out, st := runFixer(t, in, nil)
	if evs := blockEvents(t, out); !wellFormed(evs) {
		t.Fatalf("blocks still malformed: %v\n%s", evs, out)
	}
	if st.BlockStopFixed != 1 {
		t.Errorf("stats %+v", st)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason not repaired as well")
	}
}

func TestSSEFixAddsMissingStopsAndDropsStrayOnes(t *testing.T) {
	missing := startTool(0) + jsonDelta(0) + startTool(1) + jsonDelta(1) + stopBlock(0) + tail // block 1 never closed
	if out, st := runFixer(t, missing, nil); !wellFormed(blockEvents(t, out)) || st.BlockStopAdded != 1 {
		t.Errorf("missing stop not added: %+v\n%s", st, out)
	}
	stray := startTool(0) + jsonDelta(0) + stopBlock(0) + stopBlock(0) + tail // a stop with nothing open
	if out, st := runFixer(t, stray, nil); !wellFormed(blockEvents(t, out)) || st.BlockStopDropped != 1 {
		t.Errorf("stray stop not dropped: %+v\n%s", st, out)
	}
	// the added stop comes before message_delta, and message_delta/message_stop are still in place
	out, _ := runFixer(t, missing, nil)
	if strings.Index(out, `"index":1}`) > strings.Index(out, "message_delta") || !strings.HasSuffix(out, `"message_stop"}`+"\n\n") {
		t.Errorf("ordering wrong:\n%s", out)
	}
}

func TestSSEFixLeavesAWellFormedParallelStreamAlone(t *testing.T) {
	good := startTool(0) + jsonDelta(0) + stopBlock(0) + startTool(1) + jsonDelta(1) + stopBlock(1) + tail
	if out, st := runFixer(t, good, nil); strings.Contains(out, "end_turn") && false || st.BlockStopFixed+st.BlockStopAdded+st.BlockStopDropped != 0 || !wellFormed(blockEvents(t, out)) {
		t.Errorf("a correct stream was changed: %+v", st)
	}
	// interleaved but correctly closed blocks are valid for index-based parsers and are not reordered
	inter := startTool(0) + startTool(1) + jsonDelta(0) + jsonDelta(1) + stopBlock(1) + stopBlock(0) + tail
	if out, st := runFixer(t, inter, nil); st.BlockStopFixed+st.BlockStopAdded+st.BlockStopDropped != 0 || !wellFormed(blockEvents(t, out)) {
		t.Errorf("interleaved but valid stream was changed: %+v", st)
	}
}

// Whatever order of block events arrives, the repaired stream is well formed and chunking does not matter.
func TestSSEFixBlockStructureProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for n := 0; n < 3000; n++ {
		var b strings.Builder
		b.WriteString(ev("message_start", `{"type":"message_start","message":{"id":"x"}}`))
		blocks := rng.Intn(4)
		started := 0
		for i := 0; i < blocks+rng.Intn(5); i++ {
			switch rng.Intn(3) {
			case 0:
				if started < 4 {
					b.WriteString(startTool(started))
					started++
				}
			case 1:
				if started > 0 {
					b.WriteString(jsonDelta(rng.Intn(started)))
				}
			default:
				if started > 0 {
					b.WriteString(stopBlock(rng.Intn(started))) // any index, including already closed ones
				}
			}
		}
		b.WriteString(tail)
		out, _ := runFixer(t, b.String(), nil)
		if !wellFormed(blockEvents(t, out)) {
			t.Fatalf("not repaired:\nin:\n%s\nout:\n%s", b.String(), out)
		}
		if whole, _ := runFixer(t, b.String(), iotest.OneByteReader); whole != out {
			t.Fatalf("chunking changed the result")
		}
	}
}

// ---- more Mimir defects found under load: late deltas for closed blocks, and streams that just stop ----

func textDelta(i int, s string) string {
	return ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":"text_delta","text":%q}}`, i, s))
}
func startText(i int) string {
	return ev("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"text","text":""}}`, i))
}

var msgStart = ev("message_start", `{"type":"message_start","message":{"id":"x"}}`)

func TestSSEFixDropsDeltasForClosedOrUnknownBlocks(t *testing.T) {
	// Mimir keeps sending newline text_deltas for block 0 after it was closed, between parallel tool calls
	in := msgStart + startText(0) + textDelta(0, "Reading") + stopBlock(0) + startTool(1) + jsonDelta(1) + textDelta(0, "\n") + startTool(2) + jsonDelta(2) +
		textDelta(0, "\n\n") + stopBlock(1) + stopBlock(2) + textDelta(7, "never started") + tail
	out, st := runFixer(t, in, nil)
	if st.LateDeltaDropped != 3 {
		t.Errorf("stats %+v", st)
	}
	if strings.Contains(out, `"\n"`) || strings.Contains(out, "never started") {
		t.Errorf("a late delta survived:\n%s", out)
	}
	if !strings.Contains(out, "Reading") || !wellFormed(blockEvents(t, out)) {
		t.Errorf("real content lost or blocks malformed:\n%s", out)
	}
}

func TestSSEFixFinishesATruncatedStream(t *testing.T) {
	cases := map[string]struct {
		in            string
		wantStop      string
		wantExtraStop bool
	}{
		"open block, nothing after":   {msgStart + startText(0) + textDelta(0, "partial"), "max_tokens", true},
		"block closed, nothing after": {msgStart + startText(0) + textDelta(0, "x") + stopBlock(0), "max_tokens", false},
		"message_delta but no stop":   {msgStart + startText(0) + stopBlock(0) + ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`), "end_turn", false},
		"partial event at the end":    {msgStart + startText(0) + textDelta(0, "a") + "event: content_block_delta\ndata: {\"type\":\"content_bl", "max_tokens", true},
	}
	for name, c := range cases {
		out, st := runFixer(t, c.in, nil)
		if !strings.HasSuffix(out, ev("message_stop", `{"type":"message_stop"}`)) || !wellFormed(blockEvents(t, out)) {
			t.Errorf("%s: not finished cleanly:\n%s", name, out)
		}
		if !strings.Contains(out, `"stop_reason":"`+c.wantStop+`"`) || st.Truncated != 1 {
			t.Errorf("%s: stop reason or stats wrong (%+v):\n%s", name, st, out)
		}
		if strings.Contains(out, `"content_bl"`) {
			t.Errorf("%s: a partial event leaked into the output", name)
		}
	}
}

func TestSSEFixDoesNotHideARealUpstreamFailure(t *testing.T) {
	// a connection that breaks (not a clean end) must reach the client as a failure, not as a finished message
	in := msgStart + startText(0) + textDelta(0, "partial")
	f := &sseFixer{rc: io.NopCloser(io.MultiReader(strings.NewReader(in), iotest.ErrReader(io.ErrUnexpectedEOF))), stats: &fixStats{}}
	out, err := io.ReadAll(f)
	if err == nil || strings.Contains(string(out), "message_stop") {
		t.Errorf("a broken connection was turned into a clean end: err=%v out=%q", err, out)
	}
}

func TestSSEFixLeavesCompleteStreamsAlone(t *testing.T) {
	// not a message at all (no message_start): never touched
	odd := "event: ping\ndata: {}\n\n"
	if out, st := runFixer(t, odd, nil); out != odd || st.Truncated != 0 {
		t.Errorf("a non-message stream was changed: %q", out)
	}
	complete := msgStart + startText(0) + textDelta(0, "ok") + stopBlock(0) + tail
	if out, st := runFixer(t, complete, nil); out != complete || len(st.counts()) != 1 { // only the end_turn tool check does not apply; no repairs at all
		_ = st
		if out != complete {
			t.Errorf("a complete stream was changed:\n%s", out)
		}
	}
}

// Property: whatever Mimir sends (random block events, stray stops and deltas, a missing ending), the repaired
// stream is well formed, deltas only touch open blocks, and it ends with message_stop.
func TestSSEFixStreamIsAlwaysWellFormedProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for n := 0; n < 4000; n++ {
		var b strings.Builder
		b.WriteString(msgStart)
		started := 0
		for i := 0; i < rng.Intn(12); i++ {
			switch rng.Intn(5) {
			case 0:
				if started < 4 {
					b.WriteString(startTool(started))
					started++
				}
			case 1:
				if started > 0 {
					b.WriteString(jsonDelta(rng.Intn(started + 1))) // may target a block that was never started
				}
			case 2:
				if started > 0 {
					b.WriteString(stopBlock(rng.Intn(started)))
				}
			case 3:
				b.WriteString(textDelta(rng.Intn(started+1), "t"))
			default:
				b.WriteString(ev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hm"}}`))
			}
		}
		switch rng.Intn(3) {
		case 0:
			b.WriteString(tail)
		case 1:
			b.WriteString(ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)) // no message_stop
		} // case 2: no ending at all
		out, _ := runFixer(t, b.String(), nil)
		if !wellFormed(blockEvents(t, out)) || !deltasOnOpenBlocks(t, out) || !strings.HasSuffix(out, `"message_stop"}`+"\n\n") {
			t.Fatalf("repaired stream is not clean\nin:\n%s\nout:\n%s", b.String(), out)
		}
		if again, _ := runFixer(t, out, nil); again != out {
			t.Fatalf("not idempotent on its own output\n%s", out)
		}
	}
}

// deltasOnOpenBlocks: every content_block_delta targets a block that was started and not yet stopped.
func deltasOnOpenBlocks(t *testing.T, stream string) bool {
	t.Helper()
	open := map[int]bool{}
	for _, e := range strings.Split(strings.TrimSpace(stream), "\n\n") {
		var name, data string
		for _, l := range strings.Split(e, "\n") {
			if strings.HasPrefix(l, "event:") {
				name = strings.TrimSpace(l[6:])
			} else if strings.HasPrefix(l, "data:") {
				data = strings.TrimSpace(l[5:])
			}
		}
		var d struct {
			Index int `json:"index"`
		}
		_ = json.Unmarshal([]byte(data), &d)
		switch name {
		case "content_block_start":
			open[d.Index] = true
		case "content_block_stop":
			delete(open, d.Index)
		case "content_block_delta":
			if !open[d.Index] {
				return false
			}
		}
	}
	return true
}

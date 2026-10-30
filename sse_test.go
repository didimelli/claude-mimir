package main

import (
	"bytes"
	"io"
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

func TestSSEFixForwardsATrailingPartialEvent(t *testing.T) {
	in := ev("message_start", `{"type":"message_start"}`) + "event: content_block_delta\ndata: {\"typ"
	if out, _ := runFixer(t, in, nil); out != in {
		t.Errorf("partial event lost: %q", out)
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

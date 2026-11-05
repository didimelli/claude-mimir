package main

import (
	"math/rand"
	"strings"
	"testing"
)

// chunked feeds text in pieces of random size, as a token stream would deliver it.
func chunked(w *lineWindow, text string, seed int64) {
	r := rand.New(rand.NewSource(seed))
	for len(text) > 0 {
		n := 1 + r.Intn(7)
		if n > len(text) {
			n = len(text)
		}
		w.feed(text[:n])
		text = text[n:]
	}
	w.flush()
}

func measure(text string) lineWindow {
	var w lineWindow
	w.feed(text)
	w.flush()
	return w
}

func TestLineWindowRepeatedLine(t *testing.T) {
	if w := measure(strings.Repeat("---\n", 40)); w.MaxRepeat != 40 || w.Chars != 160 {
		t.Fatalf("repeat %d chars %d", w.MaxRepeat, w.Chars)
	}
}

// The loop seen in practice interleaved separator lines with a little filler; counting identical lines in a row
// would never get past 8.
func TestLineWindowInterleavedLoop(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 20; i++ {
		b.WriteString(strings.Repeat("---\n\n", 8))
		b.WriteString("GO!!!\n\n")
	}
	if w := measure(b.String()); w.MaxRepeat < runawayThreshold {
		t.Fatalf("interleaved loop reached only %d", w.MaxRepeat)
	}
}

func TestLineWindowNormalTextStaysLow(t *testing.T) {
	code := `func (s *Set) Retire(id ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	for _, e := range s.entries {
		if e.id == id {
			if e.state == stateSealed {
				return nil
			}
			for i := range e.parts {
				if e.parts[i].open {
					return ErrBusy
				}
			}
		}
	}
	return ErrNotInSet
}
`
	if w := measure(strings.Repeat(code, 6)); w.MaxRepeat >= runawayThreshold {
		t.Fatalf("code reached %d", w.MaxRepeat)
	}
	table := "| a | b |\n|---|---|\n" + strings.Repeat("| 1 | 2 |\n", 12)
	if w := measure(table); w.MaxRepeat >= runawayThreshold {
		t.Fatalf("table reached %d", w.MaxRepeat)
	}
}

func TestLineWindowIndependentOfChunking(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString("  same line  \r\n")
		if i%9 == 0 {
			b.WriteString("other " + strings.Repeat("é", i) + "\n")
		}
	}
	b.WriteString("no trailing newline")
	want := measure(b.String())
	for seed := int64(0); seed < 100; seed++ {
		var w lineWindow
		chunked(&w, b.String(), seed)
		if w.Chars != want.Chars || w.MaxRepeat != want.MaxRepeat {
			t.Fatalf("seed %d: chars %d/%d repeat %d/%d", seed, w.Chars, want.Chars, w.MaxRepeat, want.MaxRepeat)
		}
	}
	if want.MaxRepeat != 50 {
		t.Fatalf("CRLF and surrounding spaces must not split a line: %d", want.MaxRepeat)
	}
}

func TestLineWindowIsABoundedWindow(t *testing.T) {
	// 100 identical lines, then 100 different ones: the peak is capped by the window, and the old lines age out.
	var b strings.Builder
	b.WriteString(strings.Repeat("x\n", 100))
	for i := 0; i < 100; i++ {
		b.WriteString(strings.Repeat("y", i+1) + "\n")
	}
	w := measure(b.String())
	if w.MaxRepeat != runawayWindow {
		t.Fatalf("peak %d, want the window size %d", w.MaxRepeat, runawayWindow)
	}
	if got := w.counts["x"]; got != 0 {
		t.Fatalf("x still counted %d times after it left the window", got)
	}
	if len(w.counts) > runawayWindow {
		t.Fatalf("%d distinct lines kept", len(w.counts))
	}
}

func TestLineWindowKeepsLongLinesBounded(t *testing.T) {
	long := strings.Repeat("é", 5000) // multi-byte on purpose: the cap must not split a character
	w := measure(long + "\n" + long + "\n")
	if w.Chars != 2*(len(long)+1) {
		t.Fatalf("chars %d", w.Chars)
	}
	if w.MaxRepeat != 2 {
		t.Fatalf("two identical long lines counted %d times", w.MaxRepeat)
	}
	if w.partial.Len() > runawayLineCap {
		t.Fatalf("kept %d bytes of a line", w.partial.Len())
	}
}

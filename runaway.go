package main

import (
	"strings"
	"unicode/utf8"
)

// Runaway telemetry. Mimir sometimes emits a reply that never ends: tens of thousands of tokens of text instead of a
// tool call, either one line repeated over and over or long, unproductive deliberation. This file only measures it,
// as numbers and without keeping any text, so the request log can show how often it happens and in which stream
// (visible text or reasoning, which is never forwarded).
const (
	runawayWindow    = 60  // non-empty lines compared with each other
	runawayThreshold = 30  // identical lines inside the window that mark a loop
	runawayLineCap   = 200 // bytes of a line kept for comparison
)

// lineWindow watches text that arrives in token-sized pieces. It counts the bytes seen and the highest number of
// times one identical line appeared inside the last runawayWindow non-empty lines (a loop that interleaves a few
// filler lines still shows up, a plain "same line N times in a row" counter would miss it). It keeps no text beyond
// that window.
type lineWindow struct {
	Chars     int // bytes seen
	MaxRepeat int // peak count of one line inside the window

	partial strings.Builder // the unfinished line, capped at runawayLineCap bytes
	ring    [runawayWindow]string
	lines   int
	counts  map[string]int
}

func (w *lineWindow) feed(s string) {
	w.Chars += len(s)
	for {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			w.extend(s)
			return
		}
		w.extend(s[:i])
		w.endLine()
		s = s[i+1:]
	}
}

func (w *lineWindow) extend(s string) {
	room := runawayLineCap - w.partial.Len()
	if room <= 0 || s == "" {
		return
	}
	if len(s) > room {
		cut := room
		for cut > 0 && !utf8.RuneStart(s[cut]) { // never keep half of a multi-byte character
			cut--
		}
		s = s[:cut]
	}
	w.partial.WriteString(s)
}

func (w *lineWindow) endLine() {
	line := strings.TrimSpace(w.partial.String())
	w.partial.Reset()
	if line == "" {
		return
	}
	if w.counts == nil {
		w.counts = map[string]int{}
	}
	slot := w.lines % runawayWindow
	if w.lines >= runawayWindow {
		old := w.ring[slot]
		if w.counts[old]--; w.counts[old] == 0 {
			delete(w.counts, old)
		}
	}
	w.ring[slot] = line
	w.lines++
	w.counts[line]++
	if c := w.counts[line]; c > w.MaxRepeat {
		w.MaxRepeat = c
	}
}

// flush ends a trailing line that had no newline.
func (w *lineWindow) flush() { w.endLine() }

package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// ---- building blocks for tests ----

func tb(t string) json.RawMessage { return textBlock(t) }
func mk(role string, blocks ...json.RawMessage) msg {
	return msg{"role": marshal(role), "content": marshal(blocks)}
}
func mkStr(role, s string) msg { return msg{"role": marshal(role), "content": marshal(s)} }
func toolUse(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"tool_use","id":%q,"name":"Read","input":{"p":"x"}}`, id))
}
func toolResult(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"tool_result","tool_use_id":%q,"content":"ok"}`, id))
}
func toolAddition() json.RawMessage {
	return json.RawMessage(`{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"x"}}}`)
}

func roles(ms []msg) string {
	var r []string
	for _, m := range ms {
		r = append(r, roleOf(m)[:1])
	}
	return strings.Join(r, "")
}

func textsIn(m msg) []string {
	var out []string
	for _, b := range contentBlocks(m["content"]) {
		var t struct{ Type, Text string }
		_ = json.Unmarshal(b, &t)
		if t.Type == "text" {
			out = append(out, t.Text)
		}
	}
	return out
}

// ---- table tests ----

func TestFoldInPlace(t *testing.T) {
	// the shape of a real Mimir subagent request: u, s, a(tool_use), u(tool_result), s(string), a(tool_use), u(tool_result)
	in := []msg{
		mk("user", tb("task")),
		mk("system", tb("tools just became available"), toolAddition(), toolAddition()),
		mk("assistant", toolUse("t1")),
		mk("user", toolResult("t1")),
		mkStr("system", "MCP servers need auth"),
		mk("assistant", toolUse("t2")),
		mk("user", toolResult("t2")),
	}
	out, sys, st := foldMessages(in, nil)
	if got, want := roles(out), "uuauuau"; got != want {
		t.Fatalf("roles %s, want %s", got, want)
	}
	if len(sys) != 0 {
		t.Fatalf("no top-level system expected, got %s", sys)
	}
	if st.Folded != 2 || st.DroppedBlocks != 2 {
		t.Fatalf("stats %+v", st)
	}
	if got := textsIn(out[1]); len(got) != 1 || got[0] != "<system-reminder>\ntools just became available\n</system-reminder>" {
		t.Fatalf("folded text %q", got)
	}
	// the first message is never changed: the prefix stays stable
	if !reflect.DeepEqual(out[0], in[0]) {
		t.Fatalf("first message changed")
	}
	// no tool_addition block survives anywhere
	if strings.Contains(string(marshal(out)), "tool_addition") {
		t.Fatalf("tool_addition survived")
	}
}

func TestFoldWedgedEntryGoesAfterToolResult(t *testing.T) {
	in := []msg{
		mk("user", tb("task")),
		mk("assistant", toolUse("t1")),
		mk("system", tb("reminder")),
		mk("user", toolResult("t1")),
	}
	out, _, _ := foldMessages(in, nil)
	if roles(out) != "uau" {
		t.Fatalf("roles %s", roles(out))
	}
	blocks := contentBlocks(out[2]["content"])
	if len(blocks) != 2 || !strings.Contains(string(blocks[0]), "tool_result") || !strings.Contains(string(blocks[1]), "reminder") {
		t.Fatalf("tool_result must come first, then the reminder: %s", out[2]["content"])
	}
}

func TestFoldLeadingEntriesGoToSystem(t *testing.T) {
	in := []msg{mkStr("system", "lead one"), mk("system", tb("lead two")), mk("user", tb("hi"))}
	out, sys, st := foldMessages(in, marshal("top"))
	if roles(out) != "u" || st.Hoisted != 2 {
		t.Fatalf("roles %s stats %+v", roles(out), st)
	}
	var blocks []struct{ Type, Text string }
	_ = json.Unmarshal(sys, &blocks)
	if len(blocks) != 3 || blocks[0].Text != "top" || blocks[1].Text != "lead one" || blocks[2].Text != "lead two" {
		t.Fatalf("system %s", sys)
	}
}

func TestFoldSkipsEmptyAndKeepsOrderInARun(t *testing.T) {
	in := []msg{mk("user", tb("a")), mk("system", tb("   ")), mkStr("system", ""), mkStr("system", "one"), mkStr("system", "two"), mk("assistant", tb("b"))}
	out, _, st := foldMessages(in, nil)
	if roles(out) != "uuua" {
		t.Fatalf("roles %s", roles(out))
	}
	if st.Skipped != 2 || st.Folded != 2 {
		t.Fatalf("stats %+v", st)
	}
	if !strings.Contains(textsIn(out[1])[0], "one") || !strings.Contains(textsIn(out[2])[0], "two") {
		t.Fatalf("order lost")
	}
}

func TestFoldDoesNotDoubleWrap(t *testing.T) {
	in := []msg{mk("user", tb("a")), mkStr("system", "<system-reminder>\nalready\n</system-reminder>")}
	out, _, _ := foldMessages(in, nil)
	if got := textsIn(out[1])[0]; strings.Count(got, "<system-reminder>") != 1 {
		t.Fatalf("double wrapped: %q", got)
	}
}

func TestFoldTrailingEntryAfterAssistantToolUse(t *testing.T) {
	// a conversation can end right after a tool_use plus a system entry; the entry must not be lost
	in := []msg{mk("user", tb("a")), mk("assistant", toolUse("t1")), mkStr("system", "late")}
	out, _, st := foldMessages(in, nil)
	if roles(out) != "uau" || st.Folded != 1 {
		t.Fatalf("roles %s stats %+v", roles(out), st)
	}
}

// ---- property tests ----

// genMessages builds a conversation from bytes: each byte picks the next message.
func genMessages(data []byte) []msg {
	var out []msg
	n := 0
	var lastIDs []string
	for _, b := range data {
		switch b % 9 {
		case 0:
			out = append(out, mk("user", tb(fmt.Sprintf("u%d", n))))
		case 1:
			out = append(out, mkStr("user", fmt.Sprintf("us%d", n)))
		case 2:
			out = append(out, mk("assistant", tb(fmt.Sprintf("a%d", n))))
		case 3:
			id := fmt.Sprintf("t%d", n)
			lastIDs = []string{id}
			out = append(out, mk("assistant", tb("calling"), toolUse(id)))
		case 4:
			var bl []json.RawMessage
			for _, id := range lastIDs {
				bl = append(bl, toolResult(id))
			}
			bl = append(bl, tb(fmt.Sprintf("after%d", n)))
			out = append(out, mk("user", bl...))
			lastIDs = nil
		case 5:
			out = append(out, mk("system", tb(fmt.Sprintf("⟦s%d⟧", n))))
		case 6:
			out = append(out, mkStr("system", fmt.Sprintf("⟦ss%d⟧", n)))
		case 7:
			out = append(out, mk("system", tb(fmt.Sprintf("⟦sa%d⟧", n)), toolAddition(), tb(fmt.Sprintf("⟦sb%d⟧", n))))
		case 8:
			out = append(out, mk("system", tb("  ")))
		}
		n++
	}
	return out
}

func systemTextsIn(in []msg) []string {
	var all []string
	for _, m := range in {
		if roleOf(m) == "system" {
			ts, _, _ := systemTexts(m["content"])
			all = append(all, ts...)
		}
	}
	return all
}

// pairingHolds: every assistant tool_use is directly followed by a user turn that starts with its tool_results.
func pairingHolds(ms []msg) bool {
	for i, m := range ms {
		if roleOf(m) != "assistant" || !hasToolUse(m) {
			continue
		}
		if i+1 >= len(ms) || roleOf(ms[i+1]) != "user" {
			return false
		}
		bl := contentBlocks(ms[i+1]["content"])
		if len(bl) == 0 || !strings.Contains(string(bl[0]), `"tool_result"`) {
			return false
		}
	}
	return true
}

func nonSystem(in []msg) []msg {
	var out []msg
	for _, m := range in {
		if roleOf(m) != "system" {
			out = append(out, m)
		}
	}
	return out
}

func checkFold(t *testing.T, in []msg) {
	t.Helper()
	before := string(marshal(in))
	out, sys, _ := foldMessages(in, nil)
	if string(marshal(in)) != before {
		t.Fatalf("input was mutated")
	}
	for _, m := range out {
		if roleOf(m) == "system" {
			t.Fatalf("a system entry survived: %s", marshal(out))
		}
	}
	// every system entry's text appears in the output once per entry (in a reminder or in the top-level system),
	// on top of however often the same text already occurs in the non-system messages
	flat := string(marshal(out)) + string(sys)
	nsFlat := string(marshal(nonSystem(in)))
	needle := func(s string) string { b := marshal(s); return string(b[1 : len(b)-1]) } // as it appears inside JSON
	mult := map[string]int{}
	for _, s := range systemTextsIn(in) {
		mult[s]++
	}
	for s, c := range mult {
		if got, want := strings.Count(flat, needle(s)), c+strings.Count(nsFlat, needle(s)); got != want {
			t.Fatalf("system text %q appears %d times, want %d", s, got, want)
		}
	}
	// the non-system messages keep their order and their own blocks (a wedged entry may be appended after them);
	// every message in between is an extra user turn made from a system entry
	ns := nonSystem(in)
	j := 0
	for _, m := range ns {
		found := false
		for ; j < len(out); j++ {
			if extends(m, out[j]) {
				found = true
				j++
				break
			}
			if roleOf(out[j]) != "user" {
				break
			}
		}
		if !found {
			t.Fatalf("a non-system message was lost or reordered\nin=%s\nout=%s", marshal(in), marshal(out))
		}
	}
	// tool pairing: if it held before, it holds after, tool_result first
	if pairingHolds(ns) && !pairingHolds(out) {
		t.Fatalf("tool pairing broken\nin=%s\nout=%s", marshal(in), marshal(out))
	}
	// idempotent
	again, _, _ := foldMessages(out, nil)
	if string(marshal(again)) != string(marshal(out)) {
		t.Fatalf("not idempotent")
	}
}

// extends: o has m's role and starts with all of m's blocks.
func extends(m, o msg) bool {
	if roleOf(m) != roleOf(o) {
		return false
	}
	mb, ob := contentBlocks(m["content"]), contentBlocks(o["content"])
	if len(ob) < len(mb) {
		return false
	}
	for i := range mb {
		if string(marshal(mb[i])) != string(marshal(ob[i])) {
			return false
		}
	}
	return true
}

// prefix stability: folding a growing conversation never rewrites what was already sent, except while a system
// entry is still wedged after an assistant tool_use whose result has not arrived yet.
func checkPrefixStable(t *testing.T, in []msg) {
	t.Helper()
	full, _, _ := foldMessages(in, nil)
	for k := 1; k < len(in); k++ {
		pre := in[:k]
		if wedgedAtEnd(pre) {
			continue
		}
		part, _, _ := foldMessages(pre, nil)
		for i := range part {
			if i >= len(full) || string(marshal(part[i])) != string(marshal(full[i])) {
				t.Fatalf("prefix of %d messages is not a prefix of the full fold at %d\nin=%s", k, i, marshal(in))
			}
		}
	}
}

func wedgedAtEnd(pre []msg) bool {
	i := len(pre)
	for i > 0 && roleOf(pre[i-1]) == "system" {
		i--
	}
	return i < len(pre) && i > 0 && roleOf(pre[i-1]) == "assistant" && hasToolUse(pre[i-1])
}

func TestFoldProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 3000; i++ {
		data := make([]byte, rng.Intn(14))
		for j := range data {
			data[j] = byte(rng.Intn(256))
		}
		in := genMessages(data)
		checkFold(t, in)
		checkPrefixStable(t, in)
	}
}

func FuzzFold(f *testing.F) {
	f.Add([]byte{0, 5, 3, 4, 6, 3, 4})
	f.Add([]byte{5, 5, 0, 7, 3, 5, 4})
	f.Add([]byte{0, 3, 5, 4, 5, 2})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 40 {
			data = data[:40]
		}
		in := genMessages(data)
		checkFold(t, in)
		checkPrefixStable(t, in)
	})
}

// FOLD_REAL_BODIES=<dir of *.body files> runs the same checks on real captured Claude Code requests (never committed).
func TestFoldOnRealBodies(t *testing.T) {
	dir := os.Getenv("FOLD_REAL_BODIES")
	if dir == "" {
		t.Skip("FOLD_REAL_BODIES not set")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.body"))
	n := 0
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		var top map[string]json.RawMessage
		if json.Unmarshal(raw, &top) != nil || top["messages"] == nil {
			continue
		}
		var raws []msg
		if json.Unmarshal(top["messages"], &raws) != nil {
			continue
		}
		checkFold(t, raws)
		checkPrefixStable(t, raws)
		n++
	}
	t.Logf("checked %d real bodies", n)
}

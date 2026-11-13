package main

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// recorder stands in for PushHack: it records each call and answers from a
// function (nil reply = {}).
type recorder struct {
	mu     sync.Mutex
	got    []map[string]any
	answer func(cmd string, args map[string]any) (any, string)
}

func (r *recorder) call(ns, cmd string, args map[string]any, cb func(json.RawMessage, string)) {
	m := map[string]any{"ns": ns, "cmd": cmd}
	for k, v := range args {
		m[k] = v
	}
	r.mu.Lock()
	r.got = append(r.got, m)
	r.mu.Unlock()
	var reply any = map[string]any{}
	code := ""
	if r.answer != nil {
		if a, c := r.answer(cmd, args); a != nil || c != "" {
			reply, code = a, c
		}
	}
	b, _ := json.Marshal(reply)
	go cb(b, code)
}

func (r *recorder) calls() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.got...)
}

func editSet() *Set {
	return &Set{BeatsPerBar: 4, Tempo: 120, Length: 32, Tracks: []Track{
		{Name: "Grp", Kind: "group", GroupID: -1, ID: 0},
		{Name: "Bass", Kind: "midi", GroupID: 0, ID: 1, Clips: []Clip{{Start: 0, End: 8, Name: "B1"}, {Start: 16, End: 20, Name: "B2"}}},
		{Name: "Vox", Kind: "audio", GroupID: -1, ID: 2, Clips: []Clip{{Start: 4, End: 12, Name: "V1"}}},
	}}
}

type editRig struct {
	e     *editCtl
	st    *store
	rec   *recorder
	toast func() string
}

func newEditRig(t *testing.T) *editRig {
	t.Helper()
	st := &store{}
	st.putSet(editSet())
	vc := newViewCtl(func() *Set { s, _, _ := st.get(); return s })
	rec := &recorder{}
	cc := newContentCache(rec.call, func() {})
	shift := false
	e := newEditCtl(st, vc, cc, func() {}, func() bool { return shift })
	e.call = rec.call
	e.refresh = func(done func()) { done() }
	t.Cleanup(func() { reservedBottom.Store(0) })
	return &editRig{e: e, st: st, rec: rec, toast: func() string {
		vc.mu.Lock()
		defer vc.mu.Unlock()
		return vc.toast
	}}
}

// edits: the clip_* and transport calls (not clips_info, which the content cache makes).
func (r *editRig) edits() []map[string]any {
	var out []map[string]any
	for _, c := range r.rec.calls() {
		if c["cmd"] != "clips_info" {
			out = append(out, c)
		}
	}
	return out
}

func (r *editRig) waitEdits(t *testing.T, n int) []map[string]any {
	t.Helper()
	waitFor(t, func() bool { return len(r.edits()) >= n })
	time.Sleep(10 * time.Millisecond)
	return r.edits()
}

func TestEditModeSelectsTheClipUnderTheCursor(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(5)
	r.e.toggle()
	if !r.e.isOn() || reservedBottom.Load() != softStripH {
		t.Fatal("edit mode did not turn on")
	}
	s, _, _ := r.st.get()
	c, box, ok := r.e.selected(s)
	if !ok || box.track != 1 || c.Name != "B1" {
		t.Fatalf("selected %+v %+v %v", c, box, ok)
	}
	r.e.moveTrack(+1)
	if _, box, _ := r.e.selected(s); box.track != 2 {
		t.Fatalf("down: track %d", box.track)
	}
	r.e.moveTrack(-1)
	r.e.moveTrack(-1) // the group track is skipped; stays on Bass
	if r.e.selTrack() != 1 {
		t.Fatalf("up past the group: %d", r.e.selTrack())
	}
	r.e.toggle()
	if r.e.isOn() || reservedBottom.Load() != 0 {
		t.Fatal("edit mode did not turn off")
	}
}

func TestEditCursorTurnsOnAtThePlayhead(t *testing.T) {
	r := newEditRig(t)
	r.st.putPos(17, false, false, false, 0, 4, false, false)
	r.e.toggle()
	if c, ok := r.st.getCursor(); !ok || c != 17 {
		t.Fatalf("cursor %v %v", c, ok)
	}
}

func TestEditMoveClipJumpsBetweenClips(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(2)
	r.e.toggle()
	r.e.moveClip(+1)
	if c, _ := r.st.getCursor(); c != 16 {
		t.Fatalf("next: %v", c)
	}
	r.e.moveClip(+1) // no clip after: stays
	if c, _ := r.st.getCursor(); c != 16 {
		t.Fatalf("past the last: %v", c)
	}
	r.e.moveClip(-1)
	if c, _ := r.st.getCursor(); c != 0 {
		t.Fatalf("prev: %v", c)
	}
}

func TestEncoderEditShowsAGhostThenSendsOnRelease(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(1)
	r.e.toggle()
	r.rec.answer = func(cmd string, a map[string]any) (any, string) {
		if cmd == "clip_move" {
			return map[string]any{"clip": map[string]any{"track": 1, "start": 4, "end": 12}}, ""
		}
		return nil, ""
	}
	// encoder 1 (position): friction 2 ticks per step
	for i := 0; i < 4; i++ {
		r.e.turn(encPosition, 1)
	}
	s, _, _ := r.st.get()
	vp := viewport{}
	r.e.decorate(s, &vp)
	if vp.ghost == nil || vp.sel == nil || vp.ghost.start <= vp.sel.start {
		t.Fatalf("ghost %+v sel %+v", vp.ghost, vp.sel)
	}
	if n := len(r.edits()); n != 0 {
		t.Fatalf("sent before release: %d", n)
	}
	r.e.released(encPosition)
	got := r.waitEdits(t, 1)
	if got[0]["cmd"] != "clip_move" || got[0]["track"] != 1 || got[0]["start"] != 0.0 {
		t.Fatalf("%v", got[0])
	}
	if to := got[0]["to"].(float64); to <= 0 {
		t.Fatalf("to %v", to)
	}
	waitFor(t, func() bool { c, _ := r.st.getCursor(); return c == 4 })
}

func TestEncoderEditGoesOutAfterARest(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(1)
	r.e.toggle()
	for i := 0; i < 3; i++ {
		r.e.turn(encTranspose, 1)
	}
	got := r.waitEdits(t, 1)
	if got[0]["cmd"] != "clip_transpose" || got[0]["semitones"] != 1 {
		t.Fatalf("%v", got[0])
	}
}

func TestTrimAndMoveGoOutInOrderOnTheNewClip(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(1)
	r.e.toggle()
	r.rec.answer = func(cmd string, a map[string]any) (any, string) {
		if cmd == "clip_trim" {
			return map[string]any{"clip": map[string]any{"track": 1, "start": 1, "end": 8}}, ""
		}
		return nil, ""
	}
	r.e.turn(encStart, 2) // head +1 step
	r.e.turn(encPosition, 2)
	r.e.commitNow()
	got := r.waitEdits(t, 2)
	if got[0]["cmd"] != "clip_trim" || got[1]["cmd"] != "clip_move" {
		t.Fatalf("%v", got)
	}
	if got[1]["start"] != 1.0 {
		t.Fatalf("move did not use the trimmed clip: %v", got[1])
	}
}

func TestTrimCannotTurnAClipInsideOut(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(17)
	r.e.toggle()
	for i := 0; i < 100; i++ {
		r.e.turn(encStart, 2)
	}
	r.e.mu.Lock()
	g := r.e.pend.ghost()
	r.e.mu.Unlock()
	if g.end-g.start < editMinLen-1e-9 {
		t.Fatalf("ghost %+v", g)
	}
}

func TestSplitNeedsTheCursorInsideTheClip(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(0)
	r.e.toggle()
	r.e.action("split")
	waitFor(t, func() bool { return r.toast() == "PUT THE CURSOR INSIDE THE CLIP" })
	r.st.setCursor(3)
	r.e.action("split")
	got := r.waitEdits(t, 1)
	if got[0]["cmd"] != "clip_split" || got[0]["at"] != 3.0 || got[0]["start"] != 0.0 {
		t.Fatalf("%v", got[0])
	}
}

func TestActionsSendTheRightCommands(t *testing.T) {
	cases := []struct {
		action, cmd, key string
		want             any
	}{
		{"duplicate", "clip_duplicate", "", nil},
		{"delete", "clip_delete", "", nil},
		{"mute", "clip_set", "muted", true},
		{"quantize", "clip_quantize", "grid", quantizeGrid},
		{"crop", "clip_crop", "", nil},
	}
	for _, c := range cases {
		r := newEditRig(t)
		r.st.setCursor(2)
		r.e.toggle()
		r.e.action(c.action)
		got := r.waitEdits(t, 1)
		if got[0]["cmd"] != c.cmd || got[0]["track"] != 1 {
			t.Errorf("%s: %v", c.action, got[0])
		}
		if c.key != "" && got[0][c.key] != c.want {
			t.Errorf("%s: %s = %v", c.action, c.key, got[0][c.key])
		}
	}
}

func TestCopyThenPasteOnAnotherTrack(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(2)
	r.e.toggle()
	r.e.action("paste")
	waitFor(t, func() bool { return r.toast() == "NOTHING COPIED" })
	r.e.action("copy")
	waitFor(t, func() bool { return r.toast() == "COPIED B1" })
	r.e.moveTrack(+1)
	r.st.setCursor(20)
	r.e.action("paste")
	got := r.waitEdits(t, 1)
	c := got[0]
	if c["cmd"] != "clip_duplicate" || c["track"] != 1 || c["start"] != 0.0 || c["to_track"] != 2 || c["to"] != 20.0 {
		t.Fatalf("%v", c)
	}
}

func TestNewClipOnlyOnMidiTracks(t *testing.T) {
	r := newEditRig(t)
	r.st.setCursor(10.3)
	r.e.toggle()
	r.e.moveTrack(+1) // Vox, audio
	r.e.action("new")
	waitFor(t, func() bool { return r.toast() == "NEW CLIP: MIDI TRACKS ONLY" })
	r.e.moveTrack(-1)
	r.e.action("new")
	got := r.waitEdits(t, 1)
	if got[0]["cmd"] != "clip_new" || got[0]["length"] != 4.0 {
		t.Fatalf("%v", got[0])
	}
	if at := got[0]["start"].(float64); at > 10.3 || at < 9 {
		t.Fatalf("start %v not on the grid at the cursor", at)
	}
}

func TestUndoAndErrorsShowAMessage(t *testing.T) {
	r := newEditRig(t)
	r.rec.answer = func(cmd string, a map[string]any) (any, string) { return nil, "nothing_to_undo" }
	r.e.action("undo")
	got := r.waitEdits(t, 1)
	if got[0]["ns"] != "transport" || got[0]["cmd"] != "undo" {
		t.Fatalf("%v", got[0])
	}
	waitFor(t, func() bool { return r.toast() == "NOTHING TO UNDO" })
}

func TestTouchTextInEditMode(t *testing.T) {
	r := newEditRig(t)
	s, _, _ := r.st.get()
	if cols, _ := r.e.touchText(s); cols != nil {
		t.Fatal("touch text with edit mode off")
	}
	r.st.setCursor(5)
	r.e.toggle()
	cols, text := r.e.touchText(s)
	if len(cols) != 7 || cols[0].Name != "Position" || cols[0].Value != "1.1.1" || cols[5].Name != "Velocity" {
		t.Fatalf("%+v", cols)
	}
	if text[uint8(encTrack)] != "TRACK Bass" {
		t.Fatalf("%v", text)
	}
}

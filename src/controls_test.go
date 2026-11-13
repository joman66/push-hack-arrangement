package main

import (
	"sync"
	"testing"
	"time"
)

func TestGridForZoom(t *testing.T) {
	if g := gridFor(1.3); g < 4 { // zoomed out: coarse
		t.Fatalf("grid %v too fine for fit view", g)
	}
	if g := gridFor(300); g != 1.0/16 { // zoomed in: 10px/300 = 0.033 -> 1/16 beat is the next value... 1/32 < want
		t.Fatalf("grid %v", g)
	}
	if g := gridFor(1e-6); g != 32 {
		t.Fatalf("cap %v", g)
	}
}

func TestJogTarget(t *testing.T) {
	cases := []struct {
		t, step float64
		ticks   int
		want    float64
	}{
		{4, 0.25, 1, 4.25},   // aligned
		{4, 0.25, -2, 3.5},   // aligned, back
		{4.1, 0.25, 1, 4.25}, // off grid: nearest grid line forward
		{4.1, 0.25, -1, 4.0}, // off grid: nearest grid line back
		{4.1, 0.25, 3, 4.75}, // off grid, 3 ticks
		{0.1, 0.25, -1, 0},   // clamp at 0
		{2, 0.25, 0, 2},
	}
	for _, c := range cases {
		if got := jogTarget(c.t, c.step, c.ticks); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("jogTarget(%v,%v,%d)=%v want %v", c.t, c.step, c.ticks, got, c.want)
		}
	}
}

func TestJogMultiplier(t *testing.T) {
	if jogMultiplier(200*time.Millisecond) != 1 || jogMultiplier(50*time.Millisecond) != 2 ||
		jogMultiplier(30*time.Millisecond) != 4 || jogMultiplier(10*time.Millisecond) != 8 {
		t.Fatal("ramp wrong")
	}
}

type fbCall struct {
	what string
	on   bool
}

type rig struct {
	c       *controls
	st      *store
	mu      sync.Mutex
	cmds    []map[string]any
	fb      []fbCall
	on      bool
	shift   bool
	delete  bool
	redraws int
}

func newRig() *rig {
	r := &rig{on: true}
	s := viewSet()
	r.st = &store{}
	r.st.putSet(s)
	vc := newViewCtl(func() *Set { x, _, _ := r.st.get(); return x })
	r.c = newControls(vc, r.st,
		func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.on },
		func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.shift },
		func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.delete },
		func() { r.mu.Lock(); r.redraws++; r.mu.Unlock() },
		func(m map[string]any) { r.mu.Lock(); r.cmds = append(r.cmds, m); r.mu.Unlock() },
		func(w string, on bool) { r.mu.Lock(); r.fb = append(r.fb, fbCall{w, on}); r.mu.Unlock() })
	return r
}

func (r *rig) sent() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.cmds...)
}

func (r *rig) feedback() []fbCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]fbCall(nil), r.fb...)
}

func TestPlainJogMovesCursorOnlyNoCommandSent(t *testing.T) {
	r := newRig()
	r.st.putPos(8, true, false, false, 0, 4, false, false) // playing: an audible jump would be heard
	r.c.onCC(ccJog, 1)
	if got := r.sent(); len(got) != 0 {
		t.Fatalf("plain jog must not touch Live: %v", got)
	}
	if tt, _ := r.st.pos(); tt != 8 {
		t.Fatal("the real playhead must not move")
	}
	ct, ok := r.st.getCursor()
	if !ok || ct <= 8 {
		t.Fatalf("cursor did not move forward: %v %v", ct, ok)
	}
}

func TestShiftJogReallySeeks(t *testing.T) {
	r := newRig()
	r.st.putPos(8, false, false, false, 0, 4, false, false)
	r.shift = true
	r.c.onCC(ccJog, 1)
	got := r.sent()
	if len(got) != 1 || got[0]["t"] != "set_time" {
		t.Fatalf("shift+jog must really seek: %v", got)
	}
	if tt, _ := r.st.pos(); tt <= 8 {
		t.Fatalf("playhead did not move forward: %v", tt)
	}
}

func TestShiftJogDropsAnyActiveCursor(t *testing.T) {
	r := newRig()
	r.st.putPos(8, false, false, false, 0, 4, false, false)
	r.st.setCursor(20)
	r.shift = true
	r.c.onCC(ccJog, 1)
	if _, ok := r.st.getCursor(); ok {
		t.Fatal("a real seek must drop the scrub cursor")
	}
}

func TestPlainJogContinuesFromCursorNotStalePlayhead(t *testing.T) {
	r := newRig()
	r.st.putPos(8, false, false, false, 0, 4, false, false)
	r.st.setCursor(40)
	r.c.onCC(ccJog, 1)
	ct, _ := r.st.getCursor()
	if ct <= 40 {
		t.Fatalf("must continue from the cursor (40), got %v", ct)
	}
}

func TestJogPressSetsAnchorSilently(t *testing.T) {
	r := newRig()
	r.st.putPos(8, false, false, false, 0, 4, false, false)
	r.c.onCC(ccJogPress, 127)
	if len(r.sent()) != 0 {
		t.Fatal("setting the anchor must not touch Live or the playhead")
	}
	tt, ok := r.st.getAnchor()
	if !ok || tt != 8 {
		t.Fatalf("anchor %v %v", tt, ok)
	}
	if tp, _ := r.st.pos(); tp != 8 {
		t.Fatal("the playhead itself must not move")
	}
}

func TestJogPressAnchorsAtCursorWhenOneIsActive(t *testing.T) {
	r := newRig()
	r.st.putPos(8, false, false, false, 0, 4, false, false)
	r.st.setCursor(30)
	r.c.onCC(ccJogPress, 127)
	tt, ok := r.st.getAnchor()
	if !ok || tt != 30 {
		t.Fatalf("anchor should follow the cursor: %v %v", tt, ok)
	}
}

func TestJogPressAtSameSpotClearsAnchor(t *testing.T) {
	r := newRig()
	r.st.putPos(8, false, false, false, 0, 4, false, false)
	r.c.onCC(ccJogPress, 127) // sets it
	if _, ok := r.st.getAnchor(); !ok {
		t.Fatal("first press must set the anchor")
	}
	r.c.onCC(ccJogPress, 127) // same spot -> clears it, no Shift needed
	if _, ok := r.st.getAnchor(); ok {
		t.Fatal("second press at the same spot must clear the anchor")
	}
}

func TestJogPressAtDifferentSpotMovesAnchorInstead(t *testing.T) {
	r := newRig()
	r.st.putPos(8, false, false, false, 0, 4, false, false)
	r.c.onCC(ccJogPress, 127) // anchor at 8
	r.st.setLocalTime(20)     // moved elsewhere (a real seek)
	r.c.onCC(ccJogPress, 127) // different spot -> moves the anchor, does not clear it
	tt, ok := r.st.getAnchor()
	if !ok || tt != 20 {
		t.Fatalf("anchor should move to 20, got %v %v", tt, ok)
	}
}

func TestPlayJumpsToAnchorThenToggles(t *testing.T) {
	r := newRig()
	r.st.putPos(20, false, false, false, 0, 4, false, false)
	r.st.setAnchor(4)
	r.c.onCC(ccPlay, 127)
	got := r.sent()
	if len(got) != 2 || got[0]["t"] != "set_time" || got[0]["v"] != 4.0 || got[1]["t"] != "play_toggle" {
		t.Fatalf("cmds %v", got)
	}
	if tt, _ := r.st.pos(); tt != 4 {
		t.Fatal("screen must jump to the anchor at once")
	}
}

func TestPlayWithNoAnchorJustToggles(t *testing.T) {
	r := newRig()
	r.st.putPos(20, false, false, false, 0, 4, false, false)
	r.c.onCC(ccPlay, 127)
	if got := r.sent(); len(got) != 1 || got[0]["t"] != "play_toggle" {
		t.Fatalf("cmds %v", got)
	}
}

func TestPlayWhilePlayingDoesNotJump(t *testing.T) {
	r := newRig()
	r.st.putPos(20, true, false, false, 0, 4, false, false)
	r.st.setAnchor(4)
	r.c.onCC(ccPlay, 127)
	if got := r.sent(); len(got) != 1 || got[0]["t"] != "play_toggle" {
		t.Fatalf("must only stop, not jump: %v", got)
	}
}

func TestVolumeKnobScrollsTime(t *testing.T) {
	r := newRig()
	r.c.vc.zoomH(30)
	before := r.c.vc.viewport(viewSet()).x0
	r.c.onCC(ccVolumeDial, 3)
	if r.c.vc.viewport(viewSet()).x0 <= before {
		t.Fatal("volume knob must scroll forward")
	}
	if len(r.sent()) != 0 {
		t.Fatal("scrolling must not touch Live")
	}
}

func TestTempoKnobZoomsAndClickToggles(t *testing.T) {
	r := newRig()
	s := viewSet()
	ppb0, lane0 := r.c.vc.viewport(s).ppb, r.c.vc.viewport(s).laneH
	r.c.onCC(ccTempoDial, 5) // default: time zoom
	v := r.c.vc.viewport(s)
	if v.ppb <= ppb0 || v.laneH != lane0 {
		t.Fatalf("expected time zoom only: %+v", v)
	}
	r.c.onCC(ccTempoPress, 127)
	r.c.onCC(ccTempoPress, 0) // release ignored
	if v := r.c.vc.viewport(s); len(v.toastLines) != 1 || v.toastLines[0] != "ZOOM: TRACKS" {
		t.Fatalf("toast %v", v.toastLines)
	}
	ppb1 := r.c.vc.viewport(s).ppb
	r.c.onCC(ccTempoDial, 5)
	v = r.c.vc.viewport(s)
	if v.laneH <= lane0 || v.ppb != ppb1 {
		t.Fatalf("expected track zoom only: %+v", v)
	}
	r.c.onCC(ccTempoPress, 127)
	if tl := r.c.vc.viewport(s).toastLines; len(tl) != 1 || tl[0] != "ZOOM: TIME" {
		t.Fatal("toggle back")
	}
}

func TestDeletePlusTempoPressResetsZoomToFit(t *testing.T) {
	r := newRig()
	s := viewSet()
	fitPpb, fitLaneH := fitPPB(s), fitLane(s)
	r.c.vc.zoomH(30)
	r.c.vc.zoomV(30)
	r.c.vc.scrollT(1000)
	v := r.c.vc.viewport(s)
	if v.ppb == fitPpb && v.laneH == fitLaneH && v.x0 == 0 {
		t.Fatal("test setup should have actually zoomed/scrolled away from fit")
	}

	r.delete = true
	r.c.onCC(ccTempoPress, 127)
	v = r.c.vc.viewport(s)
	if v.ppb != fitPpb || v.laneH != fitLaneH || v.x0 != 0 || v.first != 0 {
		t.Fatalf("Delete+Tempo must reset to fit: %+v (want ppb=%v laneH=%v)", v, fitPpb, fitLaneH)
	}
	if len(v.toastLines) != 1 || v.toastLines[0] != "ZOOM: FIT" {
		t.Fatalf("toast %v", v.toastLines)
	}
}

func TestTempoPressWithoutDeleteStillTogglesZoomMode(t *testing.T) {
	r := newRig()
	s := viewSet()
	r.delete = false
	r.c.onCC(ccTempoPress, 127)
	if v := r.c.vc.viewport(s); len(v.toastLines) != 1 || v.toastLines[0] != "ZOOM: TRACKS" {
		t.Fatalf("plain Tempo press must still toggle the zoom mode: %v", v.toastLines)
	}
}

func TestToastExpires(t *testing.T) {
	vc, s := newTestView()
	vc.setToast("hi", 30*time.Millisecond)
	if tl := vc.viewport(s).toastLines; len(tl) != 1 || tl[0] != "hi" {
		t.Fatal("toast must show")
	}
	time.Sleep(50 * time.Millisecond)
	if vc.viewport(s).toastLines != nil {
		t.Fatal("toast must expire")
	}
}

func TestPlayAndSession(t *testing.T) {
	r := newRig()
	r.st.putPos(0, false, false, false, 0, 4, false, false)
	r.c.onCC(ccPlay, 127)
	r.c.onCC(ccPlay, 0) // release ignored
	r.c.onCC(ccSession, 127)
	r.shift = true
	r.c.onCC(ccSession, 127) // Shift+Session: chord, no bta
	got := r.sent()
	if len(got) != 2 || got[0]["t"] != "play_toggle" || got[1]["t"] != "bta" {
		t.Fatalf("cmds %v", got)
	}
	fb := r.feedback()
	if len(fb) != 2 || fb[0] != (fbCall{"play", true}) || fb[1] != (fbCall{"bta", false}) {
		t.Fatalf("feedback %v", fb)
	}
}

func TestLoopAndPunchToggles(t *testing.T) {
	r := newRig()
	r.st.putPos(0, false, false, false, 0, 4, false, false)
	r.c.onCC(ccTopLoop, 127)
	r.c.onCC(ccTopPunchI, 127)
	r.c.onCC(ccTopPunchO, 127)
	got := r.sent()
	if len(got) != 3 || got[0]["t"] != "loop_toggle" || got[1]["t"] != "punch_in_toggle" || got[2]["t"] != "punch_out_toggle" {
		t.Fatalf("cmds %v", got)
	}
	fb := r.feedback()
	if len(fb) != 3 || fb[0] != (fbCall{"loop", true}) || fb[1] != (fbCall{"punch_in", true}) || fb[2] != (fbCall{"punch_out", true}) {
		t.Fatalf("feedback %v", fb)
	}
	// pressing again (once Live's "on" report arrives) must ask to turn it off
	r.st.putPos(0, false, false, true, 0, 4, true, true)
	r.c.onCC(ccTopLoop, 127)
	fb = r.feedback()
	if last := fb[len(fb)-1]; last != (fbCall{"loop", false}) {
		t.Fatalf("second press must ask to turn loop off: %v", last)
	}
}

func TestAdjustLoopHasFrictionSubStepTicksDoNothing(t *testing.T) {
	r := newRig()
	r.st.putPos(0, false, false, false, 8, 16, false, false)
	for i := 0; i < loopEncoderTicksPerStep-1; i++ {
		r.c.onCC(ccEncoder1, 1)
	}
	if got := r.sent(); len(got) != 0 {
		t.Fatalf("fewer than a full step's worth of ticks must do nothing: %v", got)
	}
	_, _, _, start, _ := r.st.transport()
	if start != 8 {
		t.Fatalf("start must not have moved yet: %v", start)
	}
}

func TestAdjustLoopStartAndLength(t *testing.T) {
	r := newRig()
	r.st.putPos(0, false, false, false, 8, 16, false, false)
	for i := 0; i < loopEncoderTicksPerStep; i++ {
		r.c.onCC(ccEncoder1, 1) // +1 bar total (bpb=4 in viewSet), once the friction threshold is crossed
	}
	got := r.sent()
	if len(got) != 1 || got[0]["t"] != "set_loop" || got[0]["start"] != 12.0 || got[0]["length"] != 16.0 {
		t.Fatalf("start bump: %v", got)
	}
	_, _, _, start, _ := r.st.transport()
	if start != 12 {
		t.Fatalf("local start %v", start)
	}
	for i := 0; i < loopEncoderTicksPerStep; i++ {
		r.c.onCC(ccEncoder2, 127) // decodeRel(127) == -1
	}
	_, _, _, _, length := r.st.transport()
	if length != 12 { // 16 - 4
		t.Fatalf("length after -1 bar: %v", length)
	}
}

func TestAdjustLoopShiftIsFinerNotCoarser(t *testing.T) {
	r := newRig()
	r.st.putPos(0, false, false, false, 8, 16, false, false)
	r.shift = true
	for i := 0; i < loopEncoderTicksPerStep; i++ {
		r.c.onCC(ccEncoder1, 1)
	}
	_, _, _, start, _ := r.st.transport()
	if start != 8.25 { // a precise 1/16 beat step, not a whole bar
		t.Fatalf("shift step wrong: start=%v", start)
	}
}

func TestAdjustLoopLengthNeverBelowOneStep(t *testing.T) {
	r := newRig()
	r.st.putPos(0, false, false, false, 0, 4, false, false)
	r.c.onCC(ccEncoder2, 100) // decodeRel(100) == -28: a big negative jump
	_, _, _, _, length := r.st.transport()
	if length <= 0 {
		t.Fatalf("length must stay positive: %v", length)
	}
}

func TestPageButtonsNavigateLocators(t *testing.T) {
	r := newRig() // viewSet's locators: 40 ("A"), 100 ("B")
	r.st.putPos(0, false, false, false, 0, 4, false, false)

	r.c.onCC(ccPageRight, 127) // -> 40
	got := r.sent()
	if len(got) != 1 || got[0]["t"] != "set_time" || got[0]["v"] != 40.0 {
		t.Fatalf("cmds %v", got)
	}
	s, _, _ := r.st.get()
	if !s.HasSelLocator || s.SelLocator != 40 {
		t.Fatalf("selected locator not recorded: %+v", s)
	}

	r.c.onCC(ccPageRight, 127) // -> 100
	got = r.sent()
	if v := got[len(got)-1]["v"]; v != 100.0 {
		t.Fatalf("must reach the next locator: %v", v)
	}

	r.c.onCC(ccPageRight, 127) // no further locator: clamp at 100
	got = r.sent()
	if v := got[len(got)-1]["v"]; v != 100.0 {
		t.Fatalf("must clamp at the last locator: %v", v)
	}

	r.c.onCC(ccPageLeft, 127) // -> 40
	got = r.sent()
	if v := got[len(got)-1]["v"]; v != 40.0 {
		t.Fatalf("page left must move to the earlier locator: %v", v)
	}
}

func TestIgnoredWhenModeOff(t *testing.T) {
	r := newRig()
	r.on = false
	r.c.onCC(ccPlay, 127)
	r.c.onCC(ccJog, 1)
	if len(r.sent()) != 0 {
		t.Fatal("must ignore input while off")
	}
}

func TestDpadRepeatsWhileHeldAndStopsOnRelease(t *testing.T) {
	r := newRig()
	r.c.vc.zoomV(60) // tall lanes -> track scrolling possible
	r.c.onCC(ccDPadDown, 127)
	r.mu.Lock()
	first := r.redraws
	r.mu.Unlock()
	if first != 1 {
		t.Fatalf("press must act once at once, got %d", first)
	}
	time.Sleep(repeatDelay + 3*repeatEvery)
	r.mu.Lock()
	n := r.redraws
	r.mu.Unlock()
	if n < 3 {
		t.Fatalf("expected repeats while held, got %d", n)
	}
	r.c.onCC(ccDPadDown, 0)
	time.Sleep(3 * repeatEvery)
	r.mu.Lock()
	m := r.redraws
	r.mu.Unlock()
	time.Sleep(3 * repeatEvery)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.redraws != m {
		t.Fatal("repeat continued after release")
	}
}

// editRigOn: controls with clip edit mode wired, PushHack faked.
func editControls(t *testing.T) (*rig, *recorder) {
	r := newRig()
	rec := &recorder{}
	cc := newContentCache(rec.call, func() {})
	e := newEditCtl(r.st, r.c.vc, cc, func() {}, r.c.shiftHeld)
	e.call = rec.call
	e.refresh = func(done func()) { done() }
	r.c.edit = e
	t.Cleanup(func() { reservedBottom.Store(0) })
	return r, rec
}

func editCmds(rec *recorder) []string {
	var out []string
	for _, c := range rec.calls() {
		if c["cmd"] != "clips_info" {
			out = append(out, c["cmd"].(string))
		}
	}
	return out
}

func TestClipButtonTogglesEditModeAndEncodersChangeJob(t *testing.T) {
	r, rec := editControls(t)
	r.st.setCursor(0.5)
	r.c.onCC(ccClip, 127)
	if !r.c.editOn() {
		t.Fatal("Clip did not turn edit mode on")
	}
	_, _, _, start, _ := r.st.transport()
	for i := 0; i < 6; i++ {
		r.c.onCC(ccEncoder1, 1)
	}
	if _, _, _, s2, _ := r.st.transport(); s2 != start {
		t.Fatal("encoder 1 moved the loop in edit mode")
	}
	if !r.c.edit.pendActive() {
		t.Fatal("encoder 1 did not start a clip move")
	}
	r.c.onCC(ccClip, 127) // off: the pending edit is sent
	waitFor(t, func() bool { return len(editCmds(rec)) == 1 })
	if editCmds(rec)[0] != "clip_move" {
		t.Fatalf("%v", editCmds(rec))
	}
}

func TestDeleteButtonDeletesOnReleaseUnlessUsedForZoomReset(t *testing.T) {
	r, rec := editControls(t)
	r.st.setCursor(0.5)
	r.c.onCC(ccClip, 127)

	// Delete + Tempo press: zoom reset, no clip deleted
	r.c.onCC(ccDelete, 127)
	r.mu.Lock()
	r.delete = true
	r.mu.Unlock()
	r.c.onCC(ccTempoPress, 127)
	r.c.onCC(ccDelete, 0)
	r.mu.Lock()
	r.delete = false
	r.mu.Unlock()
	time.Sleep(30 * time.Millisecond)
	if n := len(editCmds(rec)); n != 0 {
		t.Fatalf("zoom reset deleted a clip: %v", editCmds(rec))
	}

	// Delete alone: delete the clip
	r.c.onCC(ccDelete, 127)
	r.c.onCC(ccDelete, 0)
	waitFor(t, func() bool { return len(editCmds(rec)) == 1 })
	if editCmds(rec)[0] != "clip_delete" {
		t.Fatalf("%v", editCmds(rec))
	}
}

func TestUndoWorksOutsideEditModeShiftRedoes(t *testing.T) {
	r, rec := editControls(t)
	r.c.onCC(ccUndo, 127)
	r.mu.Lock()
	r.shift = true
	r.mu.Unlock()
	r.c.onCC(ccUndo, 127)
	waitFor(t, func() bool { return len(editCmds(rec)) == 2 })
	if got := editCmds(rec); got[0] != "undo" || got[1] != "redo" {
		t.Fatalf("%v", got)
	}
}

func TestEditButtonsDoNothingOutsideEditMode(t *testing.T) {
	r, rec := editControls(t)
	r.st.setCursor(0.5)
	for _, cc := range []uint8{ccDuplicate, ccMute, ccQuantize, ccNew} {
		r.c.onCC(cc, 127)
		r.c.onCC(cc, 0)
	}
	r.c.onCC(ccDelete, 127)
	r.c.onCC(ccDelete, 0)
	time.Sleep(30 * time.Millisecond)
	if got := editCmds(rec); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

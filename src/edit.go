package main

// Clip edit mode, in the spirit of the MPC's arrange/track editing: pick a
// clip, shape it with the knobs, act on it with the buttons under the screen.
//
//   Clip button          edit mode on/off
//   D-pad up/down        select track      (Shift: scroll as usual)
//   D-pad left/right     select prev/next clip on the track (cursor jumps to it)
//   Jog wheel            move the cursor; the clip under it is selected
//   Encoder 1..7         Position, Track, Start, End, Transpose, Gain/Velocity, Colour
//                        -- shown as a white outline first, sent to Live when the
//                        knob is let go or rests for a moment
//   Buttons under screen Split, Duplicate, Delete, Mute, Quantize, Crop, Copy, Paste
//   Duplicate / Delete / Mute / Quantize buttons   the same actions
//   New                  new empty MIDI clip (one bar) at the cursor
//   Undo (Shift: Redo)   Live's undo
//
// The selected clip is the one on the selected track under the cursor (or the
// playhead, when there is no cursor). Every change goes to Live through
// PushHack (arrangement.clip_*); one job runs at a time, and each waits for the
// fresh arrangement before the next starts, so a job always sees the result
// of the one before.

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
)

const (
	editIdleCommit = 900 * time.Millisecond // an encoder edit is sent after this much rest
	editMinLen     = 1.0 / 16               // shortest clip a trim leaves, in beats
	gainStep       = 0.02                   // audio clip gain per encoder step (0..1)
	velocityStep   = 4                      // MIDI velocity per encoder step
	colorCount     = 70                     // Live's clip colour palette
	quantizeGrid   = 5                      // Live's grid index for 1/16
)

// encoder jobs in edit mode, by encoder index.
const (
	encPosition = iota
	encTrack
	encStart
	encEnd
	encTranspose
	encLevel
	encColor
)

// ticksPerStep: friction per encoder (raw ticks for one step), like the loop knobs.
var ticksPerStep = [8]int{2, 4, 2, 2, 3, 2, 3, 0}

var editEncoderNames = [8]string{"Position", "Track", "Start", "End", "Transpose", "", "Color", ""}

// softLabels: the 8 buttons under the screen in edit mode.
var softLabels = [8]string{"Split", "Duplicate", "Delete", "Mute", "Quantize", "Crop", "Copy", "Paste"}

// pendingEdit: encoder turns not yet sent to Live.
type pendingEdit struct {
	active bool
	box    clipBox // the clip as it is in Live
	midi   bool
	move   float64 // beats
	dtrack int
	head   float64 // start edge, beats (only inwards)
	tail   float64 // end edge, beats
	semis  int
	level  int
	color  int
}

func (p pendingEdit) ghost() clipBox {
	return clipBox{track: p.box.track + p.dtrack, start: p.box.start + p.move + p.head, end: p.box.end + p.move + p.tail}
}

func (p pendingEdit) moves() bool {
	return p.move != 0 || p.dtrack != 0 || p.head != 0 || p.tail != 0
}

type editCtl struct {
	mu        sync.Mutex
	on        bool
	track     int
	pend      pendingEdit
	accum     [8]int
	timer     *time.Timer
	clipboard *clipBox

	jobs    chan func(done func())
	started bool

	st        *store
	vc        *viewCtl
	cc        *contentCache
	call      func(ns, cmd string, args map[string]any, cb func(json.RawMessage, string))
	refresh   func(done func()) // fresh arrangement from Live, then done
	changed   func()
	shiftHeld func() bool
	onToggle  func(on bool) // LEDs
}

func newEditCtl(st *store, vc *viewCtl, cc *contentCache, changed func(), shiftHeld func() bool) *editCtl {
	e := &editCtl{st: st, vc: vc, cc: cc, changed: changed, shiftHeld: shiftHeld,
		call: callLive, jobs: make(chan func(done func()), 64)}
	e.refresh = func(done func()) {
		callLive("arrangement", "snapshot", nil, func(data json.RawMessage, errCode string) {
			if errCode == "" {
				var m wireMsg
				if json.Unmarshal(data, &m) == nil {
					st.putSet(setFromSnapshot(&m))
				}
			}
			done()
		})
	}
	return e
}

func (e *editCtl) isOn() bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.on
}

// ---- jobs: one at a time ----

func (e *editCtl) run(job func(done func())) {
	e.mu.Lock()
	if !e.started {
		e.started = true
		go func() {
			for j := range e.jobs {
				fin := make(chan struct{})
				var once sync.Once
				j(func() { once.Do(func() { close(fin) }) })
				select {
				case <-fin:
				case <-time.After(3 * callTimeout): // never wedge the queue
				}
			}
		}()
	}
	e.mu.Unlock()
	select {
	case e.jobs <- job:
	default:
		e.toast("BUSY")
	}
}

// ---- mode ----

// toggle turns edit mode on or off.
func (e *editCtl) toggle() {
	e.mu.Lock()
	e.on = !e.on
	on := e.on
	e.mu.Unlock()
	if on {
		reservedBottom.Store(softStripH)
		if _, ok := e.st.getCursor(); !ok {
			t, _ := e.st.pos()
			e.st.setCursor(t)
		}
		e.pickTrack()
		if v, _ := scriptVersion.Load().(string); v != "" && !scriptAtLeast(0, 6) {
			e.toast("EDITING NEEDS PUSH HACK SCRIPT 0.6")
		} else {
			e.toast("CLIP EDIT: ON")
		}
	} else {
		e.commitNow()
		reservedBottom.Store(0)
		e.toast("CLIP EDIT: OFF")
	}
	e.vc.revealTrack(e.selTrack())
	if e.onToggle != nil {
		e.onToggle(on)
	}
	e.changed()
}

// off: the arrangement mode itself was turned off.
func (e *editCtl) off() {
	if e.isOn() {
		e.toggle()
	}
}

// pickTrack chooses a track when edit mode turns on: the first one on screen
// with a clip under the cursor, else the first one on screen.
func (e *editCtl) pickTrack() {
	s, _, _ := e.st.get()
	if s == nil || len(s.Tracks) == 0 {
		return
	}
	vp := e.vc.viewport(s)
	first := int(math.Max(0, math.Floor(vp.first)))
	t := e.pointer()
	pick := first
	for i := first; i < len(s.Tracks); i++ {
		if clipAt(&s.Tracks[i], t) >= 0 {
			pick = i
			break
		}
	}
	e.mu.Lock()
	e.track = min(pick, len(s.Tracks)-1)
	e.mu.Unlock()
}

func (e *editCtl) selTrack() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.track
}

// pointer: the cursor, else the playhead.
func (e *editCtl) pointer() float64 {
	if t, ok := e.st.getCursor(); ok {
		return t
	}
	t, _ := e.st.pos()
	return t
}

// clipAt: index of the clip on tr that contains t, or -1.
func clipAt(tr *Track, t float64) int {
	for i, c := range tr.Clips {
		if t >= c.Start-1e-6 && t < c.End-1e-6 {
			return i
		}
	}
	return -1
}

// selected: the selected clip, if there is one.
func (e *editCtl) selected(s *Set) (Clip, clipBox, bool) {
	tr := e.selTrack()
	if s == nil || tr < 0 || tr >= len(s.Tracks) {
		return Clip{}, clipBox{}, false
	}
	i := clipAt(&s.Tracks[tr], e.pointer())
	if i < 0 {
		return Clip{}, clipBox{}, false
	}
	c := s.Tracks[tr].Clips[i]
	return c, clipBox{track: tr, start: c.Start, end: c.End}, true
}

// ---- navigation ----

// moveTrack selects the next track up (-1) or down (+1), skipping group tracks.
func (e *editCtl) moveTrack(dir int) {
	s, _, _ := e.st.get()
	if s == nil || len(s.Tracks) == 0 {
		return
	}
	e.mu.Lock()
	i := e.track
	for {
		i += dir
		if i < 0 || i >= len(s.Tracks) {
			i = e.track
			break
		}
		if s.Tracks[i].Kind != "group" {
			break
		}
	}
	e.track = i
	e.mu.Unlock()
	e.vc.revealTrack(i)
	e.changed()
}

// moveClip selects the previous (-1) or next (+1) clip on the selected track
// and puts the cursor on its start.
func (e *editCtl) moveClip(dir int) {
	s, _, _ := e.st.get()
	tr := e.selTrack()
	if s == nil || tr < 0 || tr >= len(s.Tracks) {
		return
	}
	clips := s.Tracks[tr].Clips
	t := e.pointer()
	cur := clipAt(&s.Tracks[tr], t)
	target := -1
	if dir > 0 {
		for i, c := range clips {
			if c.Start > t+1e-6 {
				target = i
				break
			}
		}
	} else {
		ref := t
		if cur >= 0 {
			ref = clips[cur].Start
		}
		for i := len(clips) - 1; i >= 0; i-- {
			if clips[i].Start < ref-1e-6 {
				target = i
				break
			}
		}
	}
	if target < 0 {
		return
	}
	e.st.setCursor(clips[target].Start)
	e.vc.reveal(clips[target].Start)
	e.changed()
}

// ---- encoders ----

// timeStep: one encoder step in beats -- the jog grid for this zoom, kept
// between a 1/16 note and a bar. Shift: a 1/64 note.
func (e *editCtl) timeStep(s *Set) float64 {
	if e.shiftHeld() {
		return 1.0 / 16
	}
	return math.Max(0.25, math.Min(s.bpb(), gridFor(e.vc.viewport(s).ppb)))
}

// turn: encoder enc (0-7) turned by delta raw ticks.
func (e *editCtl) turn(enc, delta int) {
	if enc < 0 || enc > 7 || ticksPerStep[enc] == 0 || delta == 0 {
		return
	}
	s, _, _ := e.st.get()
	if s == nil {
		return
	}
	e.mu.Lock()
	e.accum[enc] += delta
	steps := e.accum[enc] / ticksPerStep[enc]
	e.accum[enc] -= steps * ticksPerStep[enc]
	e.mu.Unlock()
	if steps == 0 {
		return
	}
	if !e.pendActive() {
		_, box, ok := e.selected(s)
		if !ok {
			e.toast("NO CLIP SELECTED")
			return
		}
		e.mu.Lock()
		e.pend = pendingEdit{active: true, box: box, midi: s.Tracks[box.track].Kind == "midi"}
		e.mu.Unlock()
	}
	step := e.timeStep(s)
	shift := e.shiftHeld()
	e.mu.Lock()
	p := &e.pend
	switch enc {
	case encPosition:
		p.move = math.Max(-p.box.start-p.head, p.move+float64(steps)*step)
	case encTrack:
		p.dtrack = clampInt(p.dtrack+steps, -p.box.track, len(s.Tracks)-1-p.box.track)
	case encStart:
		maxHead := p.box.end + p.tail - p.box.start - editMinLen
		p.head = math.Max(0, math.Min(maxHead, p.head+float64(steps)*step))
	case encEnd:
		minTail := p.box.start + p.head + editMinLen - p.box.end
		p.tail = math.Max(minTail, p.tail+float64(steps)*step)
	case encTranspose:
		n := steps
		if shift {
			n *= 12
		}
		p.semis = clampInt(p.semis+n, -48, 48)
	case encLevel:
		p.level += steps
	case encColor:
		p.color += steps
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.timer = time.AfterFunc(editIdleCommit, e.commitNow)
	e.mu.Unlock()
	e.changed()
}

// released: an encoder was let go; send what is pending now.
func (e *editCtl) released(enc int) {
	if enc >= 0 && enc < 8 && e.pendActive() {
		e.commitNow()
	}
}

func (e *editCtl) pendActive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.pend.active
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// commitNow sends the pending encoder edit as a chain of commands.
func (e *editCtl) commitNow() {
	e.mu.Lock()
	p := e.pend
	e.pend = pendingEdit{}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.mu.Unlock()
	if !p.active {
		return
	}
	info := e.cc.info(p.box.track, p.box.start)

	type step struct {
		cmd  string
		args func(ref clipBox) map[string]any
	}
	var steps []step
	if p.head != 0 || p.tail != 0 {
		steps = append(steps, step{"clip_trim", func(r clipBox) map[string]any {
			return map[string]any{"new_start": r.start + p.head, "new_end": r.end + p.tail}
		}})
	}
	if p.move != 0 || p.dtrack != 0 {
		steps = append(steps, step{"clip_move", func(r clipBox) map[string]any {
			a := map[string]any{"to": math.Max(0, r.start+p.move)}
			if p.dtrack != 0 {
				a["to_track"] = r.track + p.dtrack
			}
			return a
		}})
	}
	if p.semis != 0 {
		steps = append(steps, step{"clip_transpose", func(clipBox) map[string]any {
			return map[string]any{"semitones": p.semis}
		}})
	}
	if p.level != 0 {
		switch {
		case p.midi:
			steps = append(steps, step{"clip_velocity", func(clipBox) map[string]any {
				return map[string]any{"delta": p.level * velocityStep}
			}})
		case info != nil:
			g := math.Max(0, math.Min(1, info.Gain+float64(p.level)*gainStep))
			steps = append(steps, step{"clip_set", func(clipBox) map[string]any { return map[string]any{"gain": g} }})
		default:
			e.toast("GAIN: CLIP NOT READ YET")
		}
	}
	if p.color != 0 {
		if info != nil && info.ColorIndex >= 0 {
			ci := ((info.ColorIndex+p.color)%colorCount + colorCount) % colorCount
			steps = append(steps, step{"clip_set", func(clipBox) map[string]any { return map[string]any{"color_index": ci} }})
		} else {
			e.toast("COLOR: CLIP NOT READ YET")
		}
	}
	if len(steps) == 0 {
		e.changed()
		return
	}
	e.run(func(done func()) {
		ref := p.box
		touched := map[int]bool{ref.track: true}
		var next func(i int)
		next = func(i int) {
			if i == len(steps) {
				e.after(ref, touched, done)
				return
			}
			args := steps[i].args(ref)
			args["track"], args["start"] = ref.track, ref.start
			e.call("arrangement", steps[i].cmd, args, func(data json.RawMessage, errCode string) {
				if errCode != "" {
					e.fail(errCode)
					e.after(ref, touched, done)
					return
				}
				if r, ok := replyClip(data); ok {
					ref = r
					touched[r.track] = true
				}
				next(i + 1)
			})
		}
		next(0)
	})
	e.changed()
}

// replyClip reads {"clip":{track,start,end}} from an edit reply.
func replyClip(data json.RawMessage) (clipBox, bool) {
	var raw struct {
		Clip *struct {
			Track int     `json:"track"`
			Start float64 `json:"start"`
			End   float64 `json:"end"`
		} `json:"clip"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.Clip == nil {
		return clipBox{}, false
	}
	return clipBox{track: raw.Clip.Track, start: raw.Clip.Start, end: raw.Clip.End}, true
}

// after an edit: select the resulting clip, read the arrangement again, and
// fetch the content of the tracks that changed.
func (e *editCtl) after(ref clipBox, touched map[int]bool, done func()) {
	if ref.track >= 0 {
		e.mu.Lock()
		e.track = ref.track
		e.mu.Unlock()
		e.st.setCursor(ref.start)
	}
	var tracks []int
	for t := range touched {
		tracks = append(tracks, t)
	}
	e.cc.invalidate(tracks...)
	e.refresh(func() {
		e.vc.revealTrack(e.selTrack())
		e.vc.reveal(e.pointer())
		e.changed()
		done()
	})
}

// ---- actions ----

// action runs one of the clip actions on the selected clip.
func (e *editCtl) action(name string) {
	e.commitNow()
	e.run(func(done func()) {
		s, _, _ := e.st.get()
		c, box, ok := e.selected(s)
		needClip := name != "paste" && name != "new" && name != "undo" && name != "redo"
		if needClip && !ok {
			e.toast("NO CLIP SELECTED")
			done()
			return
		}
		args := map[string]any{"track": box.track, "start": box.start}
		touched := map[int]bool{box.track: true}
		keep := box // what to select after
		cmd, ns := "", "arrangement"
		switch name {
		case "split":
			at := e.pointer()
			if at <= box.start+editMinLen || at >= box.end-editMinLen {
				e.toast("PUT THE CURSOR INSIDE THE CLIP")
				done()
				return
			}
			cmd, args["at"] = "clip_split", at
		case "duplicate":
			cmd = "clip_duplicate"
		case "delete":
			cmd, keep = "clip_delete", clipBox{track: box.track, start: e.pointer()}
		case "mute":
			cmd, args["muted"] = "clip_set", !c.Muted
		case "quantize":
			cmd, args["grid"], args["amount"] = "clip_quantize", quantizeGrid, 1.0
		case "crop":
			cmd = "clip_crop"
		case "copy":
			b := box
			e.mu.Lock()
			e.clipboard = &b
			e.mu.Unlock()
			e.toast("COPIED " + strings.ToUpper(c.Name))
			done()
			return
		case "paste":
			e.mu.Lock()
			cb := e.clipboard
			e.mu.Unlock()
			if cb == nil {
				e.toast("NOTHING COPIED")
				done()
				return
			}
			tr := e.selTrack()
			cmd = "clip_duplicate"
			args = map[string]any{"track": cb.track, "start": cb.start, "to": e.pointer(), "to_track": tr}
			touched[tr] = true
		case "new":
			tr := e.selTrack()
			if s == nil || tr < 0 || tr >= len(s.Tracks) {
				done()
				return
			}
			if s.Tracks[tr].Kind != "midi" {
				e.toast("NEW CLIP: MIDI TRACKS ONLY")
				done()
				return
			}
			at := math.Floor(e.pointer()/e.timeStep(s)+1e-9) * e.timeStep(s)
			cmd = "clip_new"
			args = map[string]any{"track": tr, "start": at, "length": s.bpb()}
			touched = map[int]bool{tr: true}
		case "undo", "redo":
			ns, cmd, args = "transport", name, nil
			keep = clipBox{track: -1}
			e.cc.invalidate(allTracks(s)...)
		default:
			done()
			return
		}
		e.call(ns, cmd, args, func(data json.RawMessage, errCode string) {
			if errCode != "" {
				e.fail(errCode)
				e.after(clipBox{track: -1}, touched, done)
				return
			}
			if r, ok := replyClip(data); ok {
				keep = r
				touched[r.track] = true
			}
			e.toast(actionDone[name])
			e.after(keep, touched, done)
		})
	})
}

var actionDone = map[string]string{
	"split": "SPLIT", "duplicate": "DUPLICATED", "delete": "DELETED", "mute": "MUTE CHANGED",
	"quantize": "QUANTIZED", "crop": "CROPPED", "paste": "PASTED", "new": "NEW CLIP",
	"undo": "UNDO", "redo": "REDO",
}

func allTracks(s *Set) []int {
	if s == nil {
		return nil
	}
	out := make([]int, len(s.Tracks))
	for i := range out {
		out[i] = i
	}
	return out
}

// ---- feedback ----

func (e *editCtl) toast(msg string) {
	if msg == "" {
		return
	}
	e.vc.setToast(msg, toastFor)
	e.changed()
	time.AfterFunc(toastFor+50*time.Millisecond, e.changed)
}

var errText = map[string]string{
	"no_clip":          "CLIP NOT FOUND",
	"looping":          "LOOPED CLIP: CANNOT EXTEND",
	"wrong_clip_kind":  "NOT FOR THIS KIND OF CLIP",
	"wrong_track_kind": "WRONG KIND OF TRACK",
	"unknown_cmd":      "UPDATE PUSH HACK (SCRIPT 0.6)",
	"not_connected":    "LIVE NOT CONNECTED",
	"timeout":          "LIVE DID NOT ANSWER",
	"failed":           "LIVE REFUSED THE EDIT",
	"bad_arg":          "NOT POSSIBLE THERE",
	"bad_track":        "NOT ON THAT TRACK",
	"nothing_to_undo":  "NOTHING TO UNDO",
	"nothing_to_redo":  "NOTHING TO REDO",
	"too_big":          "SET TOO LONG",
}

func (e *editCtl) fail(code string) {
	msg, ok := errText[code]
	if !ok {
		msg = "ERROR: " + strings.ToUpper(code)
	}
	e.toast(msg)
}

// ---- drawing ----

// decorate adds the edit mode's parts to the viewport.
func (e *editCtl) decorate(s *Set, vp *viewport) {
	if !e.isOn() || s == nil {
		return
	}
	vp.selTrack, vp.selTrackOn = e.selTrack(), true
	e.mu.Lock()
	p := e.pend
	e.mu.Unlock()
	if p.active {
		b, g := p.box, p.ghost()
		vp.sel = &b
		if p.moves() {
			vp.ghost = &g
		}
	} else if _, box, ok := e.selected(s); ok {
		vp.sel = &box
	}
	var soft [8]widgets.SoftButton
	for i, l := range softLabels {
		soft[i] = widgets.SoftButton{Label: l}
	}
	e.mu.Lock()
	empty := e.clipboard == nil
	e.mu.Unlock()
	if empty {
		soft[7].State = widgets.SoftOff
	}
	vp.soft = &soft
}

// touchText: what the knobs show in edit mode (header columns and the bottom
// bar while turning). nil when edit mode is off.
func (e *editCtl) touchText(s *Set) ([]widgets.HeaderCol, map[uint8]string) {
	if !e.isOn() || s == nil {
		return nil, nil
	}
	bpb := s.bpb()
	e.mu.Lock()
	p := e.pend
	e.mu.Unlock()
	if !p.active {
		_, box, ok := e.selected(s)
		if !ok {
			cols := []widgets.HeaderCol{{Col: 0, Name: "No clip", Value: "--"}}
			return cols, map[uint8]string{}
		}
		p = pendingEdit{box: box, midi: s.Tracks[box.track].Kind == "midi"}
	}
	g := p.ghost()
	trackName := "?"
	if g.track >= 0 && g.track < len(s.Tracks) {
		trackName = s.Tracks[g.track].Name
	}
	info := e.cc.info(p.box.track, p.box.start)
	level, levelName := fmt.Sprintf("%+d", p.level*velocityStep), "Velocity"
	if !p.midi {
		levelName = "Gain"
		level = "--"
		if info != nil {
			level = fmt.Sprintf("%.2f", math.Max(0, math.Min(1, info.Gain+float64(p.level)*gainStep)))
		}
	}
	color := "--"
	if info != nil && info.ColorIndex >= 0 {
		color = fmt.Sprint(((info.ColorIndex+p.color)%colorCount + colorCount) % colorCount)
	}
	vals := [8]string{
		bbsPosition(g.start-p.head, bpb), trackName, bbsPosition(g.start, bpb), bbsPosition(g.end, bpb),
		fmt.Sprintf("%+d st", p.semis), level, color, "",
	}
	names := editEncoderNames
	names[encLevel] = levelName
	var cols []widgets.HeaderCol
	text := map[uint8]string{}
	for i := 0; i < 7; i++ {
		cols = append(cols, widgets.HeaderCol{Col: i, Name: names[i], Value: vals[i]})
		text[uint8(i)] = strings.ToUpper(names[i]) + " " + vals[i]
	}
	return cols, text
}

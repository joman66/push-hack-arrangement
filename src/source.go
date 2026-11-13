package main

// Where the arrangement comes from: the PushHack Remote Script running inside
// Live (ships with Push Hack). It serves the OPEN set (also unsaved edits) over a
// local Unix socket, one JSON per line. We subscribe to its "arrangement" and
// "position" topics. Messages are turned into the old snapshot / pos shape here,
// and commands go the other way (see sendCmd), so the rest of the hack does not
// know about the protocol.

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// liveSocketPath is a var so a test can use its own path.
var liveSocketPath = "/tmp/push-hack.sock"

const msgNotConnected = "Remote Script not connected"

type store struct {
	mu       sync.Mutex
	set      *Set
	msg      string
	hint     string
	playhead float64
	playing  bool
	bta      bool // a track is off the arrangement (Back to Arrangement is lit)

	loopOn                bool
	loopStart, loopLength float64
	punchIn, punchOut     bool

	// hold / loopHold: ignore Live's reported time (hold) or loop start/length
	// (loopHold) until each passes. Set on a local jog or loop-knob edit, so
	// the screen shows OUR value at once instead of waiting a round trip to
	// the Remote Script and back. Kept SEPARATE on purpose: they used to share
	// one field, so turning the loop knob kept re-arming the same hold and the
	// on-screen playhead would sit frozen for as long as you kept turning it,
	// even while the song was playing and Live's own position kept moving.
	// Booleans (playing, bta, loop on/off, punch) don't need a hold at all: a
	// button press is a one-shot action, so the ~100-250ms round trip is not
	// felt the way it would be while a knob is turning continuously.
	hold     time.Time
	loopHold time.Time

	// Play-start anchor (jog wheel press). Local only: never sent by Live.
	hasAnchor bool
	anchor    float64

	// Locator last jumped to with Page </>. Local only, matched by time.
	hasSelLocator bool
	selLocator    float64

	// Scrub cursor (plain jog wheel). Local only, never sent to or echoed by
	// Live: this is the whole point -- looking around without seeking. A real
	// seek (setLocalTime, from Shift+jog, locator nav or the anchor jump)
	// drops it, since the real playhead is now wherever the cursor pointed to.
	hasCursor bool
	cursor    float64
}

// get returns a copy of the set with the live playhead, or a message to show.
func (st *store) get() (*Set, string, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.set == nil {
		return nil, st.msg, st.hint
	}
	s := *st.set // slices shared, read-only
	s.Playhead = st.playhead
	s.Overridden = st.bta
	s.Loop = Loop{On: st.loopOn, Start: st.loopStart, Length: st.loopLength}
	s.PunchIn, s.PunchOut = st.punchIn, st.punchOut
	s.HasAnchor, s.Anchor = st.hasAnchor, st.anchor
	s.HasSelLocator, s.SelLocator = st.hasSelLocator, st.selLocator
	s.HasCursor, s.Cursor = st.hasCursor, st.cursor
	return &s, "", ""
}

func (st *store) putSet(s *Set) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.set, st.msg, st.hint = s, "", ""
}

func (st *store) putMessage(msg, hint string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.set, st.msg, st.hint = nil, msg, hint
}

func (st *store) pos() (float64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.playhead, st.playing
}

func (st *store) state() (playing, bta bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.playing, st.bta
}

// transport returns the loop and punch state (loop on/off, punch in/out, loop
// start and length in beats).
func (st *store) transport() (loopOn, punchIn, punchOut bool, loopStart, loopLength float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.loopOn, st.punchIn, st.punchOut, st.loopStart, st.loopLength
}

// putPos stores what Live reported. Time is ignored for a short while after
// our own jog/seek edit (hold), and loop start/length after our own knob edit
// (loopHold) -- independently, so turning the loop knob can never freeze the
// playhead, or vice versa. The on/off flags always update at once (see the
// `hold` field's comment).
func (st *store) putPos(t float64, playing, bta, loopOn bool, loopStart, loopLength float64, punchIn, punchOut bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	if now.After(st.hold) {
		st.playhead = t
	}
	if now.After(st.loopHold) {
		st.loopStart, st.loopLength = loopStart, loopLength
	}
	if bta != st.bta {
		log.Printf("back to arrangement available: %v", bta)
	}
	st.playing, st.bta = playing, bta
	st.loopOn, st.punchIn, st.punchOut = loopOn, punchIn, punchOut
}

// setLocalTime moves the playhead at once (jog), before Live confirms.
func (st *store) setLocalTime(t float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.playhead = t
	st.hold = time.Now().Add(localHold)
	st.hasCursor = false // a real seek: the cursor's job is done
}

// setCursor / clearCursor / getCursor: the scrub cursor. Local only, and
// unlike setLocalTime, NOT reconciled against anything Live reports -- there
// is nothing to reconcile, since we never told Live to move.
func (st *store) setCursor(t float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hasCursor, st.cursor = true, t
}

func (st *store) clearCursor() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hasCursor = false
}

func (st *store) getCursor() (float64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cursor, st.hasCursor
}

// setLocalLoop moves the loop start/length at once (the loop knobs), before
// Live confirms. Uses its own hold (loopHold), separate from the playhead's,
// so adjusting the loop never freezes the on-screen playhead.
func (st *store) setLocalLoop(start, length float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loopStart, st.loopLength = start, length
	st.loopHold = time.Now().Add(localHold)
}

const localHold = 400 * time.Millisecond

// setAnchor / clearAnchor / getAnchor: the play-start anchor (jog wheel
// press). Local only -- Live never reports it back.
func (st *store) setAnchor(t float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hasAnchor, st.anchor = true, t
}

func (st *store) clearAnchor() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hasAnchor = false
}

func (st *store) getAnchor() (float64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.anchor, st.hasAnchor
}

// setSelLocator remembers which locator (by time) Page </> last jumped to,
// so the render can draw its line white instead of light blue.
func (st *store) setSelLocator(t float64) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.hasSelLocator, st.selLocator = true, t
}

const hintActivate = "Enable PushHack in Live Preferences (control surface, Input/Output None), restart Live"

// wire format ---------------------------------------------------------------

type wireClip struct {
	Start, End float64
	Name       string
	Color      uint32
	Muted      bool
}

func (c *wireClip) UnmarshalJSON(b []byte) error {
	var a []json.RawMessage
	if err := json.Unmarshal(b, &a); err != nil || len(a) < 4 {
		return err
	}
	_ = json.Unmarshal(a[0], &c.Start)
	_ = json.Unmarshal(a[1], &c.End)
	_ = json.Unmarshal(a[2], &c.Name)
	_ = json.Unmarshal(a[3], &c.Color)
	if len(a) > 4 { // [.., muted] since script 0.6.0
		var m int
		_ = json.Unmarshal(a[4], &m)
		c.Muted = m != 0
	}
	return nil
}

type wireMsg struct {
	T string `json:"t"`

	// Envelope of the PushHack protocol: an event carries a topic and data, a
	// reply carries ok. See readLive.
	ID    int             `json:"id"`
	OK    *bool           `json:"ok"`
	Error string          `json:"error"`
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"`

	Tracks []struct {
		Name  string     `json:"name"`
		Kind  string     `json:"kind"`
		Color uint32     `json:"color"`
		Group int        `json:"group"`
		Clips []wireClip `json:"clips"`
	} `json:"tracks"`
	Locators []struct {
		Time float64 `json:"time"`
		Name string  `json:"name"`
	} `json:"locators"`
	Sig   [2]int  `json:"sig"`
	Tempo float64 `json:"tempo"`

	// "pos" fields. Loop/punch live here, not in the snapshot: "pos" is sent
	// every tick it changes, so a toggle or a knob turn is reflected almost
	// at once, instead of waiting for the next (much rarer) snapshot.
	Time       float64 `json:"time"`
	Playing    bool    `json:"playing"`
	BTA        bool    `json:"bta"`
	LoopOn     bool    `json:"loop_on"`
	LoopStart  float64 `json:"loop_start"`
	LoopLength float64 `json:"loop_length"`
	PunchIn    bool    `json:"punch_in"`
	PunchOut   bool    `json:"punch_out"`
}

func setFromSnapshot(m *wireMsg) *Set {
	s := &Set{Name: "Live Set", BeatsPerBar: 4, Tempo: m.Tempo}
	if m.Sig[0] > 0 && m.Sig[1] > 0 {
		s.BeatsPerBar = float64(m.Sig[0]) * 4 / float64(m.Sig[1])
	}
	for i, t := range m.Tracks {
		tr := Track{Name: t.Name, Kind: t.Kind, Color: t.Color, GroupID: t.Group, ID: i}
		for _, c := range t.Clips {
			if c.End <= c.Start {
				continue
			}
			col := c.Color
			if col == 0 {
				col = t.Color
			}
			tr.Clips = append(tr.Clips, Clip{Start: c.Start, End: c.End, Name: c.Name, Color: col, Muted: c.Muted})
			if c.End > s.Length {
				s.Length = c.End
			}
		}
		s.Tracks = append(s.Tracks, tr)
	}
	for _, l := range m.Locators {
		s.Locators = append(s.Locators, Locator{Time: l.Time, Name: l.Name})
	}
	sort.Slice(s.Locators, func(i, j int) bool { return s.Locators[i].Time < s.Locators[j].Time })
	// Loop/punch are not in the snapshot (see wireMsg): the store overlays
	// them from the last "pos" message, the same way it overlays Playhead.
	return s
}

// runLiveSource keeps a connection to the Remote Script; reconnects on loss.
// onChange is called after every update that changes what is drawn.
func runLiveSource(st *store, onChange func(), stop <-chan struct{}) {
	st.putMessage(msgNotConnected, hintActivate)
	for {
		conn, err := net.DialTimeout("unix", liveSocketPath, 2*time.Second)
		if err == nil {
			// hello and subscribe go out before anything else. The replies come
			// back through readLive.
			_, err = conn.Write([]byte(handshake))
		}
		if err == nil {
			link.set(conn)
			log.Print("Remote Script connected")
			st.putMessage("Waiting for Live Set data", "")
			readLive(conn, st, onChange, stop)
			link.set(nil)
			calls.failAll("not_connected")
			conn.Close()
			log.Print("Remote Script disconnected")
			st.putMessage(msgNotConnected, hintActivate)
			onChange()
		}
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// handshake: ids 1 and 2 are matched in readLive.
const handshake = `{"id":1,"cmd":"hello"}` + "\n" +
	`{"id":2,"cmd":"subscribe","topics":["arrangement","position"]}` + "\n"

func readLive(conn net.Conn, st *store, onChange func(), stop <-chan struct{}) {
	go func() { <-stop; conn.Close() }()
	r := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		m, ok := decodeLive(line)
		if !ok {
			continue
		}
		switch m.T {
		case "snapshot":
			s := setFromSnapshot(&m)
			st.putSet(s)
			log.Printf("snapshot: %d tracks, %.0f beats, %d locators", len(s.Tracks), s.Length, len(s.Locators))
			onChange()
		case "pos":
			st.putPos(m.Time, m.Playing, m.BTA, m.LoopOn, m.LoopStart, m.LoopLength, m.PunchIn, m.PunchOut)
			onChange()
		case "refused":
			// Subscribe failed: a PushHack too old to have these topics.
			log.Printf("Remote Script refused the subscription: %s", m.Error)
			st.putMessage("PushHack Remote Script is too old", hintActivate)
			onChange()
			return
		}
	}
}

// decodeLive turns one line from PushHack into a wireMsg whose T says what
// it is: "snapshot", "pos", "refused", or "" for a reply we do not need.
func decodeLive(line []byte) (wireMsg, bool) {
	var m wireMsg
	if err := json.Unmarshal(line, &m); err != nil {
		log.Printf("bad message: %v", err)
		return m, false
	}
	switch {
	case m.T == "event" && m.Topic == "arrangement":
		var d wireMsg
		if err := json.Unmarshal(m.Data, &d); err != nil {
			log.Printf("bad snapshot: %v", err)
			return m, false
		}
		d.T = "snapshot"
		return d, true
	case m.T == "event" && m.Topic == "position":
		var d wireMsg
		if err := json.Unmarshal(m.Data, &d); err != nil {
			log.Printf("bad position: %v", err)
			return m, false
		}
		d.T = "pos"
		return d, true
	case m.OK != nil && m.ID >= firstCallID:
		calls.deliver(m.ID, m.Data, m.Error, *m.OK)
	case m.OK != nil && *m.OK && m.ID == 1:
		var hello struct {
			Version string `json:"script_version"`
		}
		_ = json.Unmarshal(m.Data, &hello)
		scriptVersion.Store(hello.Version)
		log.Printf("PushHack script %s", hello.Version)
	case m.OK != nil && !*m.OK:
		if m.ID == 1 || m.ID == 2 { // hello or subscribe; a failed command is only logged
			return wireMsg{T: "refused", Error: m.Error}, true
		}
		log.Printf("Remote Script refused a command: %s", m.Error)
	}
	return m, false
}

// link: the current connection, for commands to Live.
type liveLink struct {
	mu   sync.Mutex
	conn net.Conn
}

var link liveLink

func (l *liveLink) set(c net.Conn) {
	l.mu.Lock()
	l.conn = c
	l.mu.Unlock()
}

// request turns a command of this hack ({"t": "set_time", "v": 12}) into the
// PushHack form ({"ns": "transport", "cmd": "set_time", "v": 12}). ok is false
// for a command we do not know.
func request(m map[string]any) (map[string]any, bool) {
	t, _ := m["t"].(string)
	ns := map[string]string{
		"set_time": "transport", "set_loop": "transport", "play_toggle": "transport",
		"bta": "arrangement", "loop_toggle": "arrangement",
		"punch_in_toggle": "arrangement", "punch_out_toggle": "arrangement",
	}[t]
	if ns == "" {
		return nil, false
	}
	out := map[string]any{"ns": ns, "cmd": t}
	for k, v := range m {
		if k != "t" {
			out[k] = v
		}
	}
	return out, true
}

// sendCmd sends one command line to the Remote Script. Dropped if not connected.
func sendCmd(m map[string]any) {
	req, ok := request(m)
	if !ok {
		log.Printf("unknown command %v", m["t"])
		return
	}
	b, err := json.Marshal(req)
	if err != nil {
		return
	}
	link.mu.Lock()
	defer link.mu.Unlock()
	if link.conn == nil {
		return
	}
	_ = link.conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
	_, _ = link.conn.Write(append(b, '\n'))
}

// ---- calls: a command that wants its reply -------------------------------

// firstCallID: ids below this are the handshake (1, 2) and fire-and-forget
// commands (no id). Replies at or above it go to a waiting callback.
const firstCallID = 100

const callTimeout = 5 * time.Second

// scriptVersion: PushHack's version from the hello reply ("" = not known yet).
var scriptVersion atomic.Value

// scriptAtLeast: true when the connected PushHack is at least major.minor.
func scriptAtLeast(major, minor int) bool {
	v, _ := scriptVersion.Load().(string)
	p := strings.SplitN(v, ".", 3)
	if len(p) < 2 {
		return false
	}
	a, _ := strconv.Atoi(p[0])
	b, _ := strconv.Atoi(p[1])
	return a > major || (a == major && b >= minor)
}

type callTable struct {
	mu   sync.Mutex
	next int
	m    map[int]func(json.RawMessage, string)
}

var calls = callTable{next: firstCallID, m: map[int]func(json.RawMessage, string){}}

func (t *callTable) add(cb func(json.RawMessage, string)) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := t.next
	t.next++
	t.m[id] = cb
	return id
}

func (t *callTable) take(id int) func(json.RawMessage, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cb := t.m[id]
	delete(t.m, id)
	return cb
}

// deliver hands a reply to its callback. errCode is "" on success.
func (t *callTable) deliver(id int, data json.RawMessage, errCode string, ok bool) {
	cb := t.take(id)
	if cb == nil {
		return
	}
	if ok {
		errCode = ""
	} else if errCode == "" {
		errCode = "failed"
	}
	go cb(data, errCode)
}

// failAll: the link dropped; nobody will answer.
func (t *callTable) failAll(code string) {
	t.mu.Lock()
	m := t.m
	t.m = map[int]func(json.RawMessage, string){}
	t.mu.Unlock()
	for _, cb := range m {
		go cb(nil, code)
	}
}

// callLive sends {ns, cmd, args...} and runs cb with the reply data, or an
// error code ("not_connected", "timeout", or PushHack's own code). cb runs on
// its own goroutine. Never blocks.
func callLive(ns, cmd string, args map[string]any, cb func(data json.RawMessage, errCode string)) {
	if cb == nil {
		cb = func(json.RawMessage, string) {}
	}
	id := calls.add(cb)
	req := map[string]any{"id": id, "ns": ns, "cmd": cmd}
	for k, v := range args {
		req[k] = v
	}
	b, err := json.Marshal(req)
	if err != nil {
		calls.deliver(id, nil, "bad_arg", false)
		return
	}
	link.mu.Lock()
	conn := link.conn
	if conn != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
		_, err = conn.Write(append(b, '\n'))
	}
	link.mu.Unlock()
	if conn == nil || err != nil {
		calls.deliver(id, nil, "not_connected", false)
		return
	}
	time.AfterFunc(callTimeout, func() {
		if cb := calls.take(id); cb != nil {
			cb(nil, "timeout")
		}
	})
}

// refreshSnapshot asks for the arrangement at once (after an edit), instead
// of waiting up to a second for the next "arrangement" event.
func refreshSnapshot(st *store, onChange func()) {
	callLive("arrangement", "snapshot", nil, func(data json.RawMessage, errCode string) {
		if errCode != "" {
			return
		}
		var m wireMsg
		if json.Unmarshal(data, &m) != nil {
			return
		}
		st.putSet(setFromSnapshot(&m))
		onChange()
	})
}

package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const snapJSON = `{"t":"snapshot","tracks":[
 {"name":"Drums","kind":"group","color":16711680,"group":-1,"clips":[]},
 {"name":"Kick","kind":"midi","color":65280,"group":0,"clips":[[0,8,"A",1122867],[8,16,"B",0],[5,5,"empty",1]]}],
 "locators":[{"time":9,"name":"B"},{"time":4,"name":"Drop"}],"sig":[3,4],"tempo":124}`

func TestSetFromSnapshot(t *testing.T) {
	var m wireMsg
	if err := json.Unmarshal([]byte(snapJSON), &m); err != nil {
		t.Fatal(err)
	}
	s := setFromSnapshot(&m)
	if len(s.Tracks) != 2 || s.Tracks[1].GroupID != 0 || s.Tracks[1].Kind != "midi" {
		t.Fatalf("tracks %+v", s.Tracks)
	}
	cl := s.Tracks[1].Clips
	if len(cl) != 2 { // zero-length clip dropped
		t.Fatalf("clips %+v", cl)
	}
	if cl[0].Color != 1122867 || cl[1].Color != 65280 { // 0 -> track colour
		t.Fatalf("colours %+v", cl)
	}
	if s.Length != 16 || s.BeatsPerBar != 3 {
		t.Fatalf("set %+v", s)
	}
	// snapshot carries locators out of order; setFromSnapshot must sort them.
	if len(s.Locators) != 2 || s.Locators[0].Time != 4 || s.Locators[1].Time != 9 {
		t.Fatalf("locators not sorted: %+v", s.Locators)
	}
	// Loop/punch are not in the snapshot -- they come from "pos" (see store).
	if s.Loop.On || s.Loop.Length != 0 {
		t.Fatalf("snapshot must not carry loop state: %+v", s.Loop)
	}
}

func TestStoreMessageThenSetWithPlayhead(t *testing.T) {
	st := &store{}
	st.putMessage("x", "y")
	if s, msg, hint := st.get(); s != nil || msg != "x" || hint != "y" {
		t.Fatal("message state wrong")
	}
	st.putSet(&Set{Length: 8})
	st.putPos(3.5, true, false, false, 0, 4, false, false)
	s, _, _ := st.get()
	if s == nil || s.Playhead != 3.5 {
		t.Fatalf("playhead %+v", s)
	}
}

func TestLocalTimeHoldsAgainstLatePos(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.setLocalTime(6)
	st.putPos(1, true, true, false, 0, 4, false, false) // late report from Live
	if tt, playing := st.pos(); tt != 6 || !playing {
		t.Fatalf("t=%v playing=%v", tt, playing)
	}
	if _, bta := st.state(); !bta {
		t.Fatal("bta must still update (booleans are not held)")
	}
}

func TestLocalLoopHoldsAgainstLatePos(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.setLocalLoop(16, 32)
	st.putPos(1, false, false, true, 0, 4, false, false) // late report: stale start/length
	on, _, _, start, length := st.transport()
	if start != 16 || length != 32 {
		t.Fatalf("local loop overwritten: start=%v length=%v", start, length)
	}
	if !on {
		t.Fatal("loop-on is a boolean: must update at once")
	}
}

// Regression: hold and loopHold used to be the same field, so adjusting the
// loop start/length (a knob turn keeps re-arming its hold) also suppressed
// every Live-reported playhead update -- the on-screen playhead would sit
// frozen for as long as you kept turning the knob, even while the song was
// playing. They must be independent.
func TestAdjustingLoopDoesNotFreezeThePlayhead(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.setLocalLoop(16, 32) // arms loopHold; must NOT touch the playhead's hold
	st.putPos(42, true, false, true, 0, 4, false, false)
	if tt, playing := st.pos(); tt != 42 || !playing {
		t.Fatalf("playhead must keep advancing while the loop is held: t=%v playing=%v", tt, playing)
	}
	_, _, _, start, length := st.transport()
	if start != 16 || length != 32 {
		t.Fatalf("loop should still be held: start=%v length=%v", start, length)
	}
}

// The mirror image: jogging/seeking must not freeze the loop's on-screen
// start/length either.
func TestSeekingDoesNotFreezeTheLoop(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.setLocalTime(6) // arms hold; must NOT touch the loop's hold
	st.putPos(1, false, false, true, 99, 111, false, false)
	_, _, _, start, length := st.transport()
	if start != 99 || length != 111 {
		t.Fatalf("loop must keep updating while the time is held: start=%v length=%v", start, length)
	}
	if tt, _ := st.pos(); tt != 6 {
		t.Fatalf("the time hold should still protect the playhead: %v", tt)
	}
}

func TestHoldExpiresAndLiveWins(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.setLocalTime(6)
	time.Sleep(localHold + 20*time.Millisecond)
	st.putPos(9, false, false, false, 0, 4, false, false)
	if tt, _ := st.pos(); tt != 9 {
		t.Fatalf("t=%v, want Live's report once the hold expires", tt)
	}
}

func TestStoreGetMarksOverridden(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.putPos(1, false, true, false, 0, 4, false, false)
	if s, _, _ := st.get(); !s.Overridden {
		t.Fatal("bta must mark the set overridden")
	}
	st.putPos(1, false, false, false, 0, 4, false, false)
	if s, _, _ := st.get(); s.Overridden {
		t.Fatal("no bta: crisp")
	}
}

func TestTransportAndPunch(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.putPos(1, false, false, true, 8, 16, true, false)
	on, in, out, start, length := st.transport()
	if !on || !in || out || start != 8 || length != 16 {
		t.Fatalf("transport %v %v %v %v %v", on, in, out, start, length)
	}
	s, _, _ := st.get()
	if !s.Loop.On || s.Loop.Start != 8 || s.Loop.Length != 16 || !s.PunchIn || s.PunchOut {
		t.Fatalf("set %+v", s)
	}
}

func TestAnchorSetClearGet(t *testing.T) {
	st := &store{}
	if _, ok := st.getAnchor(); ok {
		t.Fatal("no anchor by default")
	}
	st.setAnchor(12.5)
	if t2, ok := st.getAnchor(); !ok || t2 != 12.5 {
		t.Fatalf("anchor %v %v", t2, ok)
	}
	st.putSet(&Set{Length: 8})
	if s, _, _ := st.get(); !s.HasAnchor || s.Anchor != 12.5 {
		t.Fatalf("set does not carry the anchor: %+v", s)
	}
	st.clearAnchor()
	if _, ok := st.getAnchor(); ok {
		t.Fatal("anchor must be gone after clear")
	}
}

func TestSelLocator(t *testing.T) {
	st := &store{}
	st.putSet(&Set{Length: 8})
	st.setSelLocator(4)
	s, _, _ := st.get()
	if !s.HasSelLocator || s.SelLocator != 4 {
		t.Fatalf("set %+v", s)
	}
}

func TestDecodeLiveEvents(t *testing.T) {
	snap := `{"t":"event","topic":"arrangement","data":{"tracks":[{"name":"Kick","kind":"midi","color":1,"group":-1,"clips":[[0,8,"A",2]]}],"locators":[],"sig":[4,4],"tempo":120}}`
	m, ok := decodeLive([]byte(snap))
	if !ok || m.T != "snapshot" {
		t.Fatalf("snapshot not decoded: %+v %v", m, ok)
	}
	if s := setFromSnapshot(&m); len(s.Tracks) != 1 || s.Tracks[0].Clips[0].End != 8 {
		t.Fatalf("set %+v", s)
	}
	pos := `{"t":"event","topic":"position","data":{"time":7.5,"playing":true,"bta":false,"loop_on":true,"loop_start":8,"loop_length":16,"punch_in":false,"punch_out":true}}`
	m, ok = decodeLive([]byte(pos))
	if !ok || m.T != "pos" || m.Time != 7.5 || !m.Playing || !m.LoopOn || m.LoopLength != 16 || !m.PunchOut {
		t.Fatalf("pos %+v %v", m, ok)
	}
}

func TestDecodeLiveRepliesAndNoise(t *testing.T) {
	for _, line := range []string{
		`{"ok":true,"id":1,"data":{"protocol":1}}`,
		`{"ok":true,"id":2,"data":{"topics":["arrangement","position"]}}`,
		`{"ok":true}`,                                      // reply to a command
		`{"ok":false,"error":"failed"}`,                    // a failed command must not drop the link
		`{"t":"event","topic":"tempo","data":{"tempo":1}}`, // a topic we did not ask for
		`not json`,
	} {
		if m, ok := decodeLive([]byte(line)); ok {
			t.Errorf("%s: got %+v", line, m)
		}
	}
	m, ok := decodeLive([]byte(`{"ok":false,"id":2,"error":"unknown_topic"}`))
	if !ok || m.T != "refused" || m.Error != "unknown_topic" {
		t.Fatalf("refused subscribe: %+v %v", m, ok)
	}
	if m, ok := decodeLive([]byte(`{"ok":false,"id":1,"error":"x"}`)); !ok || m.T != "refused" {
		t.Fatalf("refused hello: %+v %v", m, ok)
	}
}

func TestRequestMapsEveryCommand(t *testing.T) {
	cases := []struct {
		in      map[string]any
		ns, cmd string
		key     string
		wantVal any
	}{
		{map[string]any{"t": "set_time", "v": 4.5}, "transport", "set_time", "v", 4.5},
		{map[string]any{"t": "set_loop", "start": 8.0, "length": 16.0}, "transport", "set_loop", "length", 16.0},
		{map[string]any{"t": "play_toggle"}, "transport", "play_toggle", "", nil},
		{map[string]any{"t": "bta"}, "arrangement", "bta", "", nil},
		{map[string]any{"t": "loop_toggle"}, "arrangement", "loop_toggle", "", nil},
		{map[string]any{"t": "punch_in_toggle"}, "arrangement", "punch_in_toggle", "", nil},
		{map[string]any{"t": "punch_out_toggle"}, "arrangement", "punch_out_toggle", "", nil},
	}
	for _, c := range cases {
		got, ok := request(c.in)
		if !ok || got["ns"] != c.ns || got["cmd"] != c.cmd {
			t.Errorf("%v -> %v %v", c.in, got, ok)
			continue
		}
		if _, has := got["t"]; has {
			t.Errorf("%v: old key t left in %v", c.in, got)
		}
		if c.key != "" && got[c.key] != c.wantVal {
			t.Errorf("%v: %s = %v", c.in, c.key, got[c.key])
		}
	}
	if _, ok := request(map[string]any{"t": "format_disk"}); ok {
		t.Error("an unknown command was passed on")
	}
}

// A fake PushHack on a socket of its own: the real path is never opened.
func TestRunLiveSourceHandshakeAndStream(t *testing.T) {
	dir, err := os.MkdirTemp("", "ph")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	old := liveSocketPath
	liveSocketPath = filepath.Join(dir, "s.sock")
	defer func() { liveSocketPath = old }()

	ln, err := net.Listen("unix", liveSocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 8)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		rd := bufio.NewReader(c)
		for i := 0; i < 2; i++ {
			l, _ := rd.ReadString('\n')
			got <- l
		}
		c.Write([]byte(`{"t":"event","topic":"arrangement","data":{"tracks":[],"locators":[],"sig":[4,4],"tempo":120}}` + "\n"))
		c.Write([]byte(`{"t":"event","topic":"position","data":{"time":3,"playing":true}}` + "\n"))
		l, _ := rd.ReadString('\n') // a command from the hack
		got <- l
	}()

	st := &store{}
	changed := make(chan struct{}, 8)
	stop := make(chan struct{})
	defer close(stop)
	go runLiveSource(st, func() { changed <- struct{}{} }, stop)

	if l := <-got; !strings.Contains(l, `"hello"`) {
		t.Fatalf("first line %q", l)
	}
	if l := <-got; !strings.Contains(l, `"subscribe"`) || !strings.Contains(l, `"arrangement"`) || !strings.Contains(l, `"position"`) {
		t.Fatalf("second line %q", l)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("no data reached the store")
		}
		if ph, playing := st.pos(); playing && ph == 3 {
			break
		}
	}
	sendCmd(map[string]any{"t": "play_toggle"})
	select {
	case l := <-got:
		if !strings.Contains(l, `"ns":"transport"`) || !strings.Contains(l, `"cmd":"play_toggle"`) {
			t.Fatalf("command line %q", l)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("command never arrived")
	}
}

func TestCallLiveGetsItsReply(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	link.set(a)
	defer link.set(nil)
	go func() {
		rd := bufio.NewReader(b)
		line, _ := rd.ReadBytes('\n')
		var req map[string]any
		_ = json.Unmarshal(line, &req)
		id := int(req["id"].(float64))
		if req["ns"] != "arrangement" || req["cmd"] != "clip_delete" || req["track"].(float64) != 2 {
			b.Write([]byte(`{"id":` + strconv.Itoa(id) + `,"ok":false,"error":"wrong_request"}` + "\n"))
			return
		}
		b.Write([]byte(`{"id":` + strconv.Itoa(id) + `,"ok":true,"data":{"x":1}}` + "\n"))
	}()
	st := &store{}
	stop := make(chan struct{})
	defer close(stop)
	go readLive(a, st, func() {}, stop)
	got := make(chan string, 1)
	callLive("arrangement", "clip_delete", map[string]any{"track": 2, "start": 4.0}, func(d json.RawMessage, code string) {
		got <- code + string(d)
	})
	select {
	case g := <-got:
		if g != `{"x":1}` {
			t.Fatalf("reply %q", g)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no reply")
	}
}

func TestCallLiveWithoutConnection(t *testing.T) {
	link.set(nil)
	got := make(chan string, 1)
	callLive("transport", "undo", nil, func(_ json.RawMessage, code string) { got <- code })
	if g := <-got; g != "not_connected" {
		t.Fatalf("%q", g)
	}
}

func TestScriptVersionFromHello(t *testing.T) {
	defer scriptVersion.Store("")
	decodeLive([]byte(`{"ok":true,"id":1,"data":{"protocol":1,"script_version":"0.6.0"}}`))
	if !scriptAtLeast(0, 6) || scriptAtLeast(0, 7) || scriptAtLeast(1, 0) {
		t.Fatal("version compare")
	}
}

func TestSnapshotReadsMutedClips(t *testing.T) {
	m, ok := decodeLive([]byte(`{"t":"event","topic":"arrangement","data":{"tracks":[{"name":"A","kind":"midi","color":1,"group":-1,
	  "clips":[[0,4,"a",5,1],[4,8,"b",5,0],[8,9,"old",5]]}],"locators":[],"sig":[4,4],"tempo":98}}`))
	if !ok {
		t.Fatal("not decoded")
	}
	s := setFromSnapshot(&m)
	c := s.Tracks[0].Clips
	if !c[0].Muted || c[1].Muted || c[2].Muted || s.Tempo != 98 {
		t.Fatalf("%+v tempo %v", c, s.Tempo)
	}
}

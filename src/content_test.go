package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestClipInfoDecodes(t *testing.T) {
	raw := `{"kind":"midi","start":4,"end":12,"start_marker":0,"end_marker":8,"loop_start":0,"loop_end":4,
	  "looping":true,"notes":[[60,0,1,100,0],[67,2.5,0.5,90,1]],"color_index":7}`
	var ci ClipInfo
	if err := json.Unmarshal([]byte(raw), &ci); err != nil {
		t.Fatal(err)
	}
	ci.prepare()
	if len(ci.Notes) != 2 || ci.Notes[1] != (Note{67, 2.5, 0.5, 90, true}) {
		t.Fatalf("notes %+v", ci.Notes)
	}
	if ci.pitchLo != 60 || ci.pitchHi != 67 || !ci.Looping || ci.ColorIndex != 7 {
		t.Fatalf("%+v", ci)
	}
	var a ClipInfo
	_ = json.Unmarshal([]byte(`{"kind":"audio","warping":true,"warp_markers":[[4,2],[0,0]]}`), &a)
	a.prepare()
	if a.Warp[0] != (WarpMarker{0, 0}) || a.Warp[1] != (WarpMarker{4, 2}) {
		t.Fatalf("warp markers not sorted: %+v", a.Warp)
	}
}

func TestSegmentsUnlooped(t *testing.T) {
	ci := &ClipInfo{Kind: "midi", StartMarker: 2}
	got := segments(ci, 8, 1)
	if len(got) != 1 || got[0] != (seg{0, 8, 2}) {
		t.Fatalf("%+v", got)
	}
}

func TestSegmentsLoopedFromAStartMarkerInsideTheLoop(t *testing.T) {
	// loop 0..4, start marker at 1, clip 10 beats long: 1..4, then 0..4, 0..4 (cut at 10)
	ci := &ClipInfo{Kind: "midi", Looping: true, StartMarker: 1, LoopStart: 0, LoopEnd: 4}
	got := segments(ci, 10, 1)
	want := []seg{{0, 3, 1}, {3, 7, 0}, {7, 10, 0}}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seg %d: %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestSegmentsUnwarpedAudioUsesSeconds(t *testing.T) {
	// 120 BPM: 0.5 s per beat. Loop 0..2 s = 4 beats.
	ci := &ClipInfo{Kind: "audio", Looping: true, LoopStart: 0, LoopEnd: 2}
	upb := ci.unitsPerBeat(120)
	if upb != 0.5 {
		t.Fatalf("upb %v", upb)
	}
	got := segments(ci, 8, upb)
	if len(got) != 2 || got[1] != (seg{4, 8, 0}) {
		t.Fatalf("%+v", got)
	}
}

func TestSegmentsCapATinyLoop(t *testing.T) {
	ci := &ClipInfo{Kind: "midi", Looping: true, LoopStart: 0, LoopEnd: 1e-4}
	if n := len(segments(ci, 1e6, 1)); n > maxSegs+1 {
		t.Fatalf("%d segments", n)
	}
}

func TestFileSecondsThroughWarpMarkers(t *testing.T) {
	ci := &ClipInfo{Kind: "audio", Warping: true, Warp: []WarpMarker{{0, 0}, {4, 2}, {8, 3}}}
	for _, c := range []struct{ beat, sec float64 }{{0, 0}, {2, 1}, {4, 2}, {6, 2.5}, {10, 3.5}, {-2, -1}} {
		if got := fileSeconds(ci, c.beat, 120); math.Abs(got-c.sec) > 1e-9 {
			t.Errorf("beat %v: %v want %v", c.beat, got, c.sec)
		}
	}
	unwarped := &ClipInfo{Kind: "audio"}
	if fileSeconds(unwarped, 1.25, 90) != 1.25 {
		t.Error("unwarped clip time is already seconds")
	}
}

// fakeLive answers clips_info calls from a table.
type fakeLive struct {
	mu    sync.Mutex
	calls int
	clips map[clipKey]*ClipInfo
	err   string
}

func (f *fakeLive) call(ns, cmd string, args map[string]any, cb func(json.RawMessage, string)) {
	f.mu.Lock()
	f.calls++
	if f.err != "" {
		e := f.err
		f.mu.Unlock()
		cb(nil, e)
		return
	}
	var out []*ClipInfo
	for _, r := range args["clips"].([]map[string]any) {
		out = append(out, f.clips[keyOf(r["track"].(int), r["start"].(float64))])
	}
	f.mu.Unlock()
	b, _ := json.Marshal(map[string]any{"clips": out})
	cb(b, "")
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestContentCacheFetchesVisibleClipsOnce(t *testing.T) {
	f := &fakeLive{clips: map[clipKey]*ClipInfo{
		keyOf(0, 0): {Kind: "midi", Start: 0, End: 4, Notes: []Note{{Pitch: 60, Dur: 1}}},
		keyOf(1, 8): {Kind: "audio", Start: 8, End: 12, File: "/nope.wav"},
	}}
	cc := newContentCache(f.call, func() {})
	c0, c1 := &Clip{Start: 0, End: 4}, &Clip{Start: 8, End: 12}
	if cc.lookup(0, c0) != nil {
		t.Fatal("content before any fetch")
	}
	cc.lookup(1, c1)
	waitFor(t, func() bool { return cc.lookup(0, c0) != nil && cc.lookup(1, c1) != nil })
	if got := cc.lookup(0, c0); got.pitchHi != 60 {
		t.Fatalf("not prepared: %+v", got)
	}
	f.mu.Lock()
	n := f.calls
	f.mu.Unlock()
	for i := 0; i < 20; i++ {
		cc.lookup(0, c0)
	}
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != n {
		t.Fatalf("fresh entries fetched again: %d calls, was %d", f.calls, n)
	}
}

func TestContentCacheRefetchesWhenTheClipChanges(t *testing.T) {
	f := &fakeLive{clips: map[clipKey]*ClipInfo{keyOf(0, 0): {Kind: "midi", Start: 0, End: 4}}}
	cc := newContentCache(f.call, func() {})
	c := &Clip{Start: 0, End: 4}
	cc.lookup(0, c)
	waitFor(t, func() bool { return cc.info(0, 0) != nil })
	f.mu.Lock()
	f.clips[keyOf(0, 0)] = &ClipInfo{Kind: "midi", Start: 0, End: 6}
	f.mu.Unlock()
	c.End = 6
	cc.lookup(0, c)
	waitFor(t, func() bool { i := cc.info(0, 0); return i != nil && i.End == 6 })
}

func TestContentCacheStopsOnAnOldScript(t *testing.T) {
	f := &fakeLive{err: "unknown_cmd"}
	cc := newContentCache(f.call, func() {})
	cc.lookup(0, &Clip{Start: 0, End: 4})
	waitFor(t, func() bool { cc.mu.Lock(); defer cc.mu.Unlock(); return cc.disabled })
	f.mu.Lock()
	n := f.calls
	f.mu.Unlock()
	cc.now = func() time.Time { return time.Now().Add(time.Minute) }
	cc.lookup(0, &Clip{Start: 0, End: 4})
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls != n {
		t.Fatal("kept asking an old script")
	}
}

// ---- audio files ----

func writeWAV(t *testing.T, path string, tag uint16, bits, ch int, frames [][]float64) {
	t.Helper()
	var data bytes.Buffer
	for _, fr := range frames {
		for _, v := range fr {
			switch {
			case tag == 3:
				_ = binary.Write(&data, binary.LittleEndian, float32(v))
			case bits == 16:
				_ = binary.Write(&data, binary.LittleEndian, int16(v*32767))
			case bits == 24:
				x := int32(v * 8388607)
				data.Write([]byte{byte(x), byte(x >> 8), byte(x >> 16)})
			}
		}
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(4+8+16+8+data.Len()+8+2))
	b.WriteString("WAVE")
	b.WriteString("junk") // a chunk to skip, odd size + pad byte
	_ = binary.Write(&b, binary.LittleEndian, uint32(1))
	b.Write([]byte{0, 0})
	b.WriteString("fmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	bps := bits / 8
	for _, v := range []any{tag, uint16(ch), uint32(48000), uint32(48000 * ch * bps), uint16(ch * bps), uint16(bits)} {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(data.Len()))
	b.Write(data.Bytes())
	if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ramp: block 0 quiet, block 1 loud on channel 2 only.
func ramp(ch int) [][]float64 {
	var out [][]float64
	for i := 0; i < peakBlock*2+10; i++ {
		fr := make([]float64, ch)
		if i >= peakBlock && i < 2*peakBlock {
			fr[ch-1] = 0.9
			if i%2 == 0 {
				fr[ch-1] = -0.5
			}
		} else {
			fr[0] = 0.1
		}
		out = append(out, fr)
	}
	return out
}

func checkRamp(t *testing.T, p *peaks, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if p.rate != 48000 || p.frames != peakBlock*2+10 || len(p.lo) != 3 {
		t.Fatalf("rate %v frames %d blocks %d", p.rate, p.frames, len(p.lo))
	}
	if p.hi[0] > 15 || p.hi[1] < 110 || p.lo[1] > -60 {
		t.Fatalf("peaks lo %v hi %v", p.lo, p.hi)
	}
	secPerBlock := float64(peakBlock) / 48000
	lo, hi, ok := p.span(secPerBlock*1.2, secPerBlock*1.5)
	if !ok || hi < 110 || lo > -60 {
		t.Fatalf("span: %v %v %v", lo, hi, ok)
	}
	if _, _, ok := p.span(10, 11); ok {
		t.Fatal("span past the end")
	}
}

func TestReadPeaksWAV(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		tag  uint16
		bits int
	}{{1, 16}, {1, 24}, {3, 32}} {
		p := filepath.Join(dir, "a.wav")
		writeWAV(t, p, c.tag, c.bits, 2, ramp(2))
		pk, err := readPeaks(p)
		checkRamp(t, pk, err)
	}
}

func TestReadPeaksAIFF(t *testing.T) {
	frames := ramp(1)
	var data bytes.Buffer
	for _, fr := range frames {
		_ = binary.Write(&data, binary.BigEndian, int16(fr[0]*32767))
	}
	var comm bytes.Buffer
	_ = binary.Write(&comm, binary.BigEndian, uint16(1))
	_ = binary.Write(&comm, binary.BigEndian, uint32(len(frames)))
	_ = binary.Write(&comm, binary.BigEndian, uint16(16))
	comm.Write([]byte{0x40, 0x0E, 0xBB, 0x80, 0, 0, 0, 0, 0, 0}) // 48000 as 80-bit float
	var b bytes.Buffer
	b.WriteString("FORM")
	_ = binary.Write(&b, binary.BigEndian, uint32(4+8+comm.Len()+8+8+data.Len()))
	b.WriteString("AIFF")
	b.WriteString("COMM")
	_ = binary.Write(&b, binary.BigEndian, uint32(comm.Len()))
	b.Write(comm.Bytes())
	b.WriteString("SSND")
	_ = binary.Write(&b, binary.BigEndian, uint32(8+data.Len()))
	b.Write(make([]byte, 8))
	b.Write(data.Bytes())
	p := filepath.Join(t.TempDir(), "a.aif")
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	pk, err := readPeaks(p)
	checkRamp(t, pk, err)
}

func TestReadPeaksRefusesOtherFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.mp3")
	_ = os.WriteFile(p, []byte("ID3\x04\x00\x00\x00\x00\x00\x00 not audio we read"), 0o644)
	if _, err := readPeaks(p); err == nil {
		t.Fatal("read an mp3")
	}
	if _, err := readPeaks(filepath.Join(t.TempDir(), "missing.wav")); err == nil {
		t.Fatal("read a missing file")
	}
}

func TestPeakCacheLoadsInTheBackground(t *testing.T) {
	var mu sync.Mutex
	redraws := 0
	pc := newPeakCache(func() { mu.Lock(); redraws++; mu.Unlock() })
	pc.read = func(string) (*peaks, error) { return &peaks{rate: 1, lo: []int8{-1}, hi: []int8{1}}, nil }
	if pc.get("/a.wav") != nil {
		t.Fatal("loaded at once")
	}
	waitFor(t, func() bool { return pc.get("/a.wav") != nil })
	mu.Lock()
	defer mu.Unlock()
	if redraws == 0 {
		t.Fatal("no redraw after load")
	}
}

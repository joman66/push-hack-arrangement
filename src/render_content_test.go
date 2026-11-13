package main

import (
	"encoding/json"
	"image"
	"image/png"
	"math"
	"os"
	"testing"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
)

// demoContent: a set with a looped MIDI clip, a drum clip, a warped audio
// clip and a muted clip, with their content already in the cache.
func demoContent() (*Set, *contentCache) {
	s := &Set{BeatsPerBar: 4, Tempo: 120, Length: 64, Tracks: []Track{
		{Name: "Keys", Kind: "midi", Color: 0x3FA9F5, GroupID: -1, ID: 0, Clips: []Clip{
			{Start: 0, End: 16, Name: "Chords", Color: 0x3FA9F5},
			{Start: 32, End: 48, Name: "Off", Color: 0x3FA9F5, Muted: true}}},
		{Name: "Drums", Kind: "midi", Color: 0xF5A623, GroupID: -1, ID: 1, Clips: []Clip{
			{Start: 0, End: 32, Name: "Beat", Color: 0xF5A623}}},
		{Name: "Vox", Kind: "audio", Color: 0x7ED321, GroupID: -1, ID: 2, Clips: []Clip{
			{Start: 8, End: 40, Name: "vox.wav", Color: 0x2D5016}}},
	}}
	cc := newContentCache(func(string, string, map[string]any, func(json.RawMessage, string)) {}, func() {})
	now := time.Now().Add(time.Hour) // never due for a refetch in the test
	put := func(track int, ci *ClipInfo) {
		ci.prepare()
		cc.m[keyOf(track, ci.Start)] = &contentEntry{info: ci, end: ci.End, fetched: now, seen: now}
	}
	chords := &ClipInfo{Kind: "midi", Start: 0, End: 16, Looping: true, LoopStart: 0, LoopEnd: 8}
	for bar, root := range []int{60, 65, 67, 64} {
		for _, iv := range []int{0, 4, 7} {
			chords.Notes = append(chords.Notes, Note{Pitch: root + iv, Start: float64(bar * 2), Dur: 1.75, Vel: 100})
		}
	}
	put(0, chords)
	put(0, &ClipInfo{Kind: "midi", Start: 32, End: 48, Notes: []Note{{Pitch: 60, Start: 0, Dur: 4, Vel: 90}}})
	beat := &ClipInfo{Kind: "midi", Start: 0, End: 32, Looping: true, LoopStart: 0, LoopEnd: 4}
	for i := 0; i < 16; i++ {
		beat.Notes = append(beat.Notes, Note{Pitch: 42, Start: float64(i) / 4, Dur: 0.125, Vel: 80, Mute: i%4 == 3})
		if i%4 == 0 {
			beat.Notes = append(beat.Notes, Note{Pitch: 36, Start: float64(i) / 4, Dur: 0.25, Vel: 120})
		}
		if i%8 == 4 {
			beat.Notes = append(beat.Notes, Note{Pitch: 38, Start: float64(i) / 4, Dur: 0.25, Vel: 110})
		}
	}
	sortNotes(beat.Notes)
	put(1, beat)
	put(2, &ClipInfo{Kind: "audio", Start: 8, End: 40, File: "/demo/vox.wav", Warping: true,
		Warp: []WarpMarker{{0, 0}, {32, 16}}})

	// 16 s of a decaying tone every second, 1000 frames/s
	p := &peaks{rate: 1000}
	for i := 0; i < 16000/peakBlock; i++ {
		t := float64(i*peakBlock) / 1000
		a := math.Exp(-3*(t-math.Floor(t))) * (0.4 + 0.5*math.Abs(math.Sin(t/3)))
		p.lo, p.hi = append(p.lo, toInt8(-a)), append(p.hi, toInt8(a*0.9))
	}
	p.frames = int64(len(p.lo) * peakBlock)
	cc.peaks.m["/demo/vox.wav"] = &peakEntry{p: p, used: time.Now()}
	return s, cc
}

func sortNotes(ns []Note) {
	for i := 1; i < len(ns); i++ {
		for j := i; j > 0 && ns[j].Start < ns[j-1].Start; j-- {
			ns[j], ns[j-1] = ns[j-1], ns[j]
		}
	}
}

func demoView(s *Set, cc *contentCache, edit bool) viewport {
	v := viewport{ppb: 14, laneH: 46, gutter: gutterW, content: cc}
	if edit {
		v.selTrack, v.selTrackOn = 1, true
		v.sel = &clipBox{track: 1, start: 0, end: 32}
		v.ghost = &clipBox{track: 1, start: 4, end: 36}
		soft := [8]widgets.SoftButton{{Label: "Split"}, {Label: "Duplicate"}, {Label: "Delete"}, {Label: "Mute"},
			{Label: "Quantize"}, {Label: "Crop"}, {Label: "Copy"}, {Label: "Paste"}}
		v.soft = &soft
	}
	return v
}

func TestRenderDrawsNotesAndWaveform(t *testing.T) {
	s, cc := demoContent()
	img := renderArrangement(s, demoView(s, cc, false))
	plain := renderArrangement(s, viewport{ppb: 14, laneH: 46, gutter: gutterW})
	// inside each clip's content area, the content version differs from a plain clip
	for _, pt := range []struct {
		name string
		x0   int
		y    int
	}{
		{"chords", gutterW + 2, rulerH + 1 + clipHeaderH},
		{"drums", gutterW + 2, rulerH + 1 + 46 + clipHeaderH},
		{"vox", gutterW + 8*14 + 2, rulerH + 1 + 92 + clipHeaderH},
	} {
		if !regionDiffers(img, plain, pt.x0, pt.y, 100, 30) {
			t.Errorf("%s: no content drawn", pt.name)
		}
	}
	if out := os.Getenv("PREVIEW_OUT"); out != "" {
		writePNG(t, out, img)
		writePNG(t, out+".edit.png", renderArrangement(s, demoView(s, cc, true)))
		st := &store{}
		st.putSet(s)
		st.setCursor(2)
		vc := newViewCtl(func() *Set { return s })
		e := newEditCtl(st, vc, cc, func() {}, func() bool { return false })
		e.on, e.track = true, 1
		v := demoView(s, cc, true)
		v.ghost = nil
		v.overlay, _ = e.touchText(s)
		writePNG(t, out+".header.png", renderArrangement(s, v))
	}
}

func regionDiffers(a, b *image.NRGBA, x, y, w, h int) bool {
	for yy := y; yy < y+h && yy < screenH; yy++ {
		for xx := x; xx < x+w && xx < screenW; xx++ {
			if a.NRGBAAt(xx, yy) != b.NRGBAAt(xx, yy) {
				return true
			}
		}
	}
	return false
}

func writePNG(t *testing.T, path string, img image.Image) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestRenderWithoutContentCacheStillWorks(t *testing.T) {
	s, _ := demoContent()
	renderArrangement(s, viewport{ppb: 14, laneH: 46, gutter: gutterW})
}

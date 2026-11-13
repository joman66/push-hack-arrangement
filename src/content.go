package main

// Clip content: MIDI notes and audio file info of arrangement clips, fetched
// from PushHack (arrangement.clips_info) for the clips on screen only. The
// renderer asks lookup() for each visible clip; misses and old entries are
// fetched in the background, 8 clips per call, one call at a time. A reply
// asks for a redraw.

import (
	"encoding/json"
	"log"
	"math"
	"sort"
	"sync"
	"time"
)

// Note: one MIDI note. Start/Dur in clip beats (same scale as the markers).
type Note struct {
	Pitch      int
	Start, Dur float64
	Vel        int
	Mute       bool
}

func (n *Note) UnmarshalJSON(b []byte) error {
	var a []float64
	if err := json.Unmarshal(b, &a); err != nil || len(a) < 4 {
		return err
	}
	n.Pitch, n.Start, n.Dur, n.Vel = int(a[0]), a[1], a[2], int(a[3])
	n.Mute = len(a) > 4 && a[4] != 0
	return nil
}

func (n Note) MarshalJSON() ([]byte, error) {
	m := 0
	if n.Mute {
		m = 1
	}
	return json.Marshal([]float64{float64(n.Pitch), n.Start, n.Dur, float64(n.Vel), float64(m)})
}

// WarpMarker: beat in the clip <-> seconds in the file.
type WarpMarker struct{ Beat, Sec float64 }

func (w *WarpMarker) UnmarshalJSON(b []byte) error {
	var a []float64
	if err := json.Unmarshal(b, &a); err != nil || len(a) < 2 {
		return err
	}
	w.Beat, w.Sec = a[0], a[1]
	return nil
}

func (w WarpMarker) MarshalJSON() ([]byte, error) { return json.Marshal([]float64{w.Beat, w.Sec}) }

// ClipInfo: what clips_info sends for one clip.
type ClipInfo struct {
	Kind        string  `json:"kind"`
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	StartMarker float64 `json:"start_marker"`
	EndMarker   float64 `json:"end_marker"`
	LoopStart   float64 `json:"loop_start"`
	LoopEnd     float64 `json:"loop_end"`
	Looping     bool    `json:"looping"`
	Muted       bool    `json:"muted"`
	ColorIndex  int     `json:"color_index"`

	Notes      []Note `json:"notes"`
	NotesTotal int    `json:"notes_total"`

	File         string       `json:"file"`
	Warping      bool         `json:"warping"`
	Warp         []WarpMarker `json:"warp_markers"`
	SampleRate   float64      `json:"sample_rate"`
	SampleLength int          `json:"sample_length"`
	Gain         float64      `json:"gain"`
	PitchCoarse  int          `json:"pitch_coarse"`

	pitchLo, pitchHi int // note range, filled once on arrival
}

func (ci *ClipInfo) isMIDI() bool { return ci.Kind == "midi" }

// unitsPerBeat: markers are in seconds for unwarped audio, else in beats.
func (ci *ClipInfo) unitsPerBeat(tempo float64) float64 {
	if ci.isMIDI() || ci.Warping {
		return 1
	}
	if tempo <= 0 {
		tempo = 120
	}
	return 60 / tempo
}

func (ci *ClipInfo) prepare() {
	ci.pitchLo, ci.pitchHi = 128, -1
	for _, n := range ci.Notes {
		if n.Pitch < ci.pitchLo {
			ci.pitchLo = n.Pitch
		}
		if n.Pitch > ci.pitchHi {
			ci.pitchHi = n.Pitch
		}
	}
	sort.Slice(ci.Warp, func(i, j int) bool { return ci.Warp[i].Beat < ci.Warp[j].Beat })
}

// seg: a piece of the clip in the arrangement. Arrangement offset p (beats
// from the clip's left edge) in [p0, p1) plays clip time c0 + (p-p0)*upb.
type seg struct {
	p0, p1, c0 float64
}

const maxSegs = 4096

// segments: how clip time maps onto the clip's length in the arrangement.
// Not looped: one piece from the start marker. Looped: from the start marker
// to the loop end, then the loop again and again.
func segments(ci *ClipInfo, length, upb float64) []seg {
	if length <= 0 || upb <= 0 {
		return nil
	}
	if !ci.Looping {
		return []seg{{0, length, ci.StartMarker}}
	}
	loopLen := (ci.LoopEnd - ci.LoopStart) / upb
	if loopLen <= 1e-6 {
		return []seg{{0, length, ci.StartMarker}}
	}
	var out []seg
	p := 0.0
	c := ci.StartMarker
	if c >= ci.LoopEnd || c < ci.LoopStart-1e9 {
		c = ci.LoopStart
	}
	first := (ci.LoopEnd - c) / upb
	if first > 1e-9 {
		out = append(out, seg{0, math.Min(first, length), c})
		p = first
	}
	for p < length && len(out) < maxSegs {
		out = append(out, seg{p, math.Min(p+loopLen, length), ci.LoopStart})
		p += loopLen
	}
	return out
}

// fileSeconds: clip time (beats if warped, seconds if not) -> seconds in the
// audio file, through the warp markers. Outside the markers the first/last
// piece's rate goes on.
func fileSeconds(ci *ClipInfo, ct, tempo float64) float64 {
	if !ci.Warping {
		return ct
	}
	w := ci.Warp
	spb := 60 / math.Max(tempo, 1) // fallback rate
	switch len(w) {
	case 0:
		return ct * spb
	case 1:
		return w[0].Sec + (ct-w[0].Beat)*spb
	}
	i := sort.Search(len(w), func(i int) bool { return w[i].Beat > ct })
	switch {
	case i <= 0:
		i = 1
	case i >= len(w):
		i = len(w) - 1
	}
	a, b := w[i-1], w[i]
	if b.Beat-a.Beat < 1e-9 {
		return a.Sec
	}
	return a.Sec + (ct-a.Beat)*(b.Sec-a.Sec)/(b.Beat-a.Beat)
}

// ---- cache ----

type clipKey struct {
	track int
	start int64 // beats * 1000
}

func keyOf(track int, start float64) clipKey {
	return clipKey{track, int64(math.Round(start * 1000))}
}

type contentEntry struct {
	info    *ClipInfo
	end     float64   // clip end when fetched: a new end means new content
	fetched time.Time // zero = fetch again
	seen    time.Time
	pending bool
}

const (
	contentRefresh = 4 * time.Second  // visible clips are fetched again this often (edits made in Live)
	contentForget  = 30 * time.Second // not drawn this long: drop
	contentBatch   = 8                // clips per clips_info call (PushHack's limit)
)

type contentCache struct {
	mu       sync.Mutex
	m        map[clipKey]*contentEntry
	busy     bool
	disabled bool // PushHack too old for clips_info

	call    func(ns, cmd string, args map[string]any, cb func(json.RawMessage, string))
	changed func()
	peaks   *peakCache
	now     func() time.Time
}

func newContentCache(call func(string, string, map[string]any, func(json.RawMessage, string)), changed func()) *contentCache {
	cc := &contentCache{m: map[clipKey]*contentEntry{}, call: call, changed: changed, now: time.Now}
	cc.peaks = newPeakCache(changed)
	return cc
}

// lookup: content of clip c on track (nil while not fetched). Called from the
// renderer; a miss or an old entry starts a fetch.
func (cc *contentCache) lookup(track int, c *Clip) *ClipInfo {
	if cc == nil {
		return nil
	}
	k := keyOf(track, c.Start)
	now := cc.now()
	cc.mu.Lock()
	e := cc.m[k]
	if e == nil {
		e = &contentEntry{}
		cc.m[k] = e
	}
	e.seen = now
	if math.Abs(e.end-c.End) > 1e-3 {
		e.fetched = time.Time{} // the clip changed: fetch again, keep drawing the old one
	}
	info := e.info
	need := !e.pending && now.Sub(e.fetched) > contentRefresh
	start := need && !cc.busy && !cc.disabled
	if start {
		cc.busy = true
	}
	cc.mu.Unlock()
	if start {
		go cc.fetch()
	}
	return info
}

// invalidate: fetch the clips of these tracks again (after an edit).
func (cc *contentCache) invalidate(tracks ...int) {
	if cc == nil {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	for k, e := range cc.m {
		for _, t := range tracks {
			if k.track == t {
				e.fetched = time.Time{}
			}
		}
	}
}

// reset: a new connection or a different set; drop everything.
func (cc *contentCache) reset() {
	cc.mu.Lock()
	cc.m = map[clipKey]*contentEntry{}
	cc.disabled = false
	cc.mu.Unlock()
}

// next picks up to contentBatch clips to fetch: seen recently, never fetched
// or too old. Missing ones first. Drops entries not drawn for a while.
func (cc *contentCache) next() []clipKey {
	now := cc.now()
	var miss, old []clipKey
	for k, e := range cc.m {
		if now.Sub(e.seen) > contentForget {
			delete(cc.m, k)
			continue
		}
		if e.pending || now.Sub(e.seen) > time.Second || now.Sub(e.fetched) <= contentRefresh {
			continue
		}
		if e.info == nil {
			miss = append(miss, k)
		} else {
			old = append(old, k)
		}
	}
	sortKeys(miss)
	sortKeys(old)
	out := append(miss, old...)
	if len(out) > contentBatch {
		out = out[:contentBatch]
	}
	for _, k := range out {
		cc.m[k].pending = true
	}
	return out
}

func sortKeys(ks []clipKey) {
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].track != ks[j].track {
			return ks[i].track < ks[j].track
		}
		return ks[i].start < ks[j].start
	})
}

// fetch runs one clips_info call, then the next, until nothing is due.
func (cc *contentCache) fetch() {
	cc.mu.Lock()
	keys := cc.next()
	if len(keys) == 0 {
		cc.busy = false
		cc.mu.Unlock()
		return
	}
	cc.mu.Unlock()

	req := make([]map[string]any, len(keys))
	for i, k := range keys {
		req[i] = map[string]any{"track": k.track, "start": float64(k.start) / 1000}
	}
	cc.call("arrangement", "clips_info", map[string]any{"clips": req}, func(data json.RawMessage, errCode string) {
		var reply struct {
			Clips []*ClipInfo `json:"clips"`
		}
		if errCode == "" {
			if err := json.Unmarshal(data, &reply); err != nil {
				log.Printf("clips_info: %v", err)
			}
		}
		now := cc.now()
		cc.mu.Lock()
		if errCode == "unknown_cmd" {
			cc.disabled = true // PushHack before 0.6.0
			log.Print("clips_info: PushHack is too old (needs 0.6.0); no clip content")
		}
		for i, k := range keys {
			e := cc.m[k]
			if e == nil {
				continue
			}
			e.pending = false
			if errCode != "" {
				e.fetched = now.Add(-contentRefresh + time.Second) // try again in a second
				continue
			}
			e.fetched = now
			if i < len(reply.Clips) && reply.Clips[i] != nil {
				ci := reply.Clips[i]
				ci.prepare()
				e.info, e.end = ci, ci.End
			} else {
				e.info = nil
			}
		}
		cc.busy = false
		more := !cc.disabled && errCode == ""
		if more {
			cc.busy = true
		}
		cc.mu.Unlock()
		if errCode == "" {
			cc.changed()
		}
		if more {
			cc.fetch()
		}
	})
}

// info returns what is cached for a clip, without asking for a fetch.
func (cc *contentCache) info(track int, start float64) *ClipInfo {
	if cc == nil {
		return nil
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if e := cc.m[keyOf(track, start)]; e != nil {
		return e.info
	}
	return nil
}

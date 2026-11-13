package main

// What is inside a clip: MIDI notes as small bars (pitch range of the clip
// fills the lane), audio as a min/max waveform. Both follow the clip's loop,
// and audio its warp markers, so the picture lines up with what plays.

import (
	"image"
	"image/color"
	"math"
	"sort"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
)

const (
	clipHeaderH    = 12 // clip name strip when names show
	minContentH    = 4  // less than this: no notes, no waveform
	minPitchRows   = 8  // a narrow note range does not make fat notes
	minSegPxForNts = 2  // loop repeats thinner than this: stop drawing notes
)

// box draws a rectangle clipped to the clip area: x in [minX, maxX), y >= minY.
func box(img *image.NRGBA, x, y, w, h, minX, maxX, minY int, c color.NRGBA) {
	if x+w > maxX {
		w = maxX - x
	}
	fill(img, x, y, w, h, minX, minY, c)
}

// inkFor: the colour content is drawn in, dark on a light clip, light on a dark one.
func inkFor(col color.NRGBA) color.NRGBA {
	if luma(col) > 100 {
		return color.NRGBA{uint8(int(col.R) * 30 / 100), uint8(int(col.G) * 30 / 100), uint8(int(col.B) * 30 / 100), 255}
	}
	mix := func(a uint8) uint8 { return uint8(int(a) + (255-int(a))*65/100) }
	return color.NRGBA{mix(col.R), mix(col.G), mix(col.B), 255}
}

func blend(a, b color.NRGBA, t float64) color.NRGBA {
	m := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t) }
	return color.NRGBA{m(a.R, b.R), m(a.G, b.G), m(a.B, b.B), 255}
}

// drawContent draws notes or the waveform of clip c (track i) in its lane.
func drawContent(img *image.NRGBA, s *Set, v viewport, i int, c *Clip, x0, x1, y0, h int, named bool, col color.NRGBA) {
	info := v.content.lookup(i, c)
	if info == nil {
		return
	}
	top := rulerH + 1
	cy, ch := y0, h
	if named && h-clipHeaderH-1 >= minContentH {
		cy, ch = y0+clipHeaderH, h-clipHeaderH-1
	}
	if ch < minContentH {
		return
	}
	xa, xb := x0, x1
	if xa < v.gutter {
		xa = v.gutter
	}
	if xb > screenW {
		xb = screenW
	}
	ink := inkFor(col)
	if info.isMIDI() {
		drawNotes(img, info, v, c.Start, c.End-c.Start, xa, xb, cy, ch, top, ink, col)
		return
	}
	if p := v.content.peaks.get(info.File); p != nil {
		drawWave(img, info, p, v, s.Tempo, c.Start, c.End-c.Start, xa, xb, cy, ch, top, ink)
	}
}

func drawNotes(img *image.NRGBA, ci *ClipInfo, v viewport, clipStart, length float64,
	xa, xb, cy, ch, top int, ink, col color.NRGBA) {
	if ci.pitchHi < ci.pitchLo || len(ci.Notes) == 0 {
		return
	}
	lo, hi := ci.pitchLo, ci.pitchHi
	if span := hi - lo + 1; span < minPitchRows {
		lo -= (minPitchRows - span) / 2
		hi = lo + minPitchRows - 1
	}
	rowH := float64(ch) / float64(hi-lo+1)
	noteH := int(rowH)
	if rowH >= 3 {
		noteH = int(rowH) - 1 // a gap between neighbour pitches
	}
	if noteH < 1 {
		noteH = 1
	}
	muted := blend(col, ink, 0.4)
	notes := ci.Notes
	for _, sg := range segments(ci, length, 1) {
		sx0, sx1 := v.x(clipStart+sg.p0), v.x(clipStart+sg.p1)
		if sx1 < xa {
			continue
		}
		if sx0 >= xb {
			break
		}
		if sg.p0 > 0 && sx1-sx0 < minSegPxForNts {
			break // repeats too thin to read
		}
		segLen := sg.p1 - sg.p0
		k := sort.Search(len(notes), func(k int) bool { return notes[k].Start >= sg.c0-1e-9 })
		for ; k < len(notes) && notes[k].Start < sg.c0+segLen; k++ {
			n := notes[k]
			a := clipStart + sg.p0 + (n.Start - sg.c0)
			b := math.Min(a+n.Dur, clipStart+sg.p1)
			nx0, nx1 := v.x(a), v.x(b)
			if nx1 < xa || nx0 >= xb {
				continue
			}
			w := nx1 - nx0
			if w < 1 {
				w = 1
			}
			ny := cy + int(float64(hi-n.Pitch)*rowH)
			c := ink
			if n.Mute {
				c = muted
			}
			if nx0 < xa {
				w -= xa - nx0
				nx0 = xa
			}
			box(img, nx0, ny, w, noteH, xa, xb, top, c)
		}
	}
}

func drawWave(img *image.NRGBA, ci *ClipInfo, p *peaks, v viewport, tempo, clipStart, length float64,
	xa, xb, cy, ch, top int, ink color.NRGBA) {
	upb := ci.unitsPerBeat(tempo)
	segs := segments(ci, length, upb)
	if len(segs) == 0 {
		return
	}
	mid := cy + ch/2
	amp := float64(ch-1) / 2
	beatPerPx := 1 / v.ppb
	for x := xa; x < xb; x++ {
		pa := v.x0 + float64(x-v.gutter)*beatPerPx - clipStart
		if pa < 0 || pa >= length {
			continue
		}
		pb := math.Min(pa+beatPerPx, length)
		k := sort.Search(len(segs), func(k int) bool { return segs[k].p1 > pa })
		if k >= len(segs) {
			continue
		}
		sg := segs[k]
		ca := sg.c0 + (pa-sg.p0)*upb
		cb := sg.c0 + (math.Min(pb, sg.p1)-sg.p0)*upb
		lo, hi, ok := p.span(fileSeconds(ci, ca, tempo), fileSeconds(ci, cb, tempo))
		if !ok {
			continue
		}
		ya := mid - int(math.Round(float64(hi)/127*amp))
		yb := mid - int(math.Round(float64(lo)/127*amp))
		if ya < top {
			ya = top
		}
		if yb >= ya && ya < screenH {
			gfx.FillRect(img, x, ya, 1, min(yb-ya+1, screenH-ya), ink)
		}
	}
}

// boxRect: screen rectangle of a clip box; ok false when off screen.
func boxRect(s *Set, v viewport, b clipBox) (x0, y0, w, h int, ok bool) {
	if b.track < 0 || b.track >= len(s.Tracks) || b.end <= b.start {
		return
	}
	top := rulerH + 1
	y0 = top + int((float64(b.track)-v.first)*v.laneH)
	y1 := top + int((float64(b.track+1)-v.first)*v.laneH)
	if y1 <= top || y0 >= screenH {
		return
	}
	h = y1 - y0
	if h > 2 {
		h--
	}
	x0 = v.x(b.start)
	x1 := v.x(b.end)
	if x1 <= v.gutter || x0 >= screenW {
		return
	}
	w = x1 - x0
	if w < 2 {
		w = 2
	}
	return x0, y0, w, h, true
}

// darkenClipBox: the clip a pending edit will move or change.
func darkenClipBox(img *image.NRGBA, s *Set, v viewport, b clipBox) {
	x0, y0, w, h, ok := boxRect(s, v, b)
	if !ok {
		return
	}
	top := rulerH + 1
	for y := max(y0, top); y < min(y0+h, screenH); y++ {
		for x := max(x0, v.gutter); x < min(x0+w, screenW); x++ {
			c := img.NRGBAAt(x, y)
			img.SetNRGBA(x, y, color.NRGBA{c.R / 3, c.G / 3, c.B / 3, 255})
		}
	}
}

// drawClipBox outlines a clip-sized box (2 px) on track b.track.
func drawClipBox(img *image.NRGBA, s *Set, v viewport, b clipBox, col color.NRGBA) {
	x0, y0, w, h, ok := boxRect(s, v, b)
	if !ok {
		return
	}
	top := rulerH + 1
	t := 2
	if h < 6 {
		t = 1
	}
	fill(img, x0, y0, w, t, v.gutter, top, col)     // top
	fill(img, x0, y0+h-t, w, t, v.gutter, top, col) // bottom
	fill(img, x0, y0, t, h, v.gutter, top, col)     // left
	fill(img, x0+w-t, y0, t, h, v.gutter, top, col) // right
}

package main

import (
	"image"
	"image/color"
	"strconv"
	"strings"

	"github.com/federico-pepe/ableton-push-hack/core/gfx"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/text"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

// Push 3 screen size in pixels.
const (
	screenW = 960
	screenH = 160
	rulerH  = 14
)

// paletteRGB resolves a Push palette index to its real RGB -- the same table
// LED writes use (core/push3), so a colour drawn on screen matches the LED
// that means the same thing (e.g. the loop brace and the Loop button).
func paletteRGB(idx byte) color.NRGBA { return push3.ColorForIndex(idx).RGB }

// namedRGB resolves a push3 palette colour name to its RGB. Panics on a
// typo'd name -- only ever called with hand-checked names below.
func namedRGB(name string) color.NRGBA {
	idx, ok := push3.ColorByName(name)
	if !ok {
		panic("arrangement: unknown push3 colour " + name)
	}
	return paletteRGB(idx)
}

// Plain UI chrome: widgets.Default is the same shared theme push-manager's
// Shadow UI and the other hacks draw with (itself resolved from push3.Palette
// -- see core/gfx/widgets/theme.go), so this screen looks like the rest of
// the framework instead of inventing its own palette.
var (
	colBG     = widgets.Default.Black
	colGutter = widgets.Default.DarkGray
	colGrid   = widgets.Default.DarkGray
	colBar    = namedRGB("gray_mid") // widgets.Default.Gray is green-tinted; a ruler wants neutral
	colText   = widgets.Default.CrumbCol
)

// Colours that mean the same thing as a button LED, so they are resolved
// from the very same palette index or name.
var (
	colPlay   = paletteRGB(ledWhite)  // playhead, toast border, selected locator
	colLoop   = paletteRGB(ledYellow) // loop brace and boundaries, same as the lit Loop button
	colLoc    = namedRGB("sky")       // untouched locator line
	colAnchor = namedRGB("mint")      // play-start anchor marker
	colCursor = namedRGB("lavender")  // scrub cursor (plain jog wheel; does not move Live)
)

// colSelLane: track-name column of the selected track (clip edit mode).
var colSelLane = namedRGB("gray_mid")

const softStripH = 16 // soft-button labels at the bottom (clip edit mode)

func rgb(c uint32) color.NRGBA {
	return color.NRGBA{uint8(c >> 16), uint8(c >> 8), uint8(c), 255}
}

// viewport: what part of the arrangement is on screen.
type viewport struct {
	x0         float64             // time at left edge of the clip area (beats)
	ppb        float64             // pixels per beat
	laneH      float64             // pixels per track lane
	first      float64             // first visible track (fractional)
	gutter     int                 // width of the track-name column (0 = none)
	toastLines []string            // message box lines (a toast), nil = none
	overlay    []widgets.HeaderCol // touch layer header columns, nil = none

	content *contentCache // notes and waveforms; nil = plain clips

	// Clip edit mode (edit.go): the selected track, only when selTrackOn.
	selTrack   int
	selTrackOn bool
	sel        *clipBox               // selected clip, outlined white
	ghost      *clipBox               // where a pending encoder edit puts it, outlined yellow
	soft       *[8]widgets.SoftButton // labels for the buttons under the screen
}

// clipBox: a clip's place on screen terms: track index and time range.
type clipBox struct {
	track      int
	start, end float64
}

// fitView shows the whole song, every track.
func fitView(s *Set) viewport {
	return viewport{ppb: fitPPB(s), laneH: fitLane(s)}
}

func (v viewport) x(beat float64) int { return v.gutter + int((beat-v.x0)*v.ppb) }

// fill draws a rectangle clipped to [minX,screenW) x [minY,screenH).
func fill(img *image.NRGBA, x, y, w, h, minX, minY int, c color.NRGBA) {
	if x < minX {
		w -= minX - x
		x = minX
	}
	if y < minY {
		h -= minY - y
		y = minY
	}
	if x+w > screenW {
		w = screenW - x
	}
	if y+h > screenH {
		h = screenH - y
	}
	if w > 0 && h > 0 {
		gfx.FillRect(img, x, y, w, h, c)
	}
}

// dimFactor: clip colours when Session clips override the arrangement.
const dimFactor = 0.35

func dim(c color.NRGBA) color.NRGBA {
	return color.NRGBA{uint8(float64(c.R) * dimFactor), uint8(float64(c.G) * dimFactor), uint8(float64(c.B) * dimFactor), 255}
}

// grey: a muted clip, like Live draws one.
func grey(c color.NRGBA) color.NRGBA {
	l := uint8(40 + luma(c)*35/100)
	return color.NRGBA{l, l, l, 255}
}

func luma(c color.NRGBA) int { return (int(c.R)*299 + int(c.G)*587 + int(c.B)*114) / 1000 }

const (
	loopBraceY = rulerH - 2 // loop-region bar on the ruler: y start
	loopBraceH = 4          // ...and height (taller than the old 2px)
	anchorW    = 8          // play-start anchor triangle: base width
	anchorH    = 6          // ...and height
)

// fillTriangleDown draws a small filled triangle pointing down (apex at the
// bottom), base of width w centred on cx at row y0. Used for the play-start
// anchor marker -- more visible at a glance than a short bar.
func fillTriangleDown(img *image.NRGBA, cx, y0, w, h, minX int, col color.NRGBA) {
	for row := 0; row < h; row++ {
		half := w * (h - row) / (2 * h)
		fill(img, cx-half, y0+row, 2*half+1, 1, minX, 0, col)
	}
}

// renderArrangement draws the set. Text is ASCII only: the panel font has no other glyphs.
func renderArrangement(s *Set, v viewport) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, colBG)

	drawRuler(img, v, s.bpb())

	top := rulerH + 1
	g := v.gutter
	for i, tr := range s.Tracks {
		y0 := top + int((float64(i)-v.first)*v.laneH)
		y1 := top + int((float64(i+1)-v.first)*v.laneH)
		if y1 <= top || y0 >= screenH {
			continue
		}
		h := y1 - y0
		if h > 2 {
			h-- // 1 px gap between lanes
		}
		for ci, c := range tr.Clips {
			x0, x1 := v.x(c.Start), v.x(c.End)
			if x1 <= g || x0 >= screenW {
				continue
			}
			w := x1 - x0
			if w < 1 {
				w = 1
			}
			col := rgb(c.Color)
			if c.Muted {
				col = grey(col)
			}
			if s.Overridden {
				col = dim(col)
			}
			fill(img, x0, y0, w, h, g, top, col)
			named := v.laneH >= namesMinLane && w >= 36 && y0 >= top && y0+h <= screenH
			if v.content != nil && w >= 3 {
				drawContent(img, s, v, i, &tr.Clips[ci], x0, x1, y0, h, named, col)
			}
			// clip name when there is room
			if named {
				tc := colBG
				if luma(col) < 110 {
					tc = colPlay
				}
				vis := x0
				if vis < g {
					vis = g
				}
				if x1-vis >= 36 {
					text.Draw(img, vis+3, y0+10, text.Truncate(c.Name, (x1-vis-6)/7), tc)
				}
			}
		}
		if g > 0 {
			gc := colGutter
			if v.selTrackOn && i == v.selTrack {
				gc = colSelLane
			}
			fill(img, 0, y0, g, h, 0, top, gc)
			fill(img, 0, y0, 3, h, 0, top, rgb(tr.Color))
			if y0 >= top && y0+h <= screenH {
				ind := 0
				if tr.GroupID >= 0 {
					ind = 8
				}
				text.Draw(img, 7+ind, y0+10, text.Truncate(tr.Name, (g-10-ind)/7), colText)
			}
		} else if v.selTrackOn && i == v.selTrack {
			fill(img, 0, y0, 2, h, 0, top, colPlay)
		}
	}

	// edit mode: the selected clip has a white outline. While an encoder edit
	// waits to be sent, the clip goes dark where it is now and the outline
	// shows where it will be.
	switch {
	case v.ghost != nil && v.sel != nil:
		darkenClipBox(img, s, v, *v.sel)
		drawClipBox(img, s, v, *v.ghost, colPlay)
	case v.sel != nil:
		drawClipBox(img, s, v, *v.sel, colPlay)
	}

	// locators -- the one last jumped to (Page </>) draws white, the rest sky blue
	for _, l := range s.Locators {
		if x := v.x(l.Time); x >= v.gutter && x < screenW {
			col := colLoc
			if s.HasSelLocator && sameBeat(l.Time, s.SelLocator) {
				col = colPlay
			}
			gfx.FillRect(img, x, 0, 1, screenH, col)
		}
	}
	// loop brace on the ruler -- shows the region regardless of on/off
	if s.Loop.Length > 0 {
		x0, x1 := v.x(s.Loop.Start), v.x(s.Loop.Start+s.Loop.Length)
		if x1 > v.gutter && x0 < screenW {
			fill(img, x0, loopBraceY, x1-x0, loopBraceH, v.gutter, 0, colLoop)
		}
		// boundary lines, full height, only while the loop is actually ON
		if s.Loop.On {
			if x0 >= v.gutter && x0 < screenW {
				fill(img, x0, top, 1, screenH-top, v.gutter, top, colLoop)
			}
			if x1 >= v.gutter && x1 < screenW {
				fill(img, x1, top, 1, screenH-top, v.gutter, top, colLoop)
			}
		}
	}
	// play-start anchor (jog wheel press): a small triangle pointing into the timeline
	if s.HasAnchor {
		if x := v.x(s.Anchor); x >= v.gutter && x < screenW {
			fillTriangleDown(img, x, rulerH+1, anchorW, anchorH, v.gutter, colAnchor)
		}
	}
	// scrub cursor (plain jog wheel -- local only, Live's playback is untouched)
	if s.HasCursor {
		if x := v.x(s.Cursor); x >= v.gutter && x < screenW {
			fill(img, x, 0, 1, screenH, v.gutter, 0, colCursor)
		}
	}
	// playhead (Live's real position)
	if x := v.x(s.Playhead); x >= v.gutter && x < screenW {
		fill(img, x, 0, 2, screenH, v.gutter, 0, colPlay)
	}

	if v.soft != nil {
		widgets.DrawBotStrip(img, widgets.Default, screenH-softStripH, screenW, screenW/8, softStripH, *v.soft, "")
	}
	if len(v.overlay) > 0 {
		widgets.DrawTouchHeader(img, widgets.Default, v.overlay)
	}
	if len(v.toastLines) > 0 {
		drawToast(img, v.toastLines)
	}
	return img
}

func sameBeat(a, b float64) bool {
	d := a - b
	return d > -1e-6 && d < 1e-6
}

const (
	toastBarH = 18
)

// joinToastLines combines several lines (e.g. the touch layer showing the
// jog wheel and a loop knob at once) into one row for the status bar.
func joinToastLines(lines []string) string { return strings.Join(lines, "   ") }

// toastTheme: the bottom bar is black with white text.
var toastTheme = func() widgets.Theme {
	t := widgets.Default
	t.StatusBg, t.StatusCol = colBG, t.White
	return t
}()

// drawToast: a bottom status bar, widgets.DrawStatusBar -- the same one-line
// message strip push-tethered-app's modules use.
func drawToast(img *image.NRGBA, lines []string) {
	widgets.DrawStatusBar(img, toastTheme, screenH-toastBarH, screenW, toastBarH, joinToastLines(lines), false)
}

// drawRuler: bar numbers (labels >= 48 px apart), beat ticks when zoomed in.
func drawRuler(img *image.NRGBA, v viewport, bpb float64) {
	gfx.FillRect(img, v.gutter, rulerH, screenW-v.gutter, 1, colBar)
	barPx := v.ppb * bpb
	step := 1
	for float64(step)*barPx < 48 {
		step *= 2
	}
	first := int(v.x0 / bpb)
	if first < 0 {
		first = 0
	}
	if v.ppb >= 12 { // beat lines
		for b := int(v.x0); ; b++ {
			x := v.x(float64(b))
			if x >= screenW {
				break
			}
			if x >= v.gutter && float64(b) != float64(int(float64(b)/bpb))*bpb {
				fill(img, x, 6, 1, rulerH-6, v.gutter, 0, colBar)
			}
		}
	}
	for bar := first - first%step; ; bar += step {
		x := v.x(float64(bar) * bpb)
		if x >= screenW {
			break
		}
		if x < v.gutter {
			continue
		}
		fill(img, x, rulerH+1, 1, screenH-rulerH-1, v.gutter, 0, colGrid)
		fill(img, x, 0, 1, rulerH, v.gutter, 0, colBar)
		text.Draw(img, x+3, 11, strconv.Itoa(bar+1), colText)
	}
}

// renderMessage: full-screen text, for errors ("no set found").
func renderMessage(msg, hint string) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, screenW, screenH))
	gfx.FillRect(img, 0, 0, screenW, screenH, colBG)
	text.DrawScaled(img, 20, 60, 2, msg, colPlay)
	if hint != "" {
		widgets.DrawStatusBar(img, widgets.Default, screenH-toastBarH, screenW, toastBarH, hint, false)
	}
	return img
}

func (s *Set) bpb() float64 {
	if s.BeatsPerBar > 0 {
		return s.BeatsPerBar
	}
	return 4
}

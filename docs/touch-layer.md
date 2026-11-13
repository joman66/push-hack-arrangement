# Touch layer

**Status:** built for the jog wheel and loop knobs 1/2 (`touch.go`, wired in `main.go`).
The rest of this page is still the design for the remaining controls (Volume, Tempo,
the top 8 knobs above the screen) -- build their content the same way, in the same
change that gives them a job.

> **Update (prototype):** the sections below describe the first design (one bottom
> bar). Now: touching any encoder (notes 0-7) dims the screen and draws a header
> (`core/gfx/widgets`: `DrawTouchHeader`, called from `render.go`) -- per column, the top button function, the
> encoder name and its value. Columns 1-3 have content (Loop, Punch In, Punch Out;
> Loop Start, Loop Length). Tempo and Volume touches show their value in the bottom bar, with no dim (`Zoom Time: N bars` / `Zoom Tracks: N tracks`, `View: bar A-B`). The jog wheel
> keeps the bottom bar (black, white text). Knob touches wait `touchShowDelay` (150 ms) before they show; jog and button presses show at once. When a touched knob is turned (`touchLayer.onTurn`), the overlay goes away and the bar shows that knob's value until the next touch. The bottom bar is also used by toasts.

## Idea

The screen is small. We do not want a top bar and a bottom bar on it all the time.
Instead, the arrangement uses the whole screen. When you touch a knob or the jog wheel,
a temporary bar appears at the bottom of the screen and shows what that control does
and its value. The bar goes away a moment after you stop touching.

## Input: touch notes

The knobs above the screen and the jog wheel are capacitive. They send a Note On
(velocity 127) when you touch them and a Note Off (or velocity 0) when you let go.
The hack receives these events from the Push port, also while the MIDI intercept
hides them from Live (`midi.go`'s `midiHandler.Fixed`, `onNote`).

| Control | Touch note | Built |
|---|---|---|
| Knobs 1 to 8 (above the screen) | 0 to 7 | Knobs 1/2 (loop start/length) |
| Volume knob | 8 | not yet |
| Tempo knob | 10 | not yet |
| Jog wheel | 11 | yes |

Other touch notes (not planned for the layer): touch strip 12, D-pad center 13.
Source: `docs/push3-button-map.md` in the ableton-push-hack repo.

## Behavior

1. Note On of a control: show the bar for that control at once.
2. Note Off: start a timer of 500 ms (`touchHoldAfterRelease` in `touch.go`).
3. When the timer ends, remove the bar.
4. A new touch during the timer cancels the timer. The bar stays.
5. If two controls are touched at once, both lines show, jog wheel first (`touchOrder`).
6. Nothing runs while the mode is off: `midiHandler.onNote` is only ever called with
   real button input, which is itself gated by Arrangement Mode being on.
7. A short message such as "ZOOM: TIME" (`viewCtl.setToast`) is the same kind of bar,
   drawn the same way, but it is not tied to a touch -- it just times out on its own.

## Content per control

| Control | Bar shows | Example |
|---|---|---|
| Jog wheel | The playhead position -- the scrub marker's position while one is active, else the real playhead | `PLAYHEAD 97.1.1` |
| Encoder 1 (top knob 1) | The loop start | `LOOP START 97.1.1` |
| Encoder 2 (top knob 2) | The loop length | `LOOP LEN 16.0.0` |
| Volume knob | not yet: could show the view's start/end (bars) | -- |
| Tempo knob | not yet: could show the zoom mode and level | -- |
| Knobs 3 to 8 | not yet -- no job assigned | -- |
| Encoders 1 to 7, clip edit mode | The selected clip: Position, Track, Start, End, Transpose, Velocity (MIDI) or Gain (audio), Color. The header shows all seven; turning one shows its value in the bar | `POSITION 5.1.1`, `TRANSPOSE +2 st` |

Positions and lengths are formatted bars.beats.16th (`bbs.go`: `bbsPosition`,
`bbsLength`), matching Live's own display.

Rule for later work: **when you add a control, add its bar content in the same change,
update this table, and update the controls table in the README.**

## Drawing

- The bar is drawn into the same 960x160 image, after the arrangement --
  `render.go`'s `drawToast`, which calls `widgets.DrawStatusBar` (`core/gfx/widgets`),
  the same one-line message strip other push-hack modules use. No new display code.
- `viewport.toastLines []string` carries the lines. `main.go`'s `frame` closure fills
  it from the touch layer (`touch.text()`) when something is touched, or from the
  view's own one-shot toast (`viewCtl.setToast`) otherwise -- the touch layer wins if
  both would show at once.
- Text is ASCII only: the panel font has no other glyphs.
- A frame is redrawn only when something changes. The timer end asks for one redraw
  too (`touchLayer.onNote`'s `time.AfterFunc`), so the bar actually disappears.

## Implementation

- `touch.go`: `touchLayer` holds which notes are held or still in their release grace
  period (`active()`), and turns that into text (`text()`) using a caller-supplied
  `lines func() map[uint8]string` -- recomputed fresh on every redraw, so it is always
  current with no separate update path.
- `midi.go`: `midiHandler.onNote(note uint8, on bool)`, called from `Fixed` for
  `alsaseq.EvNoteOn`/`EvNoteOff` on channel 0 from Push's own port.
- Tests: `touch_test.go` (touch on/off, the release grace period, a re-touch
  cancelling it, two touches at once, an unrelated note ignored).

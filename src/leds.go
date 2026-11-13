package main

// LED blackout. Own ALSA seq client writes straight to Push's Live port,
// same way push-manager's clearAllLEDs does (button CCs -> 0, pad notes -> 0).
// Live keeps its own LED state and only sends changes, so while the mode is
// on we repeat the blackout to win against late Live updates. LEDs in ledLit
// (Play, Session) stay lit.

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

var (
	ledMu  sync.Mutex
	ledSeq *alsaseq.Client
)

func setLEDClient(c *alsaseq.Client) {
	ledMu.Lock()
	ledSeq = c
	ledMu.Unlock()
}

// lit holds LEDs we want ON while everything else is dark: cc -> palette value.
var ledLit = map[byte]byte{}

// setLED lights (or clears) one button LED now and remembers it for the blackout loop.
func setLED(cc, val byte) {
	ledMu.Lock()
	defer ledMu.Unlock()
	if val == 0 {
		delete(ledLit, cc)
	} else {
		ledLit[cc] = val
	}
	if ledSeq != nil {
		_ = ledSeq.SendCC(pushLive.Addr(), 0, cc, int32(val))
	}
}

func resetLit() {
	ledMu.Lock()
	ledLit = map[byte]byte{}
	ledMu.Unlock()
}

// syncStateLEDs: Play is green while playing; Session is bright white when Back
// to Arrangement is available. Page left/right are always white -- they
// navigate locators, not a state -- and the loop/punch buttons follow the
// transport.
func syncStateLEDs(playing, bta bool) {
	play, session := byte(ledWhite), byte(0)
	if playing {
		play = ledPlayGreen
	}
	if bta {
		session = ledWhite
	}
	setLED(byte(push3.CCPlay), play)
	setLED(byte(push3.CCSession), session)
}

// syncTransportLEDs: the three top-left buttons (Loop, Punch In, Punch Out)
// are white when off, yellow when on.
func syncTransportLEDs(loopOn, punchIn, punchOut bool) {
	v := func(on bool) byte {
		if on {
			return ledYellow
		}
		return ledWhite
	}
	setLED(byte(push3.CCScreenTop1), v(loopOn))
	setLED(byte(push3.CCScreenTop2), v(punchIn))
	setLED(byte(push3.CCScreenTop3), v(punchOut))
}

// syncNavLEDs: Page left/right are always lit white while the mode is on --
// they move between locators, not a state to reflect.
func syncNavLEDs() {
	setLED(byte(push3.CCPageLeft), ledWhite)
	setLED(byte(push3.CCPageRight), ledWhite)
}

// syncDPadLEDs: each D-pad direction is white only while it can still scroll
// further (see viewCtl.computeAvailability), dark at the edge.
func syncDPadLEDs(up, down, left, right bool) {
	v := func(on bool) byte {
		if on {
			return ledWhite
		}
		return 0
	}
	setLED(byte(push3.CCDPadUp), v(up))
	setLED(byte(push3.CCDPadDown), v(down))
	setLED(byte(push3.CCDPadLeft), v(left))
	setLED(byte(push3.CCDPadRight), v(right))
}

// syncEditLEDs: Clip lights while clip edit mode is on; Undo is always lit.
// The buttons edit mode uses (Duplicate, Delete, Mute, Quantize, New and the
// 8 under the screen) light only in edit mode.
func syncEditLEDs(on bool) {
	v := byte(0)
	if on {
		v = ledWhite
	}
	setLED(byte(push3.CCClipView), v)
	setLED(byte(push3.CCUndo), ledWhite)
	for _, cc := range []uint8{push3.CCDuplicate, push3.CCDelete, push3.CCMute, push3.CCQuantize, push3.CCNew,
		push3.CCScreenBot1, push3.CCScreenBot2, push3.CCScreenBot3, push3.CCScreenBot4,
		push3.CCScreenBot5, push3.CCScreenBot6, push3.CCScreenBot7, push3.CCScreenBot8} {
		setLED(byte(cc), v)
	}
}

// Palette indices chosen on the device.
const (
	ledPlayGreen = 126 // Play while playing
	ledWhite     = 120 // "on" for a greyscale button (Session, Page, Loop/Punch when off)
	ledYellow    = 8   // Loop/Punch when on
)

const (
	padNoteMin = 36
	padNoteMax = 99
)

// blackoutLEDs turns every button LED and every pad off.
func blackoutLEDs() {
	ledMu.Lock()
	defer ledMu.Unlock()
	if ledSeq == nil {
		return
	}
	// Every CC 1..127, not a list of known buttons: scene buttons, touch strip
	// cluster and anything else must go dark too. Non-LED CCs are ignored by Push.
	for cc := 1; cc <= 127; cc++ {
		_ = ledSeq.SendCC(pushLive.Addr(), 0, byte(cc), int32(ledLit[byte(cc)]))
	}
	for n := padNoteMin; n <= padNoteMax; n++ {
		_ = ledSeq.SendNote(pushLive.Addr(), 0, byte(n), 0)
	}
}

// pushLive: Push 3's Live port, where input comes from and LED messages go.
// Client number moves with sound cards (16 on card 0, 20 with loopback on
// card 0); runMIDI refreshes it before each subscribe.
var pushLive alsaseq.Push3

// ledActive: true while Arrangement Mode owns the LEDs.
var ledActive atomic.Bool

// burstDelays: Live repaints LEDs a moment after a command (Play, Back to
// Arrangement). Repeat the blackout right after, so the flicker is short.
var burstDelays = []time.Duration{0, 40 * time.Millisecond, 90 * time.Millisecond,
	160 * time.Millisecond, 260 * time.Millisecond, 420 * time.Millisecond,
	700 * time.Millisecond, 1100 * time.Millisecond}

// burstLEDs repeats the blackout for about a second. done (optional) runs at the
// end, to repaint the lit LEDs from the real state.
func burstLEDs(done func()) {
	go func() {
		var prev time.Duration
		for _, d := range burstDelays {
			time.Sleep(d - prev)
			prev = d
			if !ledActive.Load() {
				return
			}
			blackoutLEDs()
		}
		if done != nil && ledActive.Load() {
			done()
		}
	}()
}

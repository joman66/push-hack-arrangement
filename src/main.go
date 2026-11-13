// arrangement: draws the Live Arrangement on the Push 3 screen.
// Shift+Session toggles Arrangement Mode (display takeover + MIDI intercept).
package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/federico-pepe/ableton-push-hack/core/alsaseq"
	"github.com/federico-pepe/ableton-push-hack/core/gfx/widgets"
	"github.com/federico-pepe/ableton-push-hack/core/pmclient"
	"github.com/federico-pepe/ableton-push-hack/core/push3"
)

func main() {
	pm := flag.String("pm", "http://127.0.0.1:7701", "push-manager base URL")
	// The catalog's boot service starts every hack as `<binary> -config <hack.json>`.
	// This hack has no settings of its own, so the flag is accepted and ignored.
	_ = flag.String("config", "", "path to hack.json (passed by the boot service; unused)")
	setPath := flag.String("set", "", "dev: show this saved Live Set (.als) instead of the Remote Script data")
	hz := flag.Int("hzoom", 0, "preview: horizontal zoom ticks")
	vz := flag.Int("vzoom", 0, "preview: vertical zoom ticks")
	scroll := flag.Int("scroll", 0, "preview: scroll ticks in time")
	tscroll := flag.Int("tracks", 0, "preview: scroll tracks")
	bench := flag.Int("bench", 0, "dev: redraw for this many seconds with a fake playhead and print CPU use (use with -set)")
	preview := flag.String("preview", "", "write the view to this PNG and exit (needs -set, no device needed)")
	flag.Parse()

	if *preview != "" {
		s, err := LoadSetFile(*setPath)
		if err != nil {
			log.Fatal(err)
		}
		f, err := os.Create(*preview)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, renderArrangement(s, previewView(s, *hz, *vz, *scroll, *tscroll))); err != nil {
			log.Fatal(err)
		}
		log.Printf("wrote %s (%d tracks)", *preview, len(s.Tracks))
		return
	}

	if *bench > 0 {
		s, err := LoadSetFile(*setPath)
		if err != nil {
			log.Fatal(err)
		}
		vc := newViewCtl(func() *Set { return s })
		if err := runBench(pmclient.New(*pm), s, vc, *bench); err != nil {
			log.Fatal(err)
		}
		return
	}

	// The boot service starts this parent; the real work runs as a supervised child.
	if os.Getenv(supervisedEnv) != "1" {
		runSupervisor(*pm)
		return
	}

	// Cold-boot USB-A window: no /dev/snd access before uptime >= 30 s.
	alsaseq.WaitForBootSettle()

	client := pmclient.New(*pm)
	go runDependencyWatcher(*pm)

	st := &store{}
	vc := newViewCtl(func() *Set { s, _, _ := st.get(); return s })
	var mode *modeCtl // set below; touch's callback closes over it
	redraw := func() {
		if mode != nil {
			mode.refresh()
		}
	}
	content := newContentCache(callLive, redraw)
	chord := newChordDetector()
	edit := newEditCtl(st, vc, content, redraw, func() bool { return chord.isHeld(ccShift) })
	vc.onScrollAvailability(func(up, down, left, right bool) {
		if edit.isOn() { // D-pad selects tracks and clips: always lit
			up, down, left, right = true, true, true, true
		}
		syncDPadLEDs(up, down, left, right)
	})

	var ctl *controls // set below; touch's callback reads it at render time

	touch := newTouchLayer(func() touchInfo {
		s, _, _ := st.get()
		if s == nil {
			return touchInfo{}
		}
		bpb := s.bpb()
		t := s.Playhead
		if s.HasCursor { // jog moves the cursor, not the real playhead -- show that instead
			t = s.Cursor
		}
		loopOn, punchIn, punchOut, loopStart, loopLength := st.transport()
		first, last, tracks := vc.viewInfo(s, bpb)
		zoom := fmt.Sprintf("Zoom Time: %d bars", last-first+1)
		if ctl != nil && ctl.zoomTracks() {
			zoom = fmt.Sprintf("Zoom Tracks: %d tracks", tracks)
		}
		info := touchInfo{
			cols: []widgets.HeaderCol{
				{Col: 0, Label: "Loop", Toggle: true, On: loopOn, Name: "Loop Start", Value: bbsPosition(loopStart, bpb)},
				{Col: 1, Label: "Punch In", Toggle: true, On: punchIn, Name: "Loop Length", Value: bbsLength(loopLength, bpb)},
				{Col: 2, Label: "Punch Out", Toggle: true, On: punchOut},
			},
			text: map[uint8]string{
				noteJogTouch:      "PLAYHEAD " + bbsPosition(t, bpb),
				noteEncoder1Touch: "LOOP START " + bbsPosition(loopStart, bpb),
				noteEncoder2Touch: "LOOP LENGTH " + bbsLength(loopLength, bpb),
				noteTempoTouch:    zoom,
				noteVolumeTouch:   fmt.Sprintf("View: bar %d-%d", first, last),
			},
		}
		if cols, text := edit.touchText(s); cols != nil {
			// clip edit mode: the knobs edit the selected clip; the top
			// buttons keep Loop / Punch In / Punch Out
			for i := range cols {
				if cols[i].Col < 3 {
					cols[i].Label, cols[i].Toggle = info.cols[cols[i].Col].Label, true
					cols[i].On = info.cols[cols[i].Col].On
				}
			}
			info.cols = cols
			delete(info.text, noteEncoder1Touch)
			delete(info.text, noteEncoder2Touch)
			for n, v := range text {
				info.text[n] = v
			}
		}
		return info
	}, func() { mode.refresh() })

	frame := func() image.Image {
		s, msg, hint := st.get()
		if s == nil {
			return renderMessage(msg, hint)
		}
		vp := vc.viewport(s)
		vp.content = content
		edit.decorate(s, &vp)
		tv := touch.view()
		vp.overlay = tv.cols
		if len(tv.bar) > 0 {
			vp.toastLines = tv.bar
		}
		return renderArrangement(s, vp)
	}
	mode = newModeCtl(client, frame)

	// syncLEDs repaints every LED this hack manages from the store's current
	// state. Called once when the mode turns on, and again (via burstLEDs)
	// whenever Live reports a change, so Live's own repaint never wins.
	syncLEDs := func() {
		playing, bta := st.state()
		loopOn, punchIn, punchOut, _, _ := st.transport()
		syncStateLEDs(playing, bta)
		syncTransportLEDs(loopOn, punchIn, punchOut)
		syncNavLEDs()
		syncEditLEDs(edit.isOn())
		if s, _, _ := st.get(); s != nil {
			up, down, left, right := vc.availability(s)
			if edit.isOn() {
				up, down, left, right = true, true, true, true
			}
			syncDPadLEDs(up, down, left, right)
		}
	}
	edit.onToggle = func(bool) { syncLEDs() }

	stop := make(chan struct{})
	if *setPath != "" {
		// dev: show a saved .als once instead of the Remote Script. store.get()
		// always overlays Loop/PunchIn/PunchOut from a "pos" report (see
		// putPos); seed it once from the file so the loop bar isn't blanked.
		s, err := LoadSetFile(*setPath)
		if err != nil {
			log.Fatal(err)
		}
		st.putSet(s)
		st.putPos(s.Playhead, false, false, s.Loop.On, s.Loop.Start, s.Loop.Length, false, false)
	} else {
		var last struct{ playing, bta, loopOn, punchIn, punchOut bool }
		go runLiveSource(st, func() {
			if mode.isOn() {
				t, playing := st.pos()
				if !edit.isOn() { // editing: the view stays where you put it
					vc.follow(t, playing)
				}
				_, bta := st.state()
				loopOn, punchIn, punchOut, _, _ := st.transport()
				if playing != last.playing || bta != last.bta || loopOn != last.loopOn ||
					punchIn != last.punchIn || punchOut != last.punchOut {
					last.playing, last.bta = playing, bta
					last.loopOn, last.punchIn, last.punchOut = loopOn, punchIn, punchOut
					syncLEDs()
					burstLEDs(syncLEDs)
				}
			}
			mode.refresh()
		}, stop)
	}
	ctl = newControls(vc, st, mode.isOn,
		func() bool { return chord.isHeld(ccShift) },
		func() bool { return chord.isHeld(ccDelete) },
		mode.refresh, sendCmd,
		func(what string, on bool) {
			switch what {
			case "play":
				setLED(byte(push3.CCPlay), map[bool]byte{true: ledPlayGreen, false: ledWhite}[on])
			case "bta":
				setLED(byte(push3.CCSession), map[bool]byte{true: ledWhite, false: 0}[on])
			case "loop":
				setLED(byte(push3.CCScreenTop1), map[bool]byte{true: ledYellow, false: ledWhite}[on])
			case "punch_in":
				setLED(byte(push3.CCScreenTop2), map[bool]byte{true: ledYellow, false: ledWhite}[on])
			case "punch_out":
				setLED(byte(push3.CCScreenTop3), map[bool]byte{true: ledYellow, false: ledWhite}[on])
			}
			burstLEDs(syncLEDs)
		})
	ctl.edit = edit
	h := &midiHandler{chord: chord, onCC: func(cc, val uint8) {
		if mode.isOn() {
			touch.onButton(cc, val)
		}
		touch.onTurn(cc)
		ctl.onCC(cc, val)
	}, onNote: func(note uint8, on bool) {
		touch.onNote(note, on)
		if !on && isEncoderNote(note) && edit.isOn() {
			edit.released(int(note)) // knob let go: send the clip edit now
		}
	},
		onFire: func() {
			if mode.isOn() {
				edit.off() // first: its LEDs must go before the Push is handed back
			}
			mode.toggle()
			if mode.isOn() {
				syncLEDs()
			} else {
				ctl.releaseAll()
			}
		}}
	go runMIDI(h, stop)

	log.Print("arrangement ready: press Shift+Session on Push")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Print("stopping: releasing display + MIDI intercept")
	close(stop)
	mode.release() // always, whatever the last state was
}

// previewView applies zoom/scroll ticks to a fit view, for `make preview` screenshots.
func previewView(s *Set, hz, vz, scroll, tracks int) viewport {
	vc := newViewCtl(func() *Set { return s })
	vc.viewport(s)
	vc.zoomH(hz)
	vc.zoomV(vz)
	vc.scrollT(scroll)
	vc.scrollTracks(tracks)
	return vc.viewport(s)
}

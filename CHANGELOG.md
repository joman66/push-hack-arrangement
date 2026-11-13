# Changelog

## [Unreleased]

### Added
- Clip edit mode (Clip button), in the spirit of the MPC's arrange view. Select a track (D-pad up/down) and a clip (D-pad left/right or the jog wheel). Encoders 1-7 move the clip in time or to another track, move its start or end edge, transpose it, change velocity (MIDI) or gain (audio), and change its colour. A knob edit shows as a white outline first and goes to Live when you let go of the knob. The buttons under the screen split, duplicate, delete, mute, quantize, crop, copy and paste. The Duplicate, Delete, Mute, Quantize and New buttons work too. Undo (Shift: Redo) works in and out of edit mode. Needs PushHack Remote Script 0.6.0.
- Clips show their content: MIDI notes and audio waveforms, following loops and warp markers. Waveforms are read from WAV and AIFF files on the Push (Push 3 standalone). Muted clips are grey.

### Changed
- The hack can now wait for a reply from PushHack (`callLive`), and reads the script version from `hello`.

## [0.0.11-alpha] - 2026-11-12

First public release, installable from the Push Hack catalog.

### Changed
- The touch overlay drawing (dim, header columns, On/Off box) now comes from `core` (`widgets.DrawTouchHeader`, since v0.5.2). The look is the same.
- Touching any of the 8 encoders dims the screen and shows a header like Live's own: for each column, the top button function (Loop, Punch In, Punch Out, with on/off), then the encoder name and value (Loop Start, Loop Length). Touching the Tempo or Volume knob shows its value in the bottom bar (zoom mode and level, or the bars in view), with no dim. As soon as you turn an encoder, the dim and header go away and the bottom bar shows the value as it changes. The jog wheel keeps the bottom bar, now black with white text. The Loop, Punch In and Punch Out buttons show On/Off in a rounded box (white with black text when on, white border when off). Pressing the Loop, Punch In or Punch Out button shows the same header, so you see the box change. The "LOOP ON" style bottom message for these buttons is gone. A knob touch shows the overlay after 150 ms, so a quick tap or a turn that starts at once does not flash it.

### Fixed
- The hack finds Push 3's Live Port by name (`alsaseq.Push3`, `core` v0.5.4) instead of assuming ALSA client 16. With the loopback card as card 0 Push 3 is client 20, and the hack got no buttons and sent its LEDs to the wrong port.

## [0.0.10-alpha] - 2026-10-27

0.0.9-alpha was tagged but its release build failed (the workflow still copied the removed `remote-script/` folder). This release has the same code and the fixed workflow.

### Changed
- The hack reads Live through the shared PushHack Remote Script (`/tmp/push-hack.sock`, topics `arrangement` and `position`) and no longer has its own `PushHackArrangement` script. The old script is removed from this repo, and `hack.json` no longer installs it. Install Push Hack with PushHack (a release after 0.1.15-alpha), select **PushHack** in a control-surface slot in Live, restart Live, and remove the old `PushHackArrangement` from the User Library (Push Manager, System page).
- `docs/protocol.md` now lists the PushHack topics and commands this hack uses.

## [0.0.8-alpha] - 2026-10-19

### Changed
- Frames go to push-manager as raw pixels (`pmclient.Push`) instead of PNG, about 10 times faster. An older push-manager without the raw route still gets PNG. Needs `core` v0.4.1.

## [0.0.7-alpha] - 2026-10-19

### Changed
- Builds against `core` v0.3.0.

### Added
- The Remote Script listens on TCP `127.0.0.1:7720` on Windows, where Python has no Unix sockets. macOS and Linux keep the Unix socket. The messages do not change. See `remote-script/transport.py` and `docs/protocol.md`.

## [0.0.6-alpha] - 2026-09-29

### Fixed
- The on-screen playhead would sit frozen while playing if you turned the loop start/length knobs -- it only caught up once you stopped touching them. The playhead and the loop start/length each used to share one "ignore Live's report for a moment" timer (`store`'s `hold`), so a loop-knob edit kept re-arming the same timer the playhead was held by. They are now two independent timers (`hold` for the playhead, `loopHold` for the loop) -- see `source.go`, `source_test.go`'s `TestAdjustingLoopDoesNotFreezeThePlayhead`.

## [0.0.5-alpha] - 2026-09-29

- Loop start/length knobs (Encoder 1/2) have friction: a few raw encoder ticks now accumulate per knob before a step actually moves the loop, so a light touch doesn't send it flying. Both knobs get the same resistance, tracked separately so turning one doesn't eat into the other's count (`controls.go`'s `loopEncoderTicksPerStep`).
- Jog wheel press now toggles the play-start anchor: pressing again at the SAME position clears it, instead of needing Shift + press.
- Delete held + Tempo knob press resets the zoom to fit: shows the whole song, every track, from scratch.

## [0.0.4-alpha] - 2026-09-29

### Transport and locators
- Jog wheel press sets a play-start anchor at the current position (drawn as a small green triangle); Shift + press clears it. Pressing Play jumps there first if one is set.
- Page < / Page > jump to the previous/next locator (a real seek). The locator you land on draws white instead of sky blue. Both buttons are always lit white.
- Top buttons 1/2/3 (above the screen): Loop on/off, Punch In, Punch Out. White when off, yellow when on, matching the LED palette index used for the LED itself.
- Encoders 1/2 (top knobs 1/2, touch-capable): loop start and length, one bar per detent; Shift switches to a precise 1/16 beat step (not a coarser one -- for lining the loop up exactly).
- Loop region: the ruler bar is taller, and while Loop is on, full-height yellow lines mark the loop's start and end. The bar itself still shows the region when Loop is off.

### Jog wheel: scrub without seeking
- Plain jog wheel now moves a local "scrub cursor" only -- Live's real playback is never touched, so nothing audibly jumps while looking around during playback. Shift + jog wheel keeps the old behaviour: a real seek (`set_time`), audible if playing.
- Horizontal zoom (Tempo knob) always keeps the playhead -- or the scrub cursor, while one is active -- centred on screen, at any zoom level.
- The play-start anchor and Page </> locator jump follow the scrub cursor when one is active (so pressing jog after scrubbing anchors/jumps from where you were looking, not a stale playhead).

### Touch layer
- Built for the jog wheel and loop knobs 1/2 (`touch.go`): touching a knob or the jog wheel shows a status bar at the bottom of the screen with the exact value (bars.beats.16th, e.g. `97.1.1`, or `16.0.0` for a 16-bar length). Stays up briefly after release. The rest of the design (Volume, Tempo, the other top knobs) is still open in `docs/touch-layer.md`.

### D-pad
- Up/down/left/right now light white only while that direction still has more to scroll, dark at the edge (`view.go`'s `computeAvailability`, wired to `leds.go`'s `syncDPadLEDs`).

### Remote Script protocol
- Loop and punch state (`loop_on`, `loop_start`, `loop_length`, `punch_in`, `punch_out`) moved from the once-a-second `snapshot` message into the every-tick `pos` message, so a toggle or a knob turn shows up almost at once instead of waiting for the next snapshot.
- New commands: `loop_toggle`, `punch_in_toggle`, `punch_out_toggle`, `set_loop`. `set_time` and `set_loop` are coalesced per tick (only the last of each kind is applied), matching how the jog wheel and loop knobs can send several in one tick.
- **Restart Live after updating the script** (protocol change).

### Reuse core instead of hand-rolling
- Relative-encoder decoding now uses `core/push3.DecodeRel` (removed a duplicate local copy). CC/note constants for every new control are named from `core/push3` instead of being restated as magic numbers.
- Screen chrome (background, grid, ruler, text) now comes from `core/gfx/widgets.Default`, the same shared theme push-manager's Shadow UI and the other hacks draw with. The bottom message/touch-layer bar uses `widgets.DrawStatusBar`, the same one-line strip other push-hack modules use, in place of a hand-rolled floating box.

## [0.0.3-alpha] - 2026-09-25

- Volume knob now scrolls the view in time (this replaces Shift + jog wheel, which is gone; the jog wheel always moves the playhead).
- Tempo knob zooms. Pressing it (CC15) switches between time zoom and track zoom. A short message ("ZOOM: TIME" or "ZOOM: TRACKS") shows for about a second.
- Docs: `docs/touch-layer.md`, the design for a temporary bar shown when you touch a knob or the jog wheel (not built yet).

## [0.0.2-alpha] - 2026-09-25

- Project created. Plan added.
- Milestone 1: Go skeleton draws a static 960x160 test frame in display takeover; `-preview` writes a PNG without a device.
- Milestone 2: Shift+Session (CC49+CC51) toggles Arrangement Mode: display takeover + MIDI intercept, ON/OFF message on screen, release on exit. `scripts/deploy.sh` runs it on the Push.
- `core` now taken from GitHub (core v0.2.0), no local replace.
- Toggle changed: Shift+Session shows the view (title "Arrangement Mode"); the second press closes it at once, no message.
- Milestone 3: PushHackArrangement Remote Script streams the open set (tracks, clips, exact colours, locators, loop, time signature, playhead) over a Unix socket. The hack draws it fit to the screen, with a moving playhead. The saved-file (.als) reader is now only for `make preview` and `-set`.
- All button LEDs and the pad grid go dark while Arrangement Mode is on.
- Removed the "Arrangement Mode" title from the view.
- Milestone 4: horizontal zoom (Volume dial), vertical zoom (Tempo dial), scroll in time (jog wheel, D-pad left/right), scroll tracks (D-pad up/down). Track and clip names show when lanes are tall enough. The view follows the playhead while playing.
- All LEDs go dark in the mode (every CC, so the scene buttons too). Play is green while playing.
- Jog wheel now moves the playhead (grid by zoom, speed ramp). Shift + jog scrolls the view.
- Play starts and stops Live. Session = Back to Arrangement (bright white, palette 120, when available; the button has a greyscale LED). Play is palette 126. New Remote Script commands: `set_time`, `play_toggle`, `bta`. **Restart Live after updating the script.**
- D-pad repeats while held. The view scrolls smoothly with the playhead during playback.
- Accept `-config <hack.json>`: the catalog's boot service always passes it, and without it the hack would exit at boot. LEDs: Play = palette 126, Back to Arrangement = palette 4.
- Clips are drawn dimmed while Session clips override the arrangement (Back to Arrangement available); crisp again after Back to Arrangement. Play is white (120) when stopped.
- Less LED flicker: pressing Play shows the new colour at once (white to green, green to white). After Play, Back to Arrangement, or a change of state reported by Live, the LED blackout repeats for about a second so Live's own repaint is covered quickly.
- Docs: `docs/architecture.md`, `docs/protocol.md`, roadmap in the README.
- Supervisor: the boot service starts a small parent process that runs the hack as a child and restarts it if it dies. If the child dies while Arrangement Mode is on (crash, kill -9), the parent releases the display and MIDI intercept, so Push is never left dead. Tested on the device.
- Remote Script: on big sets it waits longer between snapshots, so building them never uses more than about 5% of Live's time.
- `-bench <seconds>` (with `-set`): measure CPU on the device. Result with a 40-track, 342-clip set at about 7.6 frames per second: hack 5.7% of one core, push-manager 4%.

# Architecture

The hack has two parts.

## 1. Remote Script (not in this repo)

The hack reads Live through the **PushHack** Remote Script, which ships with Push Hack
(`remote-script/PushHack/` in `ableton-push-hack`). This hack used to have its own script,
`PushHackArrangement`. That script is gone from this repo.

The hack connects to the Unix socket `/tmp/push-hack.sock`, sends `hello` and `subscribe`, and gets
two topics:

- `arrangement`: a snapshot of the open set (tracks, clips, locators, time signature, tempo). The script builds it about once a second at most and sends it only when it changed. On a big set it waits longer between builds.
- `position`: the playhead, transport and loop/punch state. It is sent every tick it changes, so a loop toggle or a knob turn shows up almost at once.

Commands go the other way: `set_time`, `set_loop` and `play_toggle` in the `transport` namespace, and `bta`, `loop_toggle`, `punch_in_toggle` and `punch_out_toggle` in `arrangement`. `source.go` turns them from the shape the rest of the hack uses. Commands that need an answer (clip content, clip edits, undo) go through `callLive`, which matches the reply by `id`.

This hack uses the script on the Push 3 (Linux). Another client can also use it on the computer where Live runs.

## 2. Go program (`src/`, runs as a normal process)

| File | Job |
|---|---|
| `main.go` | Start-up, wiring, signal handling. Releases display and MIDI on exit |
| `supervisor.go` | The started process is a parent. It runs the hack as a child and restarts it. If the child dies with the mode on (state file `/tmp/push-hack-arrangement.mode`), it releases display and MIDI |
| `bench.go` | `-bench` mode to measure CPU on the device |
| `midi.go` | Own ALSA port, subscribed to Push 3's Live Port (found by name; client 16 or 20). Reads every button and dial |
| `chord.go` | Shift (CC49) + Session (CC51) chord, 500 ms debounce |
| `mode.go` | Arrangement Mode on/off. ON = display takeover + MIDI intercept. OFF = give both back at once |
| `controls.go` | Jog wheel (scrub cursor; Shift = real seek), jog press (play-start anchor -- press again at the same spot to clear it), Play, Session (Back to Arrangement), Page </> (locator nav), the 3 loop/punch top buttons, loop start/length encoders (with friction, `loopEncoderTicksPerStep`), Volume knob (scroll), Tempo knob (zoom, click to switch; Delete held + press resets to fit), D-pad with repeat |
| `view.go` | Zoom and scroll state, playhead follow, `resetToFit` (Delete + Tempo press). Horizontal zoom always pivots on the playhead (or the scrub cursor, if one is active). Tracks whether each D-pad direction can still scroll further (`computeAvailability`) and tells `leds.go`'s `syncDPadLEDs` via a callback (`onScrollAvailability`) after every zoom/scroll. All values are clamped for the current set |
| `touch.go` | The touch layer: knob/jog-wheel touch shows a status bar over the arrangement. See `docs/touch-layer.md` |
| `bbs.go` | Bars.beats.16th formatting (`bbsPosition`, `bbsLength`), matching Live's own display |
| `render.go` | Draws the 960x160 image: ruler, lanes, clips, names, locators, loop (region bar + on/off boundary lines), play-start anchor (triangle), scrub cursor, playhead |
| `source.go` | Connection to the Remote Script, the current set, commands to Live. Also holds local-only state Live never reports back: the play-start anchor, the selected locator, and the scrub cursor. The playhead and the loop start/length each briefly ignore Live's report after a local edit (`hold`, `loopHold`) -- kept as two SEPARATE timers on purpose, so adjusting the loop can never freeze the playhead or vice versa |
| `model.go` | Data types (Set, Track, Clip, Locator, Loop) |
| `content.go` | Clip content cache: asks PushHack (`clips_info`) for the notes and audio info of the clips on screen, 8 per call, one call at a time, again every 4 s while drawn. Also maps a clip's beats to clip time (loops) and file seconds (warp markers) |
| `audiofile.go` | Waveform overview (min/max per 256 frames) read from WAV/AIFF files, one file at a time in the background, at most 48 files kept |
| `render_content.go` | Draws notes and waveforms inside clips, and the edit mode's outlines |
| `edit.go` | Clip edit mode: track and clip selection, encoder edits (shown first, sent on release or after a rest), the clip actions, Undo. Jobs run one at a time; each waits for the fresh arrangement |
| `leds.go` | LED blackout and the few LEDs we light (Play, Session, Page </>, the 3 loop/punch buttons, the 4 D-pad directions) |
| `depcheck.go` | Logs when push-manager or push-display is missing |
| `als.go`, `colors.go` | Reader for a saved `.als` file. Only for `make preview` and `-set`. Not used on the Push |

## How the hack takes over the Push (intercept mode)

- The hack never writes to the display memory. It sends images to push-manager (`pmclient.PushImage`).
- While the mode is on, `POST /api/midi/filter` makes push-display hide button, pad and dial MIDI from Live.
- The hack still receives that MIDI, because it has its own subscription to the Push port.
- LEDs are written straight to the Push port with our own ALSA client.

## Rules the code must keep

- Every exit path releases the display and the MIDI intercept (`modeCtl.release`).
- No access to `/dev/snd` before `alsaseq.WaitForBootSettle()` (30 s after cold boot).
- Text on the screen is ASCII only.
- The boot service starts the hack as `arrangement -config <hack.json>`. Keep accepting `-config`.
- Redraws are limited to about 8 per second (`minPushGap`). Each one is a PNG encode and an HTTP post.

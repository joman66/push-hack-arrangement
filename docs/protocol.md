# Protocol: PushHack Remote Script and the hack

The hack talks to the PushHack Remote Script (in `ableton-push-hack`, `remote-script/PushHack/`).
One JSON object per line. The full protocol is in `plans/2026-10-27-pushhack-shared-remote-script.md`
in that repo. This page lists what this hack uses.

## Endpoint

Unix socket `/tmp/push-hack.sock` (owner only). Python in Live on Windows has no Unix sockets;
A client on Windows uses TCP `127.0.0.1:7704` there.

## Start

```json
{"id":1,"cmd":"hello"}
{"id":2,"cmd":"subscribe","topics":["arrangement","position"]}
```

A reply with `"ok":false` for id 1 or 2 means the script is too old. The script then sends one event
for each topic, and again each time the value changes.

## Events (Live to hack)

`arrangement`: at most about once a second.

```json
{"t":"event","topic":"arrangement","data":{
 "tracks":[{"name":"Kick","kind":"midi","color":16711680,"group":0,
            "clips":[[0.0, 8.0, "Clip name", 5539044]]}],
 "locators":[{"time":4.0,"name":"Drop"}],
 "sig":[4,4], "tempo":124.0}}
```

- `kind`: `midi`, `audio` or `group`.
- `group`: index of the parent group track, or -1.
- A clip is `[start, end, name, color, muted]`. Times are in beats. Colors are `0xRRGGBB` as a number. `muted` (0 or 1) comes from script 0.6.0; an older script sends 4 values and the hack reads them as not muted.
- Return tracks are not sent.
- No loop or punch state here. It is in `position`, which runs every tick.

`position`: whenever any field changes.

```json
{"t":"event","topic":"position","data":{"time":12.5,"playing":true,"bta":false,
 "loop_on":true,"loop_start":8.0,"loop_length":16.0,
 "punch_in":false,"punch_out":false}}
```

`bta` is true when a track is off the arrangement, so Back to Arrangement is available.

## Commands (hack to Live)

| Command | Effect |
|---|---|
| `{"ns":"transport","cmd":"set_time","v":<beats>}` | Move the playhead (a real seek) |
| `{"ns":"transport","cmd":"play_toggle"}` | Start playing, or stop if playing |
| `{"ns":"arrangement","cmd":"bta"}` | Back to Arrangement (`song.back_to_arranger = 0`) |
| `{"ns":"arrangement","cmd":"loop_toggle"}` | Flip `song.loop` |
| `{"ns":"arrangement","cmd":"punch_in_toggle"}` | Flip `song.punch_in` |
| `{"ns":"arrangement","cmd":"punch_out_toggle"}` | Flip `song.punch_out` |
| `{"ns":"transport","cmd":"set_loop","start":<beats>,"length":<beats>}` | Move or resize the loop |

### Clip content and editing (script 0.6.0)

These are sent with an `id` (from 100 up). The hack waits for the reply (`callLive` in `source.go`, 5 s
timeout) and shows an error code as a short message. A clip is named by `track` (index) and `start`
(beats). The full list with arguments is in `docs/pushhack-remote-script.md` in `ableton-push-hack`.

| Command | Used for |
|---|---|
| `{"ns":"arrangement","cmd":"clips_info","clips":[{"track":1,"start":8}]}` | Notes, markers, audio file and warp markers of up to 8 clips on screen |
| `clip_move`, `clip_duplicate`, `clip_delete`, `clip_split`, `clip_trim`, `clip_set`, `clip_transpose`, `clip_velocity`, `clip_quantize`, `clip_crop`, `clip_new` | Clip edit mode |
| `{"ns":"arrangement","cmd":"snapshot"}` | The arrangement at once, after an edit |
| `{"ns":"transport","cmd":"undo"}` / `redo` | Undo button |

The `hello` reply carries `script_version`; the hack logs it and warns in edit mode when it is older than 0.6.0.

Each command without an `id` gets a reply (`{"ok":true}`). The hack ignores it. A failed command gets `"ok":false` and
a short `error`; the hack logs it and keeps the connection.

# RemoteAU Engine (Go)

Fork of [remote-au](https://github.com/leaperone/remote-au) (AGPL-3.0) — the
Windows-side audio engine plus the shared v2 stack compiled into the iOS app
via gomobile.

## Layout

```
cmd/remote-au           CLI: devices | selftest | recv | send | serve | recv2
cmd/remote-au-tray      Windows tray app (pure Go, no cgo)
internal/audio          pure-Go audio core (format, ring, backend registry)
internal/audio/wasapi   pure-Go WASAPI backend (loopback/mic capture, render)
internal/audio/malgo    miniaudio backend (cgo; build -tags malgo, non-Windows)
internal/codec          codec layer: PCM passthrough; Opus (-tags opus)
internal/protocol       v1 wire protocol (unchanged remote-au) + protocol/v2
internal/transport      v1 UDP/TCP transport + transport/v2 (QUIC + datagrams)
internal/discovery      v1 discovery (kept) + v2 announce marker
internal/pairing        identity keys, PIN pairing, DPAPI trust store
internal/session        v2 control-plane state machine
internal/engine         host/client orchestration (fanout, quality)
internal/quality        adaptive bitrate/FEC/jitter-target controller
internal/stats          v1 stats
mobile/                 gomobile bindings for the iOS app
```

## Building

```
go build ./...                    # default: cgo-free (pure-Go WASAPI on Windows)
go build -tags malgo ./...        # include miniaudio backend (needs gcc)
go build -tags opus ./...         # include libopus (needs libopus + gcc)
```

## Running

```
remote-au devices                  # list devices
remote-au send --source loopback   # v1: discover + stream to a receiver
remote-au recv                     # v1: receive + mix
remote-au serve                    # v2 host (QUIC, pairing, discovery)
remote-au recv2 --to <ip>:47010    # v2 test client (desktop)
```

## Testing

```
powershell -File runtests.ps1      # all packages; routes test binaries
                                   # through one path for Windows firewall
```

## Notes

- The WASAPI backend is pure Go (COM via golang.org/x/sys) so the tray app
  and CLI need no MinGW. Follows remote-au's real-time discipline: bounded
  ring, try-lock in the pull path, silence on starvation.
- The v1 discovery responder/finder is byte-compatible with stock remote-au
  (`RAUD` query/announce, ports 47001/48001/49001).

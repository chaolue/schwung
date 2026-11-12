# Network MIDI (RTP-MIDI)

Schwung can send and receive MIDI over the network using RTP-MIDI
(RFC 6295, "AppleMIDI") — the protocol built into macOS and iOS, available on
Windows through Tobias Erichsen's rtpMIDI and on Linux through `rtpmidid`.

**The network behaves as a second external MIDI port.** Nothing in a module
needs to know about it:

| Direction | What flows |
|---|---|
| Move → network | Every message that leaves the **USB-A** port: an overtake DSP's `midi_send_external`, a JS `move_midi_external_send` on cable 2, chain knob CC out, and Move's own tracks set to MIDI out. |
| Network → Move | Arrives as **cable-2 MIDI_IN**, exactly as if it had been played into USB-A: Move's tracks hear it by channel, chain slots receive it, and an overtake module gets it in `onMidiMessageExternal`. |

So, for example, the Arranger's **Output: External** route reaches the network
with no change to the module.

## Turning it on

It is **off unless its config file exists**. Create
`/data/UserData/schwung/rtpmidi.conf` (an empty file is enough) and restart
Move:

```bash
ssh ableton@move.local "touch /data/UserData/schwung/rtpmidi.conf"
```

Every key is optional:

```ini
name=Schwung             # session name shown to the other end
port=5004                # control port; data is always port+1 (UDP)
peer=172.16.254.2:5004   # invite this host; repeat for up to 8
output=1                 # Move -> network
input=1                  # network -> Move
```

A changed config takes effect at the next restart of Move (the sidecar is
replaced each time Schwung starts).

## Connecting

The sidecar listens on **every interface** — Wi-Fi and the USB-C Ethernet link
(where the Move is `172.16.254.1`) alike — and works both ways round:

- **The computer invites the Move.** In macOS *Audio MIDI Setup → MIDI Studio
  → Network*, add a session, then under *Directory* add the Move by address
  and port `5004` and press *Connect*. rtpMIDI on Windows is the same shape;
  with rtpmidid, `rtpmidid-cli connect <name> <address> 5004`.
- **The Move invites the computer.** Add `peer=<address>:<port>` to the config.
  The Move then invites that host itself and retries for ever, so nothing has
  to be clicked after either end restarts. The computer's session must accept
  incoming connections ("Anyone" in Audio MIDI Setup).

**There is no Bonjour/mDNS advertisement**, so the Move will not appear in a
session browser by itself; connect by address. Up to 8 sessions at once; all
of them receive the same output. IPv4 only.

## Things to know

- **Channels still matter.** Incoming network MIDI is external MIDI, so the
  usual rule applies: if a Move track listens and sends on the same channel,
  it echoes back out — to USB-A and to the network.
- **No recovery journal.** A datagram lost on the network is a lost message,
  as on a DIN cable. On USB-Ethernet or a wired LAN that is rare; on congested
  Wi-Fi a lost note-off is possible.
- **Latency** is the audio block the message was produced in plus up to 1 ms
  (the sidecar polls its queue every millisecond while a session is up).
- **SysEx** passes in both directions, segmented across datagrams when large.
  Outbound messages above 16 KB are dropped.
- Session up/down is written to the unified log with the `rtpmidi` source
  (`touch /data/UserData/schwung/debug_log_on`).

## How it is built

```
shim (SPI callback)                         schwung-rtpmidi (SCHED_OTHER)
  final MIDI_OUT, cable 2 ──► out ring ──►  RTP-MIDI ──► UDP ──► peers
  MIDI_IN, as cable 2     ◄── in ring  ◄──  RTP-MIDI ◄── UDP ◄── peers
                 /schwung-rtpmidi (SHM, two SPSC rings)
```

- `src/host/rtpmidi_shm.h` — the segment, the rings, the MIDI_OUT tap and the
  MIDI_IN delivery. Pure and SPI-callback-safe.
- `src/host/rtpmidi_proto.{c,h}` — the wire format, pure.
- `src/host/rtpmidi_daemon.c` — sockets and sessions. Started by
  `schwung-entry.sh` before MoveOriginal, so it inherits nothing realtime.

It is a sidecar because a socket call is a syscall, and the shim's MIDI lives
on the SPI callback. The shim's whole share is one atomic load per frame while
no session is up.

Two details that are easy to undo by accident:

- **The tap compares each slot with the previous frame.** The post-transfer
  sync copies MIDI_OUT back from hardware, so a packet can still be in its
  slot on the next frame; without the comparison it would go to the network
  twice.
- **Delivery runs in both halves of the frame and delivers the same packets
  in each** (`rtpmidi_in_shown`). The pre-ioctl half peeks, for the readers
  that run before the ioctl; the post-ioctl half pops, into both the hardware
  mailbox and Move's view. It only ever writes the first empty MIDI_IN slot —
  a zeroed slot is a terminator.

Tests: `tests/host/test_rtpmidi_proto.c` (wire format, tap, delivery) and
`tests/host/test_rtpmidi_daemon.sh`, which runs the real daemon over loopback
against both an inviting and a listening peer.

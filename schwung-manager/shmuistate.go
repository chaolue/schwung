package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"strconv"
	"syscall"
)

// ShmUIState is a read-only view of shadow_ui_state_t (/schwung-ui): the
// per-slot volume, mute, solo and patch name the shim publishes for the device
// UI's slot list.
//
// The Mixer reads these four values HERE rather than over the param channel,
// for two reasons that are both about the channel:
//
//   - Cost. A param read is a ~2.8 ms round trip on a channel shadow_ui is
//     also using; four slots x three values at 20 Hz would be the busiest
//     reader on the device. This segment costs nothing to read.
//   - Coverage. Move's own track volume, mute and solo reach a slot through
//     shadow_apply_volume / _mute / _solo on the SPI thread, which never touch
//     the web notify ring -- so a browser fed only by that ring would sit
//     still while the real level moved. Every one of those paths (and every
//     slot:* param write) already funnels through shadow_ui_state_update_slot,
//     which is what publishes this segment.
//
// Field offsets must match shadow_ui_state_t in src/host/shadow_constants.h;
// tests/host/test_manager_ui_state_offsets.sh compiles offsetof() for every
// uiOff* constant here and fails on drift.
type ShmUIState struct {
	data []byte
}

const (
	uiStateSlots   = 4  // SHADOW_UI_SLOTS
	uiStateNameLen = 64 // SHADOW_UI_NAME_LEN

	uiOffVersion       = 0   // uint32
	uiOffSlotCount     = 4   // uint8
	uiOffSlotVolumes   = 12  // uint16 x SHADOW_UI_SLOTS, percent 0-400
	uiOffSlotNames     = 24  // char[SHADOW_UI_SLOTS][SHADOW_UI_NAME_LEN]
	uiOffSlotMuted     = 280 // uint8 x SHADOW_UI_SLOTS (version >= 2)
	uiOffSlotSoloed    = 284 // uint8 x SHADOW_UI_SLOTS (version >= 2)
	uiOffSlotVolumeLin = 288 // float32 x SHADOW_UI_SLOTS (version >= 3)

	// The last byte this reader touches. A segment shorter than this is not one
	// we understand -- see OpenShmUIState. (The shim always creates the full
	// SHADOW_UI_BUFFER_SIZE, so an OLDER shim's segment is still this long;
	// the version, not the length, says which fields are filled in.)
	uiStateMinBytes = uiOffSlotVolumeLin + 4*uiStateSlots

	// SHADOW_UI_STATE_VERSION 2 appended muted/soloed. Below it those bytes are
	// zero, and zero means "not muted" -- a plausible-looking wrong answer, so
	// a v1 segment reports them as UNKNOWN instead (see UIStateSlot).
	uiStateVersionMuteSolo = 2

	// Version 3 appended the volume as a float. Before it there is only whole
	// percent, which cannot say a quiet level: -45 dB is 0.0056, i.e. 0%.
	uiStateVersionVolumeLin = 3
)

// uiStatePath is a var rather than a const so a test can point it at a file.
var uiStatePath = "/dev/shm/schwung-ui"

// ErrUIStateAbsent: the segment is not there, or not yet initialised by the
// shim (version 0). Not an error off-device; the Mixer says it is waiting.
var ErrUIStateAbsent = errors.New("slot state not available (Move may still be starting)")

// OpenShmUIState maps /schwung-ui read-only, or returns nil when it is not
// there. Never maps more than the segment holds: touching the tail of a
// mapping past EOF is SIGBUS, not a zero.
func OpenShmUIState() *ShmUIState {
	f, err := os.OpenFile(uiStatePath, os.O_RDONLY, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() < uiStateMinBytes {
		return nil
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, uiStateMinBytes,
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil
	}
	return &ShmUIState{data: data}
}

// UIStateSlot is one slot as the segment reports it. Muted and Soloed are nil
// when the segment predates them -- unknown, never "false".
type UIStateSlot struct {
	Volume float64 // linear gain, 0..4 (whole percent before version 3)
	Muted  *bool
	Soloed *bool
	Name   string
}

// Read copies the slot fields out of the segment. There is no seqlock: each
// field is a single naturally-aligned store on the shim side, so the worst a
// racing read sees is one field a frame stale, which the next poll corrects.
func (s *ShmUIState) Read() ([uiStateSlots]UIStateSlot, error) {
	var out [uiStateSlots]UIStateSlot
	if s == nil || len(s.data) < uiStateMinBytes {
		return out, ErrUIStateAbsent
	}
	return parseUIState(s.data)
}

// parseUIState is Read's body, split out so a test can hand it bytes.
func parseUIState(b []byte) ([uiStateSlots]UIStateSlot, error) {
	var out [uiStateSlots]UIStateSlot
	if len(b) < uiStateMinBytes {
		return out, ErrUIStateAbsent
	}
	version := binary.LittleEndian.Uint32(b[uiOffVersion:])
	if version == 0 {
		return out, ErrUIStateAbsent
	}
	hasMuteSolo := version >= uiStateVersionMuteSolo
	hasVolumeLin := version >= uiStateVersionVolumeLin
	for i := 0; i < uiStateSlots; i++ {
		pct := binary.LittleEndian.Uint16(b[uiOffSlotVolumes+2*i:])
		out[i].Volume = float64(pct) / 100
		if hasVolumeLin {
			f := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[uiOffSlotVolumeLin+4*i:])))
			if !math.IsNaN(f) && f >= 0 && f <= 4 {
				out[i].Volume = f
			}
		}
		name := b[uiOffSlotNames+i*uiStateNameLen : uiOffSlotNames+(i+1)*uiStateNameLen]
		if n := bytes.IndexByte(name, 0); n >= 0 {
			name = name[:n]
		}
		out[i].Name = string(name)
		if hasMuteSolo {
			m := b[uiOffSlotMuted+i] != 0
			so := b[uiOffSlotSoloed+i] != 0
			out[i].Muted, out[i].Soloed = &m, &so
		}
	}
	return out, nil
}

// formatUIVolume renders a volume for the Mixer's wire. Four places is finer
// than any fader step (a fader is 1000 steps over 60 dB) and steady enough
// that a float32 round trip does not register as a change every poll.
func formatUIVolume(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

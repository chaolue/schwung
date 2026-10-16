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
	uiStateSlots     = 8  // SHADOW_UI_SLOTS (= SHADOW_CHAIN_INSTANCES)
	uiStateBaseSlots = 4  // SHADOW_UI_STATE_BASE_SLOTS: the v1/v2 arrays' width
	uiStateNameLen   = 64 // SHADOW_UI_NAME_LEN

	// Slots 0-3 live in the v1/v2 arrays, which stay four wide forever;
	// slots 4-7 (the aux slots) live in the v3 aux block appended after them.
	uiOffVersion       = 0   // uint32
	uiOffSlotCount     = 4   // uint8
	uiOffSlotVolumes   = 12  // uint16 x 4, percent 0-400
	uiOffSlotNames     = 24  // char[4][SHADOW_UI_NAME_LEN]
	uiOffSlotMuted     = 280 // uint8 x 4 (version >= 2)
	uiOffSlotSoloed    = 284 // uint8 x 4 (version >= 2)
	uiOffAuxVolumes    = 292 // uint16 x 4, percent (version >= 3)
	uiOffAuxNames      = 304 // char[4][SHADOW_UI_NAME_LEN] (version >= 3)
	uiOffAuxMuted      = 560 // uint8 x 4 (version >= 3)
	uiOffAuxSoloed     = 564 // uint8 x 4 (version >= 3)
	uiOffSlotVolumeLin = 568 // float32 x SHADOW_UI_SLOTS (version >= 4)
	uiOffMeterSlot     = 600 // float32 x SHADOW_UI_SLOTS x 2 (version >= 5)
	uiOffMeterReturn   = 664 // float32 x 2 returns x 2 (version >= 5)
	uiOffMeterMaster   = 680 // float32 x 2 (version >= 5)
	uiOffMoveVolume    = 688 // float32 (version >= 5)
	uiOffSceneEdit     = 692 // uint8, 0xFF none (version >= 5)

	// The last byte this reader touches. A segment shorter than this is not one
	// we understand -- see OpenShmUIState. (The shim always creates the full
	// SHADOW_UI_BUFFER_SIZE; the version, not the length, says which fields
	// are filled in.)
	uiStateMinBytes = uiOffSlotVolumeLin + 4*uiStateSlots

	// The last byte the version 5 fields reach. Mapped when the segment is
	// that long; a shorter one simply has no meters.
	uiStateV5Bytes = uiOffSceneEdit + 1

	// SHADOW_UI_STATE_VERSION 2 appended muted/soloed. Below it those bytes are
	// zero, and zero means "not muted" -- a plausible-looking wrong answer, so
	// a v1 segment reports them as UNKNOWN instead (see UIStateSlot).
	uiStateVersionMuteSolo = 2

	// Version 3 appended the aux slots' block. Below it slots 4-7 are not
	// published at all, and are reported as absent rather than as empty.
	uiStateVersionAux = 3

	// Version 4 appended every slot's volume as a float. Before it there is
	// only whole percent, which cannot say a quiet level: -45 dB is 0.0056.
	uiStateVersionVolumeLin = 4

	// Version 5 appended the Mixer's meters, Move's volume and the armed
	// scene snapshot.
	uiStateVersionMeters = 5

	uiStateReturns = 2 // SHADOW_UI_SEND_RETURNS
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
	n := uiStateMinBytes
	if st.Size() >= uiStateV5Bytes {
		n = uiStateV5Bytes
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, n,
		syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil
	}
	return &ShmUIState{data: data}
}

// UIStateSlot is one slot as the segment reports it. Present is false for a
// slot the segment's version does not publish (an aux slot from a pre-aux
// shim). Muted and Soloed are nil when the segment predates them -- unknown,
// never "false".
type UIStateSlot struct {
	Present bool
	Volume  float64 // linear gain, 0..4 (whole percent before version 4)
	Muted   *bool
	Soloed  *bool
	Name    string
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
	for i := 0; i < uiStateSlots; i++ {
		// Where this slot's v1-v3 fields are, and from which version.
		volOff, nameOff, mutedOff, soloedOff, j := uiOffSlotVolumes, uiOffSlotNames,
			uiOffSlotMuted, uiOffSlotSoloed, i
		muteSoloFrom := uint32(uiStateVersionMuteSolo)
		if i >= uiStateBaseSlots {
			if version < uiStateVersionAux {
				continue // not published: absent, not empty
			}
			volOff, nameOff, mutedOff, soloedOff = uiOffAuxVolumes, uiOffAuxNames,
				uiOffAuxMuted, uiOffAuxSoloed
			j = i - uiStateBaseSlots
			muteSoloFrom = uiStateVersionAux
		}
		out[i].Present = true
		pct := binary.LittleEndian.Uint16(b[volOff+2*j:])
		out[i].Volume = float64(pct) / 100
		if version >= uiStateVersionVolumeLin {
			f := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[uiOffSlotVolumeLin+4*i:])))
			if !math.IsNaN(f) && f >= 0 && f <= 4 {
				out[i].Volume = f
			}
		}
		name := b[nameOff+j*uiStateNameLen : nameOff+(j+1)*uiStateNameLen]
		if n := bytes.IndexByte(name, 0); n >= 0 {
			name = name[:n]
		}
		out[i].Name = string(name)
		if version >= muteSoloFrom {
			m := b[mutedOff+j] != 0
			so := b[soloedOff+j] != 0
			out[i].Muted, out[i].Soloed = &m, &so
		}
	}
	return out, nil
}

// formatUIVolume renders a volume for the Mixer's wire. Four places is finer
// than any fader step (a fader is 1000 steps over 60 dB) and steady enough
// that a float32 round trip does not register as a change every poll.
func formatUIVolume(v float64) string { return strconv.FormatFloat(v, 'f', 4, 64) }

// UIMeters is the version 5 block: each strip's held peak (linear, 1.0 = full
// scale, falling 20 dB/s on the shim side), Move's volume, and the snapshot
// armed for editing (-1 none). OK is false before version 5.
type UIMeters struct {
	OK         bool
	Slots      [uiStateSlots][2]float64
	Returns    [uiStateReturns][2]float64
	Master     [2]float64
	MoveVolume float64
	SceneEdit  int
}

// ReadMeters copies the version 5 block. Like Read, no seqlock: each value is
// one aligned store, so a racing read is at worst a block stale.
func (s *ShmUIState) ReadMeters() UIMeters {
	if s == nil {
		return UIMeters{SceneEdit: -1}
	}
	return parseUIMeters(s.data)
}

func parseUIMeters(b []byte) UIMeters {
	m := UIMeters{SceneEdit: -1}
	if len(b) < uiStateV5Bytes || binary.LittleEndian.Uint32(b[uiOffVersion:]) < uiStateVersionMeters {
		return m
	}
	f := func(off int) float64 {
		v := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off:])))
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0
		}
		return v
	}
	for i := 0; i < uiStateSlots; i++ {
		m.Slots[i] = [2]float64{f(uiOffMeterSlot + 8*i), f(uiOffMeterSlot + 8*i + 4)}
	}
	for i := 0; i < uiStateReturns; i++ {
		m.Returns[i] = [2]float64{f(uiOffMeterReturn + 8*i), f(uiOffMeterReturn + 8*i + 4)}
	}
	m.Master = [2]float64{f(uiOffMeterMaster), f(uiOffMeterMaster + 4)}
	m.MoveVolume = f(uiOffMoveVolume)
	if e := b[uiOffSceneEdit]; e != 0xFF && e < 32 {
		m.SceneEdit = int(e)
	}
	m.OK = true
	return m
}

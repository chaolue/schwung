package main

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"
)

func putF32(b []byte, off int, v float32) {
	binary.LittleEndian.PutUint32(b[off:], math.Float32bits(v))
}

func meterFixture(version uint32) []byte {
	b := uiStateBytes(version, [uiStateSlots]uiSlotFixture{{volPct: 100, name: "Pad"}})
	putF32(b, uiOffMeterSlot, 0.5)     // slot 1 left
	putF32(b, uiOffMeterSlot+4, 0.25)  // slot 1 right
	putF32(b, uiOffMeterSlot+8*7+4, 1) // slot 8 right, full scale
	putF32(b, uiOffMeterReturn+8, 0.1) // return B left
	putF32(b, uiOffMeterMaster, 0.75)  // master left
	putF32(b, uiOffMeterMaster+4, float32(math.NaN()))
	putF32(b, uiOffMoveVolume, 0.5)
	b[uiOffSceneEdit] = 0xFF
	return b
}

func TestParseUIMeters(t *testing.T) {
	if m := parseUIMeters(meterFixture(4)); m.OK || m.SceneEdit != -1 {
		t.Fatalf("a version 4 segment has no meters: %+v", m)
	}
	m := parseUIMeters(meterFixture(5))
	if !m.OK || m.Slots[0] != [2]float64{0.5, 0.25} || m.Slots[7][1] != 1 || m.Returns[1][0] < 0.099 {
		t.Fatalf("meters: %+v", m)
	}
	if m.Master[0] != 0.75 || m.Master[1] != 0 {
		t.Errorf("master %v (a NaN reads as silence, never as a NaN)", m.Master)
	}
	if m.MoveVolume != 0.5 || m.SceneEdit != -1 {
		t.Errorf("move volume %v, scene %d", m.MoveVolume, m.SceneEdit)
	}
	b := meterFixture(5)
	b[uiOffSceneEdit] = 18 // B3
	if m := parseUIMeters(b); m.SceneEdit != 18 {
		t.Errorf("armed snapshot %d", m.SceneEdit)
	}
	if m := parseUIMeters(b[:uiStateMinBytes]); m.OK {
		t.Error("a segment too short for version 5 has no meters, whatever its version says")
	}
}

func TestMixerMetersEndToEnd(t *testing.T) {
	h := newMixerHarness(t, meterFixture(5))
	h.send(map[string]any{"type": "subscribe_mixer"})
	st := h.next("mixer_state", func(m map[string]any) bool { return m["type"] == "mixer_state" })
	if v, _ := globalVal(st, mixerMoveVolumeKey); v != "0.5000" {
		t.Errorf("Move's volume in the state: %q", v)
	}
	if v, _ := globalVal(st, mixerSceneEditKey); v != "-1" {
		t.Errorf("nothing armed: %q", v)
	}
	m := h.next("meters", func(m map[string]any) bool { return m["type"] == "mixer_meters" })
	slots, _ := m["slots"].([]any)
	s0, _ := slots[0].([]any)
	if len(slots) != uiStateSlots || s0[0] != 0.5 || s0[1] != 0.25 {
		t.Fatalf("meters message: %v", m)
	}

	// The shim moves a meter and arms a snapshot: both reach the browser.
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], math.Float32bits(0.125))
	if _, err := h.uiFile.WriteAt(buf[:], uiOffMeterMaster); err != nil {
		t.Fatal(err)
	}
	if _, err := h.uiFile.WriteAt([]byte{2}, uiOffSceneEdit); err != nil {
		t.Fatal(err)
	}
	// Both arrive on the same poll: the update (state) first, then the meters.
	var sawMeter, sawArmed bool
	h.next("moved meter and armed snapshot", func(m map[string]any) bool {
		if master, _ := m["master"].([]any); m["type"] == "mixer_meters" && len(master) == 2 && master[0] == 0.125 {
			sawMeter = true
		}
		if v, _ := globalVal(m, mixerSceneEditKey); v == "2" {
			sawArmed = true
		}
		return sawMeter && sawArmed
	})

	// Unchanged meters are not re-sent every poll.
	quiet, cancel := context.WithTimeout(h.ctx, 400*time.Millisecond)
	defer cancel()
	for {
		var msg map[string]any
		if err := wsjsonRead(quiet, h.conn, &msg); err != nil {
			break
		}
		if msg["type"] == "mixer_meters" {
			t.Fatalf("an unchanged meter was re-sent within the resend interval: %v", msg)
		}
	}
}

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// uiSlotFixture is one slot of a fake /schwung-ui segment.
type uiSlotFixture struct {
	volPct uint16
	volLin float32 // version >= 3 only; 0 means "use volPct/100"
	name   string
	muted  bool
	soloed bool
}

func uiStateBytes(version uint32, slots [uiStateSlots]uiSlotFixture) []byte {
	b := make([]byte, 1024) // SHADOW_UI_BUFFER_SIZE
	binary.LittleEndian.PutUint32(b[uiOffVersion:], version)
	b[uiOffSlotCount] = uiStateSlots
	for i, s := range slots {
		volOff, nameOff, mutedOff, soloedOff, j := uiOffSlotVolumes, uiOffSlotNames,
			uiOffSlotMuted, uiOffSlotSoloed, i
		if i >= uiStateBaseSlots {
			volOff, nameOff, mutedOff, soloedOff = uiOffAuxVolumes, uiOffAuxNames,
				uiOffAuxMuted, uiOffAuxSoloed
			j = i - uiStateBaseSlots
		}
		binary.LittleEndian.PutUint16(b[volOff+2*j:], s.volPct)
		copy(b[nameOff+j*uiStateNameLen:], s.name)
		if s.muted {
			b[mutedOff+j] = 1
		}
		if s.soloed {
			b[soloedOff+j] = 1
		}
		lin := s.volLin
		if lin == 0 {
			lin = float32(s.volPct) / 100
		}
		binary.LittleEndian.PutUint32(b[uiOffSlotVolumeLin+4*i:], math.Float32bits(lin))
	}
	return b
}

func TestParseUIState(t *testing.T) {
	b := uiStateBytes(2, [uiStateSlots]uiSlotFixture{
		{volPct: 100, name: "Pad"},
		{volPct: 50, name: "Bass", muted: true},
		{volPct: 400, soloed: true},
		{volPct: 0},
	})
	got, err := parseUIState(b)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Volume != 1 || got[0].Name != "Pad" || *got[0].Muted || *got[0].Soloed {
		t.Errorf("slot 0: %+v", got[0])
	}
	if got[1].Volume != 0.5 || got[1].Name != "Bass" || !*got[1].Muted {
		t.Errorf("slot 1: %+v", got[1])
	}
	if got[2].Volume != 4 || got[2].Name != "" || !*got[2].Soloed {
		t.Errorf("slot 2: %+v", got[2])
	}

	// A v1 segment has no mute/solo. Zero there means "not muted" -- a
	// plausible-looking wrong answer -- so it must come back UNKNOWN.
	v1, err := parseUIState(uiStateBytes(1, [uiStateSlots]uiSlotFixture{{volPct: 80}}))
	if err != nil {
		t.Fatal(err)
	}
	if v1[0].Muted != nil || v1[0].Soloed != nil {
		t.Error("a v1 segment must report mute/solo as unknown, not false")
	}
	if v1[0].Volume != 0.8 {
		t.Errorf("v1 volume: %v", v1[0].Volume)
	}

	// Version 4 carries the float, which is the only way a quiet level is
	// readable at all: -45 dB is 0.0056, which is 0 in whole percent. It
	// covers the aux slots too (one array indexed by slot).
	quiet := [uiStateSlots]uiSlotFixture{{volPct: 0, volLin: 0.0056}}
	quiet[6] = uiSlotFixture{volPct: 0, volLin: 0.0056, name: "Aux"}
	v4, err := parseUIState(uiStateBytes(4, quiet))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(v4[0].Volume-0.0056) > 1e-6 || math.Abs(v4[6].Volume-0.0056) > 1e-6 {
		t.Errorf("v4 must read the float volume, got %v / %v", v4[0].Volume, v4[6].Volume)
	}
	// A v3 segment with the same bytes must NOT read the float: it is not
	// there yet on a v3 shim, whatever the bytes happen to hold.
	v3, _ := parseUIState(uiStateBytes(3, quiet))
	if v3[0].Volume != 0 || v3[6].Volume != 0 {
		t.Errorf("v3 must read whole percent, got %v / %v", v3[0].Volume, v3[6].Volume)
	}
	if !v3[6].Present || v3[6].Name != "Aux" {
		t.Errorf("v3 publishes the aux slots: %+v", v3[6])
	}
	// The aux slots come from their own block, not the base arrays.
	aux := [uiStateSlots]uiSlotFixture{{volPct: 100, name: "Base"}}
	aux[5] = uiSlotFixture{volPct: 30, name: "Six", muted: true}
	a3, _ := parseUIState(uiStateBytes(3, aux))
	if a3[5].Volume != 0.3 || a3[5].Name != "Six" || a3[5].Muted == nil || !*a3[5].Muted {
		t.Errorf("aux slot 6 read wrong: %+v", a3[5])
	}
	if a3[1].Name != "" || (a3[1].Muted != nil && *a3[1].Muted) {
		t.Errorf("aux data leaked into base slot 2: %+v", a3[1])
	}
	// Before v3 the aux slots are not published: absent, not empty.
	v2, _ := parseUIState(uiStateBytes(2, aux))
	if v2[5].Present || !v2[0].Present {
		t.Errorf("v2: aux must be absent, base present (%+v / %+v)", v2[5], v2[0])
	}

	// Version 0: the shim has not initialised it yet.
	if _, err := parseUIState(make([]byte, 1024)); !errors.Is(err, ErrUIStateAbsent) {
		t.Errorf("version 0 must be absent, got %v", err)
	}
	// Short: never read past what is there.
	if _, err := parseUIState(make([]byte, uiStateMinBytes-1)); !errors.Is(err, ErrUIStateAbsent) {
		t.Errorf("a short segment must be absent, got %v", err)
	}
}

func TestOpenShmUIStateRefusesShortSegment(t *testing.T) {
	dir := t.TempDir()
	orig := uiStatePath
	uiStatePath = filepath.Join(dir, "schwung-ui")
	t.Cleanup(func() { uiStatePath = orig })

	if OpenShmUIState() != nil {
		t.Fatal("no file: want nil")
	}
	// Mapping past EOF is SIGBUS on first touch, so a short segment is refused.
	if err := os.WriteFile(uiStatePath, make([]byte, uiStateMinBytes-1), 0o644); err != nil {
		t.Fatal(err)
	}
	if OpenShmUIState() != nil {
		t.Fatal("short segment: want nil")
	}
	if err := os.WriteFile(uiStatePath, uiStateBytes(2, [uiStateSlots]uiSlotFixture{{volPct: 25}}), 0o644); err != nil {
		t.Fatal(err)
	}
	s := OpenShmUIState()
	if s == nil {
		t.Fatal("full segment: want a mapping")
	}
	got, err := s.Read()
	if err != nil || got[0].Volume != 0.25 {
		t.Fatalf("read: %v %+v", err, got[0])
	}
}

// A slow READ must not overwrite a value that was NOTIFIED while it was in
// flight -- the notify is newer, and the fader would jump back.
func TestMixerStoreReadCannotOvertakeNotify(t *testing.T) {
	var m mixerStore
	since := m.begin()
	if !m.put("0|slot:pan", "0.5") { // notified mid-read
		t.Fatal("first put must report a change")
	}
	if m.putRead("0|slot:pan", "-1.000", since) {
		t.Fatal("a read that began before the notify must not land")
	}
	if v, _ := m.get("0|slot:pan"); v != "0.5" {
		t.Fatalf("got %q, want the notified 0.5", v)
	}
	// A read that began AFTER it does land.
	if !m.putRead("0|slot:pan", "0.25", m.begin()) {
		t.Fatal("a later read must land")
	}
	d := m.takeDirty()
	if d["0|slot:pan"] != "0.25" || len(d) != 1 {
		t.Fatalf("dirty: %v", d)
	}
	if m.takeDirty() != nil {
		t.Fatal("takeDirty must clear")
	}
	if m.put("0|slot:pan", "0.25") {
		t.Fatal("an unchanged value is not a change")
	}
}

func TestMixerMessageShape(t *testing.T) {
	msg := mixerMessage("mixer_update", map[string]string{
		"0|slot:volume":  "1.00",
		"3|slot:pan":     "-0.5",
		"g|send1:return": "64",
		"garbage":        "x",
	})
	if msg.Slots["0"]["slot:volume"] != "1.00" || msg.Slots["3"]["slot:pan"] != "-0.5" {
		t.Errorf("slots: %v", msg.Slots)
	}
	if msg.Global["send1:return"] != "64" {
		t.Errorf("global: %v", msg.Global)
	}
	if len(msg.Slots) != 2 || len(msg.Global) != 1 {
		t.Errorf("a key with no scope must be dropped: %+v", msg)
	}
}

func TestMixerWriteAllowlist(t *testing.T) {
	cases := []struct {
		key, value     string
		perSlot, keyOK bool
		valueOK        bool
	}{
		{"slot:volume", "0.75", true, true, true},
		{"slot:volume", "4", true, true, true},
		{"slot:volume", "4.01", true, true, false},
		{"slot:volume", "-0.1", true, true, false},
		{"slot:volume", "NaN", true, true, false},
		{"slot:pan", "-1", true, true, true},
		{"slot:pan", "1.5", true, true, false},
		{"slot:muted", "1", true, true, true},
		{"slot:muted", "true", true, true, false},
		{"slot:soloed", "0", true, true, true},
		{"buses:main_send1", "127", true, true, true},
		{"buses:main_send2", "128", true, true, false},
		{"buses:main_send2", "64.5", true, true, false},
		{"send1:return", "0", false, true, true},
		{"send2:return", "90", false, true, true},
		{"send1:to_send2", "12", false, true, true},
		{"send2:to_send2", "12", false, false, false}, // only A feeds B
		{"synth:cutoff", "0.5", false, false, false},  // not a mixer key
		{"master_fx:fx1:mix", "0.5", false, false, false},
	}
	for _, c := range cases {
		perSlot, ok := mixerWriteScope(c.key)
		if ok != c.keyOK || (ok && perSlot != c.perSlot) {
			t.Errorf("%s: scope perSlot=%v ok=%v", c.key, perSlot, ok)
		}
		if ok && validMixerValue(c.key, c.value) != c.valueOK {
			t.Errorf("%s=%q: valid=%v want %v", c.key, c.value, !c.valueOK, c.valueOK)
		}
	}
}

func TestNormalizeMixerValue(t *testing.T) {
	cases := []struct {
		key, in, want string
		ok            bool
	}{
		{"buses:main_send1", "64", "64", true},
		{"buses:main_send1", "64.0", "64", true}, // the shim atoi()s it
		{"send1:return", "127", "127", true},
		{"send1:return", "200", "", false},
		{"slot:pan", "0.250000", "0.250000", true},
		{"slot:pan", "", "", false}, // served-and-empty is not a value
		{"slot:pan", "abc", "", false},
		{"slot:muted", "1", "1", true},
		{"send1:to_send2", "-3", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeMixerValue(c.key, c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s %q: got %q,%v want %q,%v", c.key, c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestMixerReadKeysCoverEverySlot(t *testing.T) {
	all := mixerReadKeys(-1)
	if len(all) != mixerSlots*len(mixerSlotParamKeys)+len(mixerGlobalKeys) {
		t.Fatalf("got %d keys", len(all))
	}
	one := mixerReadKeys(2)
	if len(one) != len(mixerSlotParamKeys) {
		t.Fatalf("slot 2: %d keys", len(one))
	}
	for _, k := range one {
		if k.slot != 2 {
			t.Errorf("slot 2 refresh read %+v", k)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end: a real WebSocket, a fake /schwung-ui, notifications injected.
// ---------------------------------------------------------------------------

type mixerHarness struct {
	t      *testing.T
	ru     *RemoteUI
	conn   *websocket.Conn
	ctx    context.Context
	uiFile *os.File
}

func newMixerHarness(t *testing.T, initial []byte) *mixerHarness {
	t.Helper()
	dir := t.TempDir()

	origUI, origParam := uiStatePath, shmParamPath
	uiStatePath = filepath.Join(dir, "schwung-ui")
	shmParamPath = filepath.Join(dir, "no-param-segment") // reads find no channel

	if err := os.WriteFile(uiStatePath, initial, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(uiStatePath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := &App{logger: logger}
	ru := NewRemoteUI(app, nil, dir, logger)

	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() { ru.mixerLoop(ctx); close(loopDone) }()

	srv := httptest.NewServer(ru)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}

	// ONE cleanup, in order: nothing may still be reading the segment paths
	// when they are put back, or the restore races a background read.
	t.Cleanup(func() {
		conn.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		cancel()
		<-loopDone
		ru.mix.waitIdle()
		f.Close()
		uiStatePath, shmParamPath = origUI, origParam
	})
	return &mixerHarness{t: t, ru: ru, conn: conn, ctx: ctx, uiFile: f}
}

func (h *mixerHarness) send(v any) {
	h.t.Helper()
	if err := wsjson.Write(h.ctx, h.conn, v); err != nil {
		h.t.Fatal(err)
	}
}

// next reads messages until one satisfies want, or fails after a timeout.
func (h *mixerHarness) next(what string, want func(map[string]any) bool) map[string]any {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
	defer cancel()
	for {
		var m map[string]any
		if err := wsjson.Read(ctx, h.conn, &m); err != nil {
			h.t.Fatalf("waiting for %s: %v", what, err)
		}
		if want(m) {
			return m
		}
	}
}

func slotVal(m map[string]any, slot, key string) (string, bool) {
	slots, _ := m["slots"].(map[string]any)
	s, _ := slots[slot].(map[string]any)
	v, ok := s[key].(string)
	return v, ok
}

func globalVal(m map[string]any, key string) (string, bool) {
	g, _ := m["global"].(map[string]any)
	v, ok := g[key].(string)
	return v, ok
}

func TestMixerEndToEnd(t *testing.T) {
	h := newMixerHarness(t, uiStateBytes(4, [uiStateSlots]uiSlotFixture{
		{volPct: 100, name: "Pad"},
		{volPct: 50, name: "Bass", muted: true},
		{volPct: 70},
		{volPct: 100, soloed: true},
	}))
	h.send(map[string]any{"type": "subscribe_mixer"})

	// The full state, from /schwung-ui alone: there is no param channel here.
	state := h.next("mixer_state", func(m map[string]any) bool { return m["type"] == "mixer_state" })
	if live, _ := state["live"].(bool); !live {
		t.Fatal("mixer_state must say the slot state is live")
	}
	if v, _ := slotVal(state, "0", "slot:volume"); v != "1.0000" {
		t.Errorf("slot 0 volume %q", v)
	}
	if v, _ := slotVal(state, "0", "name"); v != "Pad" {
		t.Errorf("slot 0 name %q", v)
	}
	if v, _ := slotVal(state, "1", "slot:muted"); v != "1" {
		t.Errorf("slot 1 muted %q", v)
	}
	if v, _ := slotVal(state, "3", "slot:soloed"); v != "1" {
		t.Errorf("slot 3 soloed %q", v)
	}
	// Never read: UNKNOWN, i.e. absent -- not a 0 the browser would draw.
	if _, ok := slotVal(state, "0", "slot:pan"); ok {
		t.Error("a value that was never read must be absent from mixer_state")
	}
	if _, ok := globalVal(state, "send1:return"); ok {
		t.Error("an unread return must be absent from mixer_state")
	}

	// Move turns slot 1 up: only /schwung-ui knows. The browser must hear it.
	var lin [4]byte
	binary.LittleEndian.PutUint32(lin[:], math.Float32bits(0.75))
	if _, err := h.uiFile.WriteAt(lin[:], uiOffSlotVolumeLin+4*1); err != nil {
		t.Fatal(err)
	}
	h.next("slot 1 volume update", func(m map[string]any) bool {
		v, _ := slotVal(m, "1", "slot:volume")
		return m["type"] == "mixer_update" && v == "0.7500"
	})

	// Move unmutes it.
	if _, err := h.uiFile.WriteAt([]byte{0}, uiOffSlotMuted+1); err != nil {
		t.Fatal(err)
	}
	h.next("slot 1 unmute", func(m map[string]any) bool {
		v, _ := slotVal(m, "1", "slot:muted")
		return m["type"] == "mixer_update" && v == "0"
	})

	// Notified writes: a pan and a return (normalised the way the shim reads it).
	h.ru.mixerNotify(h.ctx, 2, "slot:pan", "-0.5")
	h.next("slot 2 pan", func(m map[string]any) bool {
		v, _ := slotVal(m, "2", "slot:pan")
		return v == "-0.5"
	})
	h.ru.mixerNotify(h.ctx, 0, "send1:return", "64.0")
	h.next("send1 return", func(m map[string]any) bool {
		v, _ := globalVal(m, "send1:return")
		return v == "64"
	})

	// slot:volume on the notify ring is IGNORED: /schwung-ui owns it, and a
	// second source at a different precision would move the fader back and forth.
	h.ru.mixerNotify(h.ctx, 1, "slot:volume", "0.3")
	time.Sleep(3 * mixerPollInterval)
	if v, _ := h.ru.mix.store.get(mixerStoreKey(1, "slot:volume")); v != "0.7500" {
		t.Errorf("slot 1 volume became %q from the notify ring; /schwung-ui says 0.75", v)
	}

	// Writes are allowlisted and range-checked before anything is sent.
	for _, bad := range []map[string]any{
		{"type": "set_mixer_param", "slot": 0, "key": "synth:cutoff", "value": "0.5"},
		{"type": "set_mixer_param", "slot": 0, "key": "slot:volume", "value": "9"},
		{"type": "set_mixer_param", "key": "slot:volume", "value": "0.5"}, // no slot
		{"type": "set_mixer_param", "slot": 8, "key": "slot:pan", "value": "0"},
	} {
		h.send(bad)
		h.next("error for "+bad["key"].(string), func(m map[string]any) bool {
			msg, _ := m["message"].(string)
			return m["type"] == "error" && strings.Contains(msg, "set_mixer_param")
		})
	}
}

// A resubscribe (tab switch, reconnect) must get a FULL state again rather than
// deltas against a model it no longer holds.
func TestMixerResubscribeGetsFullState(t *testing.T) {
	h := newMixerHarness(t, uiStateBytes(4, [uiStateSlots]uiSlotFixture{{volPct: 100, name: "A"}}))
	h.send(map[string]any{"type": "subscribe_mixer"})
	h.next("first state", func(m map[string]any) bool { return m["type"] == "mixer_state" })
	h.send(map[string]any{"type": "unsubscribe_mixer"})
	h.send(map[string]any{"type": "subscribe_mixer"})
	st := h.next("second state", func(m map[string]any) bool { return m["type"] == "mixer_state" })
	if v, _ := slotVal(st, "0", "name"); v != "A" {
		t.Errorf("resubscribe state lost the name: %q", v)
	}
}

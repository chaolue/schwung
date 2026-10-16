package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nhooyr.io/websocket"
)

func writeActiveSet(t *testing.T, base, uuid string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(base, "active_set.txt"), []byte(uuid+"\nSet 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPinsFileRoundTrip(t *testing.T) {
	base := t.TempDir()
	if _, err := readPins(base); err == nil {
		t.Fatal("no active set must be an error, not an empty pin list to write over")
	}
	writeActiveSet(t, base, "abc-123")
	doc, err := readPins(base)
	if err != nil || len(doc.Slots) != 0 {
		t.Fatalf("a set with no pins file has no pins: %+v %v", doc, err)
	}
	doc.Slots["2"] = []pinEntry{{Key: "synth:cutoff", Module: "obxd"}}
	if err := writePins(base, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "set_state", "abc-123", "mixer_pins.json")); err != nil {
		t.Fatalf("pins live with the set: %v", err)
	}
	back, err := readPins(base)
	if err != nil || len(back.Slots["2"]) != 1 || back.Slots["2"][0].Module != "obxd" {
		t.Fatalf("round trip: %+v %v", back, err)
	}
	// A different set has its own.
	writeActiveSet(t, base, "other")
	if other, _ := readPins(base); len(other.Slots) != 0 {
		t.Fatalf("pins leaked across sets: %+v", other)
	}
	// A path in active_set.txt is never followed.
	writeActiveSet(t, base, "../escape")
	if _, err := readPins(base); err == nil {
		t.Fatal("a set uuid with a path in it must be refused")
	}
}

func TestPinMixerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeActiveSet(t, dir, "set-1")
	origUI, origParam := uiStatePath, shmParamPath
	uiStatePath = filepath.Join(dir, "schwung-ui")
	shmParamPath = filepath.Join(dir, "schwung-param")
	if err := os.WriteFile(uiStatePath, uiStateBytes(4, [uiStateSlots]uiSlotFixture{{volPct: 100}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shmParamPath, make([]byte, shmParamSize), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := &tableShim{settings: true, vals: map[string]string{
		"1/chain_modules":      `{"synth":"obxd","midi_fx":[],"fx":["verb"]}`,
		"1/synth:chain_params": `[{"key":"cutoff","name":"Cutoff","type":"float","min":0,"max":1},{"key":"res","name":"Res","type":"float","min":0,"max":1},{"key":"a","name":"A","type":"float","min":0,"max":1},{"key":"b","name":"B","type":"float","min":0,"max":1},{"key":"mode","name":"Mode","type":"enum","options":["x","y"]}]`,
		"1/fx1:chain_params":   verbParams,
		"1/synth:cutoff":       "0.25",
		"1/fx1:wet":            "0.4",
		"1/knob_map":           `[]`,
	}}
	for s := 0; s < mixerSlots; s++ {
		if s != 1 {
			ts.vals[string(rune('0'+s))+"/chain_modules"] = `{"synth":"","midi_fx":[],"fx":[]}`
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ru := NewRemoteUI(&App{logger: logger}, nil, dir, logger)
	ctx, cancel := context.WithCancel(context.Background())
	shimDone := make(chan struct{})
	go func() { ts.serve(ctx, t, shmParamPath); close(shimDone) }()
	loopDone := make(chan struct{})
	go func() { ru.mixerLoop(ctx); close(loopDone) }()
	srv := httptest.NewServer(ru)
	dial := func() *websocket.Conn {
		c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	slotConn, mixConn := dial(), dial()
	t.Cleanup(func() {
		slotConn.Close(websocket.StatusNormalClosure, "")
		mixConn.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		cancel()
		<-loopDone
		ru.mix.waitIdle()
		<-shimDone
		uiStatePath, shmParamPath = origUI, origParam
	})
	slotTab := &mixerHarness{t: t, ru: ru, conn: slotConn, ctx: ctx}
	mixer := &mixerHarness{t: t, ru: ru, conn: mixConn, ctx: ctx}

	slotTab.send(map[string]any{"type": "subscribe", "slot": 1})
	slotTab.next("initial pins", func(m map[string]any) bool { return m["type"] == "mixer_pins" })
	mixer.send(map[string]any{"type": "subscribe_mixer"})
	knobs := func(m map[string]any) []map[string]any {
		fx, _ := m["fx"].(map[string]any)
		ks, _ := fx["1"].([]any)
		var out []map[string]any
		for _, k := range ks {
			out = append(out, k.(map[string]any))
		}
		return out
	}
	mixer.next("the effect knob", func(m map[string]any) bool { return len(knobs(m)) == 1 })

	// Pin the synth's cutoff from the Slot tab.
	slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "synth:cutoff", "value": "1"})
	slotTab.next("pins broadcast", func(m map[string]any) bool {
		ks, _ := m["keys"].([]any)
		return m["type"] == "mixer_pins" && len(ks) == 1 && ks[0] == "synth:cutoff"
	})
	m := mixer.next("the pinned knob on the strip", func(m map[string]any) bool { return len(knobs(m)) == 2 })
	pinned := knobs(m)[1]
	if pinned["key"] != "synth:cutoff" || pinned["pinned"] != true || pinned["module"] != "obxd" {
		t.Fatalf("pinned knob: %v", pinned)
	}
	mixer.next("its value", func(m map[string]any) bool { v, _ := slotVal(m, "1", "synth:cutoff"); return v == "0.25" })
	raw, _ := os.ReadFile(filepath.Join(dir, "set_state", "set-1", "mixer_pins.json"))
	var doc pinDoc
	if json.Unmarshal(raw, &doc) != nil || len(doc.Slots["1"]) != 1 || doc.Slots["1"][0].Module != "obxd" {
		t.Fatalf("pins file: %s", raw)
	}

	// Refusals: an enum, an empty position, and a fifth pin.
	for _, bad := range []struct{ key, why string }{
		{"synth:mode", "not a numeric parameter"},
		{"fx4:wet", "nothing is loaded"},
	} {
		slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": bad.key, "value": "1"})
		slotTab.next(bad.key+" refused", func(m map[string]any) bool {
			msg, _ := m["message"].(string)
			return m["type"] == "error" && strings.Contains(msg, bad.why)
		})
	}
	for _, k := range []string{"synth:res", "synth:a", "synth:b"} {
		slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": k, "value": "1"})
	}
	slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "fx1:size", "value": "1"})
	slotTab.next("the cap", func(m map[string]any) bool {
		msg, _ := m["message"].(string)
		return m["type"] == "error" && strings.Contains(msg, "at most 4")
	})

	// The synth is swapped: its pins hide (never point at the new module),
	// and come back with it.
	ts.set("1/chain_modules", `{"synth":"braids","midi_fx":[],"fx":["verb"]}`)
	ru.mixerNotify(ctx, 1, "synth:module", "braids")
	mixer.next("pins hidden after the swap", func(m map[string]any) bool {
		ks := knobs(m)
		return m["fx"] != nil && len(ks) == 1 && ks[0]["key"] == "fx1:wet"
	})
	ts.set("1/chain_modules", `{"synth":"obxd","midi_fx":[],"fx":["verb"]}`)
	ru.mixerNotify(ctx, 1, "synth:module", "obxd")
	mixer.next("pins back with the module", func(m map[string]any) bool { return len(knobs(m)) == 5 })

	// Unpin from the Mixer.
	mixer.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "synth:cutoff", "value": "0"})
	mixer.next("unpinned", func(m map[string]any) bool {
		ks := knobs(m)
		if m["fx"] == nil || len(ks) != 4 {
			return false
		}
		for _, k := range ks {
			if k["key"] == "synth:cutoff" {
				return false
			}
		}
		return true
	})
}

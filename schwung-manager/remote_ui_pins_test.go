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
	"time"

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
	synthParams := `[{"key":"cutoff","name":"Cutoff","type":"float","min":0,"max":1}`
	for _, k := range []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"} {
		synthParams += `,{"key":"` + k + `","name":"` + k + `","type":"float","min":0,"max":1}`
	}
	synthParams += `,{"key":"mode","name":"Mode","type":"enum","options":["Saw","Square","Noise"]}` +
		`,{"key":"free","name":"Free","type":"float"}]`
	ts := &tableShim{settings: true, vals: map[string]string{
		"1/chain_modules":      `{"synth":"obxd","midi_fx":[],"fx":["verb"]}`,
		"1/synth:chain_params": synthParams,
		"1/fx1:chain_params":   verbParams,
		"1/synth:cutoff":       "0.25",
		"1/synth:mode":         "Square", // a choice reported by NAME
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
	knobs := func(m map[string]any) []map[string]any {
		fx, _ := m["fx"].(map[string]any)
		ks, _ := fx["1"].([]any)
		var out []map[string]any
		for _, k := range ks {
			out = append(out, k.(map[string]any))
		}
		return out
	}
	keysOf := func(m map[string]any) []string {
		var out []string
		ks, _ := m["keys"].([]any)
		for _, k := range ks {
			out = append(out, k.(string))
		}
		return out
	}
	pinsFile := func() pinDoc {
		raw, _ := os.ReadFile(filepath.Join(dir, "set_state", "set-1", "mixer_pins.json"))
		var doc pinDoc
		_ = json.Unmarshal(raw, &doc)
		return doc
	}

	slotTab.send(map[string]any{"type": "subscribe", "slot": 1})
	slotTab.next("initial pins", func(m map[string]any) bool { return m["type"] == "mixer_pins" })

	// Opening the Mixer pins the effect's mix BY DEFAULT -- an ordinary pin,
	// so the Slot tab hears about it too.
	mixer.send(map[string]any{"type": "subscribe_mixer"})
	m := mixer.next("the effect's default pin", func(m map[string]any) bool { return len(knobs(m)) == 1 })
	if k := knobs(m)[0]; k["key"] != "fx1:wet" || k["pinned"] != true || k["effect"] != true {
		t.Fatalf("default pin: %v", k)
	}
	slotTab.next("default pin broadcast", func(m map[string]any) bool {
		return m["type"] == "mixer_pins" && containsKey(keysOf(m), "fx1:wet")
	})

	// Pin the synth's cutoff, and a CHOICE parameter.
	slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "synth:cutoff", "value": "1"})
	slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "synth:mode", "value": "1"})
	m = mixer.next("three knobs", func(m map[string]any) bool { return len(knobs(m)) == 3 })
	ks := knobs(m)
	if ks[0]["key"] != "fx1:wet" || ks[1]["key"] != "synth:cutoff" || ks[1]["effect"] == true {
		t.Fatalf("order and groups: %v", ks)
	}
	if opts, _ := ks[2]["options"].([]any); ks[2]["key"] != "synth:mode" || len(opts) != 3 || ks[2]["max"] != 2.0 {
		t.Fatalf("a choice pin: %v", ks[2])
	}
	mixer.next("cutoff's value", func(m map[string]any) bool { v, _ := slotVal(m, "1", "synth:cutoff"); return v == "0.25" })
	mixer.next("the choice's value, as an index", func(m map[string]any) bool { v, _ := slotVal(m, "1", "synth:mode"); return v == "1" })
	if doc := pinsFile(); len(doc.Slots["1"]) != 3 || doc.Slots["1"][1].Module != "obxd" {
		t.Fatalf("pins file: %+v", doc)
	}

	// Refusals: a parameter with no range, an empty position.
	for _, bad := range []struct{ key, why string }{
		{"synth:free", "the Mixer can show"},
		{"fx4:wet", "nothing is loaded"},
	} {
		slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": bad.key, "value": "1"})
		slotTab.next(bad.key+" refused", func(m map[string]any) bool {
			msg, _ := m["message"].(string)
			return m["type"] == "error" && strings.Contains(msg, bad.why)
		})
	}
	// The cap counts every pin: three so far, five more fill it.
	for _, k := range []string{"p1", "p2", "p3", "p4", "p5"} {
		slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "synth:" + k, "value": "1"})
	}
	slotTab.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "synth:p6", "value": "1"})
	slotTab.next("the cap", func(m map[string]any) bool {
		msg, _ := m["message"].(string)
		return m["type"] == "error" && strings.Contains(msg, "at most 8")
	})

	// Unpin the effect's DEFAULT pin: it stays gone, however often the slot
	// is read again.
	mixer.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "fx1:wet", "value": "0"})
	mixer.next("default unpinned", func(m map[string]any) bool { return m["fx"] != nil && len(knobs(m)) == 7 })
	ru.mixerScheduleModules(ctx, 1, 0)
	ru.mixerScheduleModules(ctx, 1, 50)
	time.Sleep(300 * time.Millisecond)
	if doc := pinsFile(); containsKey(pinKeys(doc.Slots["1"]), "fx1:wet") {
		t.Fatal("an unpinned default came back on a re-read")
	}

	// A DIFFERENT effect in that position is newly added: it gets its own
	// default pin. The synth is swapped at the same time, which hides its pins.
	ts.set("1/chain_modules", `{"synth":"braids","midi_fx":[],"fx":["echo"]}`)
	ts.set("1/fx1:chain_params", `[{"key":"mix","name":"Mix","type":"float","min":0,"max":1}]`)
	ts.set("1/fx1:mix", "0.6")
	ru.mixerNotify(ctx, 1, "fx1:module", "echo")
	m = mixer.next("the new effect's default, the synth's pins hidden", func(m map[string]any) bool {
		ks := knobs(m)
		return m["fx"] != nil && len(ks) == 1 && ks[0]["key"] == "fx1:mix"
	})
	_ = m

	// Unpin from the Mixer.
	ts.set("1/chain_modules", `{"synth":"obxd","midi_fx":[],"fx":["echo"]}`)
	ru.mixerNotify(ctx, 1, "synth:module", "obxd")
	mixer.next("synth pins back", func(m map[string]any) bool { return len(knobs(m)) == 8 })
	mixer.send(map[string]any{"type": "pin_mixer", "slot": 1, "key": "synth:cutoff", "value": "0"})
	mixer.next("unpinned", func(m map[string]any) bool {
		ks := knobs(m)
		if m["fx"] == nil || len(ks) != 7 {
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

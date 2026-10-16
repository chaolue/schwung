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

func TestBusKeysAndStrips(t *testing.T) {
	for _, c := range []struct {
		key, bus, comp, param string
		ok                    bool
	}{
		{"send1:fx2:mix", "send1", "fx2", "mix", true},
		{"master_fx:fx8:wet", "master_fx", "fx8", "wet", true},
		{"send2:fx1:a:b", "send2", "fx1", "a:b", true},
		{"send1:return", "", "", "", false},  // a level, not a position
		{"send1:fx9:mix", "", "", "", false}, // past the cap
		{"send3:fx1:mix", "", "", "", false},
		{"fx1:mix", "", "", "", false},
		{"send1:fx1:", "", "", "", false},
	} {
		bus, comp, param, ok := splitBusKey(c.key)
		if ok != c.ok || bus.ID != c.bus || comp != c.comp || param != c.param {
			t.Errorf("splitBusKey(%q) = %q %q %q %v", c.key, bus.ID, comp, param, ok)
		}
	}

	if mixerStrips != mixerSlots+3 {
		t.Fatalf("strips: %d", mixerStrips)
	}
	send2 := busStrip(fxBus{ID: "send2"})
	if stripName(3) != "3" || stripName(send2) != "send2" || readSlotOf(send2) != 0 || readSlotOf(5) != 5 {
		t.Error("strip names / read slots")
	}
	if stripComp(send2, "fx1") != "send2:fx1" || stripComp(2, "fx1") != "fx1" {
		t.Error("stripComp")
	}
	if c, p, ok := splitStripKey(send2, "send2:fx3:mix"); !ok || c != "fx3" || p != "mix" {
		t.Error("splitStripKey on a bus")
	}
	if _, _, ok := splitStripKey(send2, "send1:fx3:mix"); ok {
		t.Error("another bus's key is not this strip's")
	}
	if _, _, ok := splitStripKey(2, "send1:fx3:mix"); ok {
		t.Error("a bus key is not a slot's")
	}
	// A bus knob's value is global: its key names its bus.
	if mixerStoreKey(send2, "send2:fx1:mix") != "g|send2:fx1:mix" {
		t.Error("bus store key")
	}

	for _, c := range []struct {
		slot      uint8
		key, want string
	}{
		{0, "send1:fx1:wet", "send1"},
		{0, "send1:return", ""}, // the Mixer's
		{0, "master_fx:fx2:mix", "master_fx"},
		{0, "master_fx:lfo1:depth", "master_fx"}, // every master_fx: key, as before
		{2, "send1:fx1:wet", ""},
		{0, "fx1:wet", ""},
	} {
		if got := notifyBusOf(c.slot, c.key); got != c.want {
			t.Errorf("notifyBusOf(%d, %q) = %q, want %q", c.slot, c.key, got, c.want)
		}
	}
}

// The Send A/B tabs and the Mixer's bus strips, against a stand-in shim.
func TestBusTabsAndMixerStrips(t *testing.T) {
	dir := t.TempDir()
	writeActiveSet(t, dir, "set-bus")
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
		// Send A holds a reverb (its wet is the guessed mix); Send B nothing;
		// Master FX a compressor with no mix at all.
		"0/send1:modules":              `[{"id":"verb","path":"/m/audio_fx/verb/verb.so"},{"id":"","path":""}]`,
		"0/send1:fx1:chain_params":     verbParams,
		"0/send1:fx1:ui_hierarchy":     `{"levels":{"root":{"knobs":["size","wet"]}}}`,
		"0/send1:fx1:wet":              "0.5",
		"0/send1:fx1:size":             "0.2",
		"0/send2:modules":              `[]`,
		"0/master_fx:modules":          `[{"id":"comp","path":""}]`,
		"0/master_fx:fx1:chain_params": `[{"key":"thresh","name":"Threshold","type":"float","min":-60,"max":0}]`,
		"0/master_fx:fx1:ui_hierarchy": `{"levels":{"root":{"knobs":["thresh"]}}}`,
		"0/master_fx:fx1:thresh":       "-12",
	}}
	for s := 0; s < mixerSlots; s++ {
		ts.vals[string(rune('0'+s))+"/chain_modules"] = `{"synth":"","midi_fx":[],"fx":[]}`
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
	sendConn, masterConn, mixConn := dial(), dial(), dial()
	t.Cleanup(func() {
		sendConn.Close(websocket.StatusNormalClosure, "")
		masterConn.Close(websocket.StatusNormalClosure, "")
		mixConn.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		cancel()
		<-loopDone
		ru.mix.waitIdle()
		<-shimDone
		uiStatePath, shmParamPath = origUI, origParam
	})
	sendTab := &mixerHarness{t: t, ru: ru, conn: sendConn, ctx: ctx}
	masterTab := &mixerHarness{t: t, ru: ru, conn: masterConn, ctx: ctx}
	mixer := &mixerHarness{t: t, ru: ru, conn: mixConn, ctx: ctx}
	errorWith := func(text string) func(map[string]any) bool {
		return func(m map[string]any) bool {
			msg, _ := m["message"].(string)
			return m["type"] == "error" && strings.Contains(msg, text)
		}
	}
	knobsOf := func(m map[string]any, strip string) []map[string]any {
		fx, _ := m["fx"].(map[string]any)
		ks, _ := fx[strip].([]any)
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

	// ---- The Send A tab: what is loaded, its controls, its values. ----
	sendTab.send(map[string]any{"type": "subscribe_bus", "key": "send1"})
	m := sendTab.next("send A's modules", func(m map[string]any) bool { return m["type"] == "master_fx_info" })
	if mods, _ := m["modules"].(map[string]any); m["bus"] != "send1" || mods["fx1"] != "verb" || len(mods) != 1 {
		t.Fatalf("bus info: %v", m)
	}
	sendTab.next("send A's hierarchy", func(m map[string]any) bool {
		return m["type"] == "hierarchy" && m["component"] == "send1:fx1"
	})
	sendTab.next("send A's values", func(m map[string]any) bool {
		p, _ := m["params"].(map[string]any)
		return m["type"] == "param_update" && p["send1:fx1:wet"] == "0.5"
	})
	sendTab.next("send A's pins", func(m map[string]any) bool { return m["type"] == "mixer_pins" && m["bus"] == "send1" })

	// The old Master FX message is the Master FX bus.
	masterTab.send(map[string]any{"type": "subscribe_master_fx"})
	masterTab.next("master's modules", func(m map[string]any) bool {
		mods, _ := m["modules"].(map[string]any)
		return m["type"] == "master_fx_info" && m["bus"] == "master_fx" && mods["fx1"] == "comp"
	})

	// ---- The Mixer: Send A's reverb is pinned by default. ----
	mixer.send(map[string]any{"type": "subscribe_mixer"})
	m = mixer.next("send A's strip", func(m map[string]any) bool { return len(knobsOf(m, "send1")) == 1 })
	if k := knobsOf(m, "send1")[0]; k["key"] != "send1:fx1:wet" || k["comp"] != "fx1" || k["module"] != "verb" ||
		k["effect"] != true || k["pinned"] != true {
		t.Fatalf("send A knob: %v", k)
	}
	mixer.next("its value, in global", func(m map[string]any) bool { v, _ := globalVal(m, "send1:fx1:wet"); return v == "0.5" })
	sendTab.next("the default pin reaches the Send A tab", func(m map[string]any) bool {
		return m["type"] == "mixer_pins" && m["bus"] == "send1" && containsKey(keysOf(m), "send1:fx1:wet")
	})

	// A Mixer write goes out on slot 0 under its own key; a bad one is refused.
	mixer.send(map[string]any{"type": "set_mixer_param", "key": "send1:fx1:wet", "value": "0.25"})
	mixer.send(map[string]any{"type": "set_mixer_param", "key": "send1:fx1:wet", "value": "1.5"})
	mixer.next("range error", errorWith("bad value"))
	mixer.send(map[string]any{"type": "set_mixer_param", "key": "send1:fx1:size", "value": "0.5"})
	mixer.next("a bus key the Mixer is not showing", errorWith("not a mixer key"))
	waitFor := func(entry string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && !containsKey(ts.setsLog(), entry) {
			time.Sleep(20 * time.Millisecond)
		}
		if !containsKey(ts.setsLog(), entry) {
			t.Fatalf("%s never reached the shim: %v", entry, ts.setsLog())
		}
	}
	waitFor("0/send1:fx1:wet=0.25")

	// The device moves it: the strip follows.
	ru.mixerNotify(ctx, 0, "send1:fx1:wet", "0.8")
	mixer.next("notified value", func(m map[string]any) bool { v, _ := globalVal(m, "send1:fx1:wet"); return v == "0.8" })

	// ---- The bus tabs write any POSITION parameter, and nothing else. ----
	sendTab.send(map[string]any{"type": "set_bus_param", "key": "send1:fx1:size", "value": "0.6"})
	waitFor("0/send1:fx1:size=0.6")
	sendTab.send(map[string]any{"type": "set_bus_param", "key": "send1:return", "value": "100"})
	sendTab.next("a level is not the tab's", errorWith("not an FX bus parameter"))
	sendTab.send(map[string]any{"type": "set_bus_param", "key": "send1:fx1:module", "value": ""})
	sendTab.next("nor what is loaded", errorWith("not an FX bus parameter"))

	// ---- Pinning from the Master FX tab puts it on the Master strip. ----
	masterTab.send(map[string]any{"type": "pin_mixer", "key": "master_fx:fx1:thresh", "value": "1"})
	m = mixer.next("the master strip", func(m map[string]any) bool { return len(knobsOf(m, "master_fx")) == 1 })
	if k := knobsOf(m, "master_fx")[0]; k["key"] != "master_fx:fx1:thresh" || k["effect"] == true || k["min"] != -60.0 {
		t.Fatalf("master knob: %v", k)
	}
	mixer.next("its value", func(m map[string]any) bool { v, _ := globalVal(m, "master_fx:fx1:thresh"); return v == "-12" })
	masterTab.next("the pin is shown on the Master FX tab", func(m map[string]any) bool {
		return m["type"] == "mixer_pins" && m["bus"] == "master_fx" && containsKey(keysOf(m), "master_fx:fx1:thresh")
	})

	raw, _ := os.ReadFile(filepath.Join(dir, "set_state", "set-bus", "mixer_pins.json"))
	var doc pinDoc
	_ = json.Unmarshal(raw, &doc)
	if len(doc.Slots["send1"]) != 1 || len(doc.Slots["master_fx"]) != 1 || !containsKey(doc.Offered["send1"], "fx1|verb") {
		t.Fatalf("pins file: %s", raw)
	}

	// Unpinning from the Mixer: no slot, the key names the strip.
	mixer.send(map[string]any{"type": "pin_mixer", "key": "send1:fx1:wet", "value": "0"})
	mixer.next("send A's default unpinned", func(m map[string]any) bool {
		fx, _ := m["fx"].(map[string]any)
		_, has := fx["send1"]
		return has && len(knobsOf(m, "send1")) == 0
	})
}

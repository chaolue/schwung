package main

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func wsjsonRead(ctx context.Context, c *websocket.Conn, v any) error { return wsjson.Read(ctx, c, v) }

func TestParseKnobMap(t *testing.T) {
	knobs, ok := parseKnobMap(`[{"knob":1,"target":"synth","param":"cutoff","value":0.42},` +
		`{"knob":3,"target":"midi_fx","param":"rate","value":null},` +
		`{"knob":9,"target":"synth","param":"x","value":1},` +
		`{"knob":2,"target":"bogus","param":"x","value":1}]`)
	if !ok || len(knobs) != 2 {
		t.Fatalf("got %+v %v (want the two usable mappings)", knobs, ok)
	}
	if knobs[0].key() != "synth:cutoff" || knobs[1].key() != "midi_fx1:rate" {
		t.Errorf("keys: %s %s (the older midi_fx spelling is the first MIDI FX)", knobs[0].key(), knobs[1].key())
	}
	vals := knobValues(knobs)
	if vals["synth:cutoff"] != "0.42" || len(vals) != 1 {
		t.Errorf("values: %v (a knob with no position yet has no value)", vals)
	}
	if _, ok := parseKnobMap(""); ok {
		t.Error("empty is not an answer")
	}
	if empty, ok := parseKnobMap("[]"); !ok || len(empty) != 0 {
		t.Error("no mappings is an answer")
	}
	msg := knobsMessage(2, knobs)
	if msg.Type != "slot_knobs" || msg.Knobs[1].Comp != "midi_fx1" || msg.Knobs[1].Key != "midi_fx1:rate" {
		t.Errorf("message: %+v", msg)
	}
}

func TestSlotKnobsEndToEnd(t *testing.T) {
	dir := t.TempDir()
	origParam := shmParamPath
	shmParamPath = filepath.Join(dir, "schwung-param")
	if err := os.WriteFile(shmParamPath, make([]byte, shmParamSize), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := &tableShim{vals: map[string]string{
		"2/chain_modules": `{"synth":"obxd","midi_fx":[],"fx":["verb"]}`,
		"2/knob_map":      `[{"knob":1,"target":"synth","param":"cutoff","value":0.4},{"knob":2,"target":"fx1","param":"wet","value":0.3}]`,
		"3/knob_map":      `[]`,
	}, settings: true}
	origEvery := knobsPollEvery
	knobsPollEvery = 50 * time.Millisecond
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ru := NewRemoteUI(&App{logger: logger}, nil, dir, logger)
	ctx, cancel := context.WithCancel(context.Background())
	shimDone := make(chan struct{})
	go func() { ts.serve(ctx, t, shmParamPath); close(shimDone) }()
	loopDone := make(chan struct{})
	go func() { ru.knobsLoop(ctx); close(loopDone) }()
	srv := httptest.NewServer(ru)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		cancel()
		<-loopDone
		<-shimDone
		shmParamPath = origParam
		knobsPollEvery = origEvery
	})
	h := &mixerHarness{t: t, ru: ru, conn: conn, ctx: ctx}

	h.send(map[string]any{"type": "subscribe", "slot": 2})
	m := h.next("slot_knobs", func(m map[string]any) bool { return m["type"] == "slot_knobs" })
	ks, _ := m["knobs"].([]any)
	if len(ks) != 2 || ks[1].(map[string]any)["key"] != "fx1:wet" {
		t.Fatalf("knobs: %v", ks)
	}
	h.next("knob positions", func(m map[string]any) bool {
		p, _ := m["params"].(map[string]any)
		return m["type"] == "param_update" && p["synth:cutoff"] == "0.4" && p["fx1:wet"] == "0.3"
	})

	// A hardware turn reaches the browser through the running poll -- even
	// while the subscribe may still be streaming (a dropped send is resent).
	ts.set("2/knob_map", `[{"knob":1,"target":"synth","param":"cutoff","value":0.65},{"knob":2,"target":"fx1","param":"wet","value":0.3}]`)
	h.next("moved knob", func(m map[string]any) bool {
		p, _ := m["params"].(map[string]any)
		return m["type"] == "param_update" && p["synth:cutoff"] == "0.65"
	})
	// Once settled, a stationary map sends nothing at all.
	time.Sleep(300 * time.Millisecond)
	quiet, cancelQuiet := context.WithTimeout(ctx, 300*time.Millisecond)
	var extra map[string]any
	if err := wsjsonRead(quiet, conn, &extra); err == nil && extra["type"] == "param_update" {
		if p, _ := extra["params"].(map[string]any); p["synth:cutoff"] != nil {
			t.Errorf("a stationary knob was re-sent: %v", extra)
		}
	}
	cancelQuiet()

	// Re-mapping knob 2 sends the new map.
	ts.set("2/knob_map", `[{"knob":1,"target":"synth","param":"cutoff","value":0.65},{"knob":2,"target":"synth","param":"res","value":0.1}]`)
	shm := ru.ensureShm()
	h.next("new map", func(m map[string]any) bool {
		ks, _ := m["knobs"].([]any)
		return m["type"] == "slot_knobs" && len(ks) == 2 && ks[1].(map[string]any)["key"] == "synth:res"
	})

	// A slot with no mappings is polled at the idle rate, not every second.
	ru.knobs.mu.Lock()
	ru.knobs.init()
	ru.knobs.last[3], ru.knobs.known[3], ru.knobs.polled[3] = nil, true, time.Now()
	ru.knobs.mu.Unlock()
	ts.set("3/knob_map", `[{"knob":1,"target":"synth","param":"x","value":1}]`)
	ru.pollSlotKnobs(ctx, shm, 3, nil, time.Now().Add(knobsPollEvery))
	ru.knobs.mu.Lock()
	still := len(ru.knobs.last[3])
	ru.knobs.mu.Unlock()
	if still != 0 {
		t.Error("an empty map was re-read inside the idle interval")
	}
	ru.pollSlotKnobs(ctx, shm, 3, nil, time.Now().Add(knobsIdleEvery+time.Second))
	ru.knobs.mu.Lock()
	got := len(ru.knobs.last[3])
	ru.knobs.mu.Unlock()
	if got != 1 {
		t.Error("an empty map must be re-read after the idle interval")
	}
}

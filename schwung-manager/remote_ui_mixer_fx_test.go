package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"nhooyr.io/websocket"
)

func f64(v float64) *float64 { return &v }

func TestPickFxKnobs(t *testing.T) {
	params := []fxParamMeta{
		{Key: "early_mix", Name: "Early", Type: "float", Min: f64(0), Max: f64(1)},
		{Key: "wet", Name: "Wet", Type: "float", Min: f64(0), Max: f64(1)},
		{Key: "mix", Name: "Mix", Type: "float", Min: f64(0), Max: f64(100), Unit: "%"},
		{Key: "mode", Name: "Mode", Type: "enum"},
		{Key: "size", Name: "Size", Type: "int", Min: f64(1), Max: f64(8)},
	}
	// Undeclared: the first EXACT guess, by priority -- "mix" before "wet",
	// and never the substring match "early_mix".
	got := pickFxKnobs(nil, params)
	if len(got) != 1 || got[0].param.Key != "mix" || !got[0].guessed {
		t.Fatalf("guess: %+v", got)
	}
	// Declared: exactly what was declared, in order, not guessed; an enum or a
	// key the module does not have is dropped.
	got = pickFxKnobs([]string{"size", "mode", "nope", "wet"}, params)
	if len(got) != 2 || got[0].param.Key != "size" || got[1].param.Key != "wet" || got[0].guessed {
		t.Fatalf("declared: %+v", got)
	}
	// Nothing that fits: no knob, rather than a wrong one.
	if got := pickFxKnobs(nil, []fxParamMeta{{Key: "early_mix", Min: f64(0), Max: f64(1)}}); len(got) != 0 {
		t.Fatalf("no exact match must give no knob: %+v", got)
	}
	// Declaring more than the cap keeps the first mixerFxMaxPerModule.
	many := []fxParamMeta{}
	var keys []string
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		many = append(many, fxParamMeta{Key: k, Min: f64(0), Max: f64(1)})
		keys = append(keys, k)
	}
	if got := pickFxKnobs(keys, many); len(got) != mixerFxMaxPerModule {
		t.Fatalf("cap: %d", len(got))
	}
	// Placing: the real key, a step when none is declared, ints step by 1.
	k := fxKnobTemplate{param: params[4]}.place("fx3", "verb")
	if k.Key != "fx3:size" || !k.Int || k.Step != 1 || k.Module != "verb" {
		t.Fatalf("place int: %+v", k)
	}
	k = fxKnobTemplate{param: params[1], guessed: true}.place("fx1", "fv")
	if k.Step != 0.01 || !k.Guessed {
		t.Fatalf("place float: %+v", k)
	}
}

func TestModuleMixerParamsFromDisk(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "modules", "audio_fx", "verb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := `{"id":"verb","capabilities":{"component_type":"audio_fx","mixer_params":["wet","size"]}}`
	if err := os.WriteFile(filepath.Join(dir, "module.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := moduleMixerParams(base, "verb"); len(got) != 2 || got[0] != "wet" {
		t.Fatalf("got %v", got)
	}
	if got := moduleMixerParams(base, "absent"); got != nil {
		t.Fatalf("absent module: %v", got)
	}
	if got := moduleMixerParams(base, "../verb"); got != nil {
		t.Fatalf("a path must never be followed: %v", got)
	}
}

func TestFxValueRules(t *testing.T) {
	k := mixerFxKnob{Key: "fx1:mix", Min: 0, Max: 100, Step: 1}
	if v, ok := normalizeFxValue(k, "42.5"); !ok || v != "42.5" {
		t.Errorf("in range: %q %v", v, ok)
	}
	if _, ok := normalizeFxValue(k, "101"); ok {
		t.Error("out of range accepted")
	}
	if _, ok := normalizeFxValue(k, ""); ok {
		t.Error("empty accepted")
	}
	ki := mixerFxKnob{Min: 1, Max: 8, Int: true}
	if v, ok := normalizeFxValue(ki, "3.0"); !ok || v != "3" {
		t.Errorf("int: %q", v)
	}
	if validFxWrite(ki, "2.5") || !validFxWrite(ki, "2") || validFxWrite(k, "-1") || !validFxWrite(k, "0.5") {
		t.Error("write validation")
	}
	for key, want := range map[string]bool{
		"fx3:module": true, "midi_fx2:module": true, "synth:module": true, "load_file": true,
		"fx:move": true, "fx9:module": false, "fx1:mix": false, "slot:volume": false,
	} {
		if mixerFxModuleKey(key) != want {
			t.Errorf("mixerFxModuleKey(%q) != %v", key, want)
		}
	}
}

// ---------------------------------------------------------------------------
// End to end: a fake shim that ANSWERS reads from a table.
// ---------------------------------------------------------------------------

type tableShim struct {
	mu   sync.Mutex
	vals map[string]string // "slot/key" -> value; absent = answered error
	sets []string          // "slot/key=value"
	// settings: answer every slot / LFO setting with "0", as a device does --
	// otherwise the subscribe path retries each one and takes seconds.
	settings bool
}

func (ts *tableShim) set(k, v string) { ts.mu.Lock(); ts.vals[k] = v; ts.mu.Unlock() }

func (ts *tableShim) setsLog() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return append([]string{}, ts.sets...)
}

func (ts *tableShim) serve(ctx context.Context, t *testing.T, path string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Error(err)
		return
	}
	defer f.Close()
	b, err := syscall.Mmap(int(f.Fd()), 0, shmParamSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		t.Error(err)
		return
	}
	defer syscall.Munmap(b)
	word := (*uint32)(unsafe.Pointer(&b[0]))
	cut := func(x []byte) string {
		if i := bytes.IndexByte(x, 0); i >= 0 {
			x = x[:i]
		}
		return string(x)
	}
	for ctx.Err() == nil {
		w := atomic.LoadUint32(word)
		if typ := byte(w); typ == 1 || typ == 2 {
			slot := b[paramOffSlot]
			key := cut(b[paramOffKey : paramOffKey+paramKeyLen])
			k := strings.Join([]string{string(rune('0' + slot)), key}, "/")
			ts.mu.Lock()
			if typ == 1 {
				val := cut(b[paramOffValue : paramOffValue+256])
				ts.sets = append(ts.sets, k+"="+val)
				ts.vals[k] = val
				b[paramOffError] = 0
				binary.LittleEndian.PutUint32(b[paramOffResultLen:], 0)
			} else if v, ok := ts.vals[k]; ok || (ts.settings && (strings.HasPrefix(key, "slot:") ||
				strings.HasPrefix(key, "lfo"))) {
				if !ok {
					v = "0"
				}
				copy(b[paramOffValue:], v+"\x00")
				b[paramOffError] = 0
				binary.LittleEndian.PutUint32(b[paramOffResultLen:], uint32(len(v)))
			} else {
				b[paramOffError] = 1
				binary.LittleEndian.PutUint32(b[paramOffResultLen:], 0xFFFFFFFF)
			}
			ts.mu.Unlock()
			id := binary.LittleEndian.Uint32(b[paramOffRequestID:])
			binary.LittleEndian.PutUint32(b[paramOffResponseID:], id)
			atomic.StoreUint32(word, (w&^uint32(0xFF))|0x00010000)
		}
		time.Sleep(100 * time.Microsecond)
	}
}

const verbParams = `[{"key":"size","name":"Size","type":"float","min":0,"max":1},` +
	`{"key":"wet","name":"Wet","type":"float","min":0,"max":1,"step":0.05},` +
	`{"key":"mode","name":"Mode","type":"enum","options":["a","b"]}]`

func TestMixerFxEndToEnd(t *testing.T) {
	dir := t.TempDir()
	writeActiveSet(t, dir, "set-fx")
	origUI, origParam := uiStatePath, shmParamPath
	uiStatePath = filepath.Join(dir, "schwung-ui")
	shmParamPath = filepath.Join(dir, "schwung-param")
	if err := os.WriteFile(uiStatePath, uiStateBytes(4, [uiStateSlots]uiSlotFixture{{volPct: 100, name: "Pad"}}), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shmParamPath, make([]byte, shmParamSize), 0o644); err != nil {
		t.Fatal(err)
	}

	ts := &tableShim{vals: map[string]string{}}
	for s := 0; s < mixerSlots; s++ {
		ts.vals[string(rune('0'+s))+"/chain_modules"] = `{"synth":"","midi_fx":[],"fx":[]}`
	}
	ts.vals["0/chain_modules"] = `{"synth":"obxd","midi_fx":[],"fx":["eq","verb"]}`
	ts.vals["0/fx1:chain_params"] = `[{"key":"low","name":"Low","type":"float","min":-12,"max":12}]` // no mix
	ts.vals["0/fx2:chain_params"] = verbParams
	ts.vals["0/fx2:wet"] = "0.35"

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ru := NewRemoteUI(&App{logger: logger}, nil, dir, logger)
	ctx, cancel := context.WithCancel(context.Background())
	shimDone := make(chan struct{})
	go func() { ts.serve(ctx, t, shmParamPath); close(shimDone) }()
	loopDone := make(chan struct{})
	go func() { ru.mixerLoop(ctx); close(loopDone) }()
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
		ru.mix.waitIdle()
		<-shimDone
		uiStatePath, shmParamPath = origUI, origParam
	})
	h := &mixerHarness{t: t, ru: ru, conn: conn, ctx: ctx}

	h.send(map[string]any{"type": "subscribe_mixer"})
	knobsOf := func(m map[string]any, slot string) []any {
		fx, _ := m["fx"].(map[string]any)
		ks, _ := fx[slot].([]any)
		return ks
	}
	// The knob arrives (fx2's guessed "wet"; the EQ has no mix and gets none),
	// then its value.
	m := h.next("fx knobs for slot 1", func(m map[string]any) bool { return len(knobsOf(m, "0")) > 0 })
	ks := knobsOf(m, "0")
	if len(ks) != 1 {
		t.Fatalf("want one knob (the EQ has no mix): %v", ks)
	}
	k := ks[0].(map[string]any)
	// The mix is pinned by default, so it can be unpinned like any pin.
	if k["key"] != "fx2:wet" || k["guessed"] != true || k["module"] != "verb" || k["step"] != 0.05 ||
		k["pinned"] != true || k["effect"] != true {
		t.Fatalf("knob: %v", k)
	}
	h.next("fx value", func(m map[string]any) bool { v, _ := slotVal(m, "0", "fx2:wet"); return v == "0.35" })

	// A write to the knob is allowed and range-checked; a write to a param the
	// Mixer is NOT showing is refused.
	h.send(map[string]any{"type": "set_mixer_param", "slot": 0, "key": "fx2:wet", "value": "0.5"})
	h.send(map[string]any{"type": "set_mixer_param", "slot": 0, "key": "fx2:wet", "value": "1.5"})
	h.next("range error", func(m map[string]any) bool {
		msg, _ := m["message"].(string)
		return m["type"] == "error" && strings.Contains(msg, "bad value")
	})
	h.send(map[string]any{"type": "set_mixer_param", "slot": 0, "key": "fx2:size", "value": "0.5"})
	h.next("not-a-mixer-key error", func(m map[string]any) bool {
		msg, _ := m["message"].(string)
		return m["type"] == "error" && strings.Contains(msg, "not a mixer key")
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !containsKey(ts.setsLog(), "0/fx2:wet=0.5") {
		time.Sleep(20 * time.Millisecond)
	}
	if got := ts.setsLog(); !containsKey(got, "0/fx2:wet=0.5") || len(got) != 1 {
		t.Fatalf("writes reaching the shim: %v (want exactly fx2:wet=0.5)", got)
	}

	// A notified value moves the knob.
	ru.mixerNotify(ctx, 0, "fx2:wet", "0.8")
	h.next("notified value", func(m map[string]any) bool { v, _ := slotVal(m, "0", "fx2:wet"); return v == "0.8" })

	// The effect is swapped on the device: the notify ring shows the module
	// write, and the knob list follows.
	ts.set("0/chain_modules", `{"synth":"obxd","midi_fx":[],"fx":["eq","verb","delay"]}`)
	ts.set("0/fx3:chain_params", `[{"key":"mix","name":"Mix","type":"int","min":0,"max":100}]`)
	ts.set("0/fx3:mix", "40")
	ru.mixerNotify(ctx, 0, "fx3:module", "delay")
	m = h.next("fx knobs after the load", func(m map[string]any) bool { return len(knobsOf(m, "0")) == 2 })
	if k := knobsOf(m, "0")[1].(map[string]any); k["key"] != "fx3:mix" || k["int"] != true {
		t.Fatalf("new knob: %v", k)
	}
	h.next("new knob's value", func(m map[string]any) bool { v, _ := slotVal(m, "0", "fx3:mix"); return v == "40" })
}

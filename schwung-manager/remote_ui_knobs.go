package main

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// A slot's mapped knobs (Knobs 1-8) on its Slot tab.
//
// Each mapping names a real parameter ("synth" + "cutoff"), so the browser
// draws a real knob on that key -- its range from the component's chain_params,
// its value from the component's params -- and writes it through set_param like
// any other control. The chain moves the hardware knob's accumulator on every
// write to a mapped parameter (knob_sync_from_write), so the Move's knob carries
// on from where the browser left it.
//
// The other direction needs a poll: a hardware knob turn reaches the module
// directly and is on no notify ring. "knob_map" answers every mapping and its
// position in ONE read; it is polled once a second for a slot whose tab is open
// and that HAS mappings (every knobsIdleEvery for one that has none, to notice a
// new mapping), and a moved knob goes out as an ordinary param_update on the
// real key -- so the generic control for the same parameter follows it too.
// ---------------------------------------------------------------------------

// Vars, not consts, so a test can run the real loop quickly.
var (
	knobsPollEvery = 1 * time.Second
	knobsIdleEvery = 5 * time.Second
)

// slotKnob is one mapping, as the chain reports it.
type slotKnob struct {
	Knob   int      `json:"knob"`   // 1-8
	Target string   `json:"target"` // "synth", "fx2", "midi_fx1"
	Param  string   `json:"param"`
	Value  *float64 `json:"value"`
}

// key is the parameter the knob drives, spelled the way set_param takes it.
// "midi_fx" is the chain's older spelling of the first MIDI FX position.
func (k slotKnob) key() string {
	t := k.Target
	if t == "midi_fx" {
		t = "midi_fx1"
	}
	return t + ":" + k.Param
}

type wsSlotKnob struct {
	Knob  int    `json:"knob"`
	Comp  string `json:"comp"`  // the component, "synth" / "fx2" / "midi_fx1"
	Param string `json:"param"` // its key within the component
	Key   string `json:"key"`   // comp + ":" + param
}

type wsSlotKnobs struct {
	Type  string       `json:"type"` // "slot_knobs"
	Slot  uint8        `json:"slot"`
	Knobs []wsSlotKnob `json:"knobs"`
}

// parseKnobMap decodes "knob_map"; usable mappings only.
func parseKnobMap(raw string) ([]slotKnob, bool) {
	var in []slotKnob
	if json.Unmarshal([]byte(raw), &in) != nil || in == nil {
		return nil, false
	}
	out := in[:0]
	for _, k := range in {
		if k.Knob >= 1 && k.Knob <= 8 && k.Param != "" && isChainComponent(slotKnob{Target: k.Target}.compName()) {
			out = append(out, k)
		}
	}
	return out, true
}

func (k slotKnob) compName() string {
	if k.Target == "midi_fx" {
		return "midi_fx1"
	}
	return k.Target
}

func knobsMessage(slot uint8, knobs []slotKnob) wsSlotKnobs {
	msg := wsSlotKnobs{Type: "slot_knobs", Slot: slot, Knobs: []wsSlotKnob{}}
	for _, k := range knobs {
		msg.Knobs = append(msg.Knobs, wsSlotKnob{Knob: k.Knob, Comp: k.compName(), Param: k.Param, Key: k.key()})
	}
	return msg
}

// sameMappings: the same knobs on the same parameters (values aside).
func sameMappings(a, b []slotKnob) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Knob != b[i].Knob || a[i].key() != b[i].key() {
			return false
		}
	}
	return true
}

// knobValues are the knobs' positions keyed by the real parameter key.
func knobValues(knobs []slotKnob) map[string]string {
	out := map[string]string{}
	for _, k := range knobs {
		if k.Value != nil {
			out[k.key()] = strconv.FormatFloat(*k.Value, 'f', -1, 64)
		}
	}
	return out
}

// knobsState is RemoteUI's last-known knob map per slot.
type knobsState struct {
	mu     sync.Mutex
	last   map[uint8][]slotKnob
	known  map[uint8]bool
	polled map[uint8]time.Time
}

func (ks *knobsState) init() {
	if ks.last == nil {
		ks.last = make(map[uint8][]slotKnob)
		ks.known = make(map[uint8]bool)
		ks.polled = make(map[uint8]time.Time)
	}
}

// sendSlotKnobs reads one slot's knob map and sends it to one client (on
// subscribe), with the knobs' positions as a param_update.
func (ru *RemoteUI) sendSlotKnobs(ctx context.Context, c *ruClient, slot uint8) {
	raw, err := ru.shm.GetParam(slot, "knob_map")
	if err != nil {
		return // the poll fills it in
	}
	knobs, ok := parseKnobMap(raw)
	if !ok {
		return
	}
	ru.knobs.mu.Lock()
	ru.knobs.init()
	ru.knobs.last[slot] = knobs
	ru.knobs.known[slot] = true
	ru.knobs.polled[slot] = time.Now()
	ru.knobs.mu.Unlock()

	ru.writeJSON(ctx, c, knobsMessage(slot, knobs))
	if vals := knobValues(knobs); len(vals) > 0 {
		ru.writeJSON(ctx, c, wsParamUpdate{Type: "param_update", Slot: slot, Params: vals})
	}
}

// knobsLoop polls the knob map of every slot somebody is looking at.
func (ru *RemoteUI) knobsLoop(ctx context.Context) {
	ticker := time.NewTicker(knobsPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		shm := ru.ensureShm()
		if shm == nil {
			continue
		}
		for slot := uint8(0); slot < remoteUISlots; slot++ {
			subs := ru.subscribedClients(slot)
			if len(subs) == 0 {
				continue // nobody is looking: never touch the param channel
			}
			ru.pollSlotKnobs(ctx, shm, slot, subs, time.Now())
		}
	}
}

func (ru *RemoteUI) pollSlotKnobs(ctx context.Context, shm *ShmParams, slot uint8, subs []*ruClient, now time.Time) {
	ks := &ru.knobs
	ks.mu.Lock()
	ks.init()
	prev, known := ks.last[slot], ks.known[slot]
	if known && len(prev) == 0 && now.Sub(ks.polled[slot]) < knobsIdleEvery {
		ks.mu.Unlock()
		return // no mappings: look again less often
	}
	ks.mu.Unlock()

	raw, ok, err := shm.TryGetParam(slot, "knob_map")
	if !ok || err != nil {
		return // busy, or not answered: try at the next tick
	}
	knobs, parsed := parseKnobMap(raw)
	if !parsed {
		return
	}

	ks.mu.Lock()
	ks.last[slot] = knobs
	ks.known[slot] = true
	ks.polled[slot] = now
	ks.mu.Unlock()

	// A write dropped because the client's previous one is still in flight
	// (the subscribe can still be streaming parameters) must not be lost: this
	// map is already recorded as sent, so a stationary knob would never be
	// sent again. A drop forgets the map, and the next poll sends it whole.
	dropped := false
	if !known || !sameMappings(prev, knobs) {
		msg := knobsMessage(slot, knobs)
		for _, c := range subs {
			if !ru.writeJSONTry(ctx, c, msg) {
				dropped = true
			}
		}
	}
	// Only the positions that MOVED: a stationary knob must not keep
	// re-asserting a value over a write the browser has just made.
	old := knobValues(prev)
	changed := map[string]string{}
	for k, v := range knobValues(knobs) {
		if !known || old[k] != v {
			changed[k] = v
		}
	}
	if len(changed) > 0 {
		upd := wsParamUpdate{Type: "param_update", Slot: slot, Params: changed}
		for _, c := range subs {
			if !ru.writeJSONTry(ctx, c, upd) {
				dropped = true
			}
		}
	}
	if dropped {
		ks.mu.Lock()
		ks.known[slot] = false
		ks.mu.Unlock()
	}
}

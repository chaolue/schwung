package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ---------------------------------------------------------------------------
// Pinning a parameter to the Mixer.
//
// Any numeric or choice knob on a Slot tab -- or on the Master FX / Send A/B
// tabs -- can be pinned; it then appears on that slot's or bus's Mixer strip
// (under "Pinned"), drawn and written like the effect knobs. Pins belong to the
// SET -- set_state/<uuid>/mixer_pins.json -- so they travel with it, duplicates
// included (set_state_copy copies everything but an exclude list), and a set
// switch brings that set's own. A strip is named "0".."7" or by its bus id
// ("master_fx", "send1", "send2"); see remote_ui_strips.go.
//
// A pin remembers the MODULE it was made on. If a different module now sits in
// that position the pin is hidden, never pointed at an unrelated parameter that
// happens to share a key; it comes back if that module returns.
// ---------------------------------------------------------------------------

// maxPinsPerSlot counts every pin, an effect's default mix pin included.
const maxPinsPerSlot = 8

type pinEntry struct {
	Key    string `json:"key"`    // "synth:cutoff"
	Module string `json:"module"` // the module it was pinned on
}

type pinDoc struct {
	Version int `json:"version"`
	// Slots: strip name ("0".."7", "master_fx", "send1", "send2") -> pins.
	Slots map[string][]pinEntry `json:"slots"`
	// Offered: per slot, the "fxN|module" placements whose default mix pin has
	// been added once. Unpinning it does not bring it back; a DIFFERENT module
	// loaded in that position is new, and gets its own.
	Offered map[string][]string `json:"offered,omitempty"`
}

type wsMixerPins struct {
	Type string   `json:"type"` // "mixer_pins"
	Slot uint8    `json:"slot"`
	Bus  string   `json:"bus,omitempty"` // set for an FX bus's pins; Slot is then 0
	Keys []string `json:"keys"`
}

type pinsState struct {
	mu sync.Mutex // serialises the file's read-modify-write
}

// activeSetUUID is line 1 of active_set.txt.
func activeSetUUID(basePath string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(basePath, "active_set.txt"))
	if err != nil {
		return "", err
	}
	uuid := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if uuid == "" || strings.ContainsAny(uuid, `/\`) || uuid == "." || uuid == ".." {
		return "", errors.New("active_set.txt names no set")
	}
	return uuid, nil
}

func pinsPath(basePath, uuid string) string {
	return filepath.Join(basePath, "set_state", uuid, "mixer_pins.json")
}

// readPins reads the active set's pins; an absent file is no pins.
func readPins(basePath string) (pinDoc, error) {
	doc := pinDoc{Version: 1, Slots: map[string][]pinEntry{}}
	uuid, err := activeSetUUID(basePath)
	if err != nil {
		return doc, err
	}
	raw, err := os.ReadFile(pinsPath(basePath, uuid))
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, err
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Slots == nil {
		return pinDoc{Version: 1, Slots: map[string][]pinEntry{}}, nil
	}
	return doc, nil
}

// writePins replaces the active set's pins file atomically. A slot with no
// pins is left out (its offered record is kept: that is what stops an
// unpinned default coming back).
func writePins(basePath string, doc pinDoc) error {
	for k, v := range doc.Slots {
		if len(v) == 0 {
			delete(doc.Slots, k)
		}
	}
	uuid, err := activeSetUUID(basePath)
	if err != nil {
		return err
	}
	dir := filepath.Join(basePath, "set_state", uuid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := pinsPath(basePath, uuid) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, pinsPath(basePath, uuid))
}

// slotPins is one strip's pins in the active set (none on any error: a pin
// that cannot be read is simply not shown).
func (ru *RemoteUI) slotPins(strip int) []pinEntry {
	ru.pins.mu.Lock()
	defer ru.pins.mu.Unlock()
	doc, err := readPins(ru.basePath)
	if err != nil {
		return nil
	}
	return doc.Slots[stripName(strip)]
}

func pinKeys(pins []pinEntry) []string {
	out := []string{}
	for _, p := range pins {
		out = append(out, p.Key)
	}
	return out
}

// sendPins tells one Slot-tab client which of the slot's keys are pinned.
func (ru *RemoteUI) sendPins(ctx context.Context, c *ruClient, slot uint8) {
	ru.writeJSONAsync(ctx, c, wsMixerPins{Type: "mixer_pins", Slot: slot, Keys: pinKeys(ru.slotPins(int(slot)))})
}

// sendBusPins tells one bus-tab client which of the bus's keys are pinned.
func (ru *RemoteUI) sendBusPins(ctx context.Context, c *ruClient, bus fxBus) {
	ru.writeJSONAsync(ctx, c, wsMixerPins{Type: "mixer_pins", Bus: bus.ID,
		Keys: pinKeys(ru.slotPins(busStrip(bus)))})
}

// broadcastPins tells every subscriber of a strip (or of every strip, -1).
func (ru *RemoteUI) broadcastPins(ctx context.Context, slot int) {
	for _, bus := range fxBuses {
		strip := busStrip(bus)
		if slot >= 0 && strip != slot {
			continue
		}
		subs := ru.busSubscribedClients(bus.ID)
		if len(subs) == 0 {
			continue
		}
		msg := wsMixerPins{Type: "mixer_pins", Bus: bus.ID, Keys: pinKeys(ru.slotPins(strip))}
		for _, c := range subs {
			ru.writeJSONAsync(ctx, c, msg)
		}
	}
	for s := uint8(0); s < remoteUISlots; s++ {
		if slot >= 0 && int(s) != slot {
			continue
		}
		subs := ru.subscribedClients(s)
		if len(subs) == 0 {
			continue
		}
		msg := wsMixerPins{Type: "mixer_pins", Slot: s, Keys: pinKeys(ru.slotPins(int(s)))}
		for _, c := range subs {
			ru.writeJSONAsync(ctx, c, msg)
		}
	}
}

// handlePinMixer pins (value "1") or unpins ("0") key on a strip: an FX bus's
// when the key names one ("send1:fx1:mix"), else msg.Slot's.
func (ru *RemoteUI) handlePinMixer(ctx context.Context, c *ruClient, msg wsMessage) {
	fail := func(why string) { ru.sendError(ctx, c, "pin_mixer: "+why) }
	strip, isBus := stripOfBusKey(msg.Key)
	if !isBus {
		if msg.Slot == nil || *msg.Slot >= remoteUISlots {
			fail("needs a slot 0-" + strconv.Itoa(remoteUISlots-1))
			return
		}
		strip = int(*msg.Slot)
	}
	comp, param, ok := splitStripKey(strip, msg.Key)
	if !ok {
		fail("not a slot parameter: " + msg.Key)
		return
	}
	pin := msg.Value == "1"
	if !pin && msg.Value != "0" {
		fail("value must be 1 or 0")
		return
	}

	module := ""
	if pin {
		if !ru.requireShm(ctx, c) {
			return
		}
		mods, err := readStripModules(ru.blockingGetter(), strip)
		if err != nil {
			fail("could not read the slot: " + err.Error())
			return
		}
		module = mods[comp]
		if module == "" {
			fail("nothing is loaded in " + componentLabel(comp))
			return
		}
		params, ok := ru.moduleParams(module, stripComp(strip, comp), readSlotOf(strip), ru.blockingGetter())
		if !ok {
			fail("could not read " + module + "'s parameters")
			return
		}
		if _, ok := findUsableParam(params, param); !ok {
			fail(param + " is not a parameter of " + module + " the Mixer can show")
			return
		}
	}

	ru.pins.mu.Lock()
	doc, err := readPins(ru.basePath)
	if err == nil {
		key := stripName(strip)
		list := doc.Slots[key][:0:0]
		for _, p := range doc.Slots[key] {
			if p.Key != msg.Key {
				list = append(list, p)
			}
		}
		if pin {
			if len(list) >= maxPinsPerSlot {
				ru.pins.mu.Unlock()
				fail(fmt.Sprintf("at most %d pins per slot -- unpin one first", maxPinsPerSlot))
				return
			}
			list = append(list, pinEntry{Key: msg.Key, Module: module})
		}
		doc.Slots[key] = list
		err = writePins(ru.basePath, doc)
	}
	ru.pins.mu.Unlock()
	if err != nil {
		fail("could not save: " + err.Error())
		return
	}
	ru.logger.Info("ws pin_mixer", "strip", stripName(strip), "key", msg.Key, "pinned", pin)
	ru.broadcastPins(ctx, strip)
	// The strip follows: the knob list is rebuilt from the pins.
	ru.mixerScheduleModules(ctx, strip, 0)
}

// offerDefaultPins pins each newly seen effect's mix (its mixer_params, or the
// guessed mix) once per placement. false: a read did not complete.
func (ru *RemoteUI) offerDefaultPins(ctx context.Context, slot int, mods slotModules, get paramGetter) bool {
	key := stripName(slot)
	ru.pins.mu.Lock()
	doc, err := readPins(ru.basePath)
	ru.pins.mu.Unlock()
	if err != nil {
		return true // no set to keep pins in: nothing to offer, nothing broken
	}
	offered := map[string]bool{}
	for _, tag := range doc.Offered[key] {
		offered[tag] = true
	}
	type offer struct {
		comp, id string
		tmpl     []fxKnobTemplate
	}
	var todo []offer
	for i := 1; i <= chainFxPositions; i++ {
		comp := "fx" + strconv.Itoa(i)
		id := mods[comp]
		if id == "" || offered[comp+"|"+id] {
			continue
		}
		tmpl, ok := ru.mixerFxTemplates(id, stripComp(slot, comp), readSlotOf(slot), get)
		if !ok {
			return false
		}
		todo = append(todo, offer{comp, id, tmpl})
	}
	if len(todo) == 0 {
		return true
	}

	ru.pins.mu.Lock()
	doc, err = readPins(ru.basePath) // again: another writer may have been here
	if err != nil {
		ru.pins.mu.Unlock()
		return true
	}
	if doc.Offered == nil {
		doc.Offered = map[string][]string{}
	}
	pins := doc.Slots[key]
	tags := doc.Offered[key]
	for _, o := range todo {
		// A different module in this position: the old one's offer is spent.
		kept := tags[:0:0]
		for _, t := range tags {
			if !strings.HasPrefix(t, o.comp+"|") {
				kept = append(kept, t)
			}
		}
		tags = append(kept, o.comp+"|"+o.id)
		for _, t := range o.tmpl {
			k := stripComp(slot, o.comp) + ":" + t.param.Key
			dup := false
			for _, p := range pins {
				if p.Key == k && p.Module == o.id {
					dup = true
				}
			}
			if !dup && len(pins) < maxPinsPerSlot {
				pins = append(pins, pinEntry{Key: k, Module: o.id})
			}
		}
	}
	if doc.Slots == nil {
		doc.Slots = map[string][]pinEntry{}
	}
	doc.Slots[key] = pins
	doc.Offered[key] = tags
	err = writePins(ru.basePath, doc)
	ru.pins.mu.Unlock()
	if err == nil {
		ru.broadcastPins(ctx, slot)
	}
	return true
}

// findUsableParam is the named parameter if a fader can drive it.
func findUsableParam(params []fxParamMeta, key string) (fxParamMeta, bool) {
	for _, p := range params {
		if p.Key == key && p.usable() {
			return p, true
		}
	}
	return fxParamMeta{}, false
}

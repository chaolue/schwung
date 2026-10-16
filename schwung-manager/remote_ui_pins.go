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
// Any numeric knob on a Slot tab can be pinned; it then appears on that slot's
// Mixer strip (under "Pinned"), drawn and written like the effect knobs. Pins
// belong to the SET -- set_state/<uuid>/mixer_pins.json -- so they travel with
// it, duplicates included (set_state_copy copies everything but an exclude
// list), and a set switch brings that set's own.
//
// A pin remembers the MODULE it was made on. If a different module now sits in
// that position the pin is hidden, never pointed at an unrelated parameter that
// happens to share a key; it comes back if that module returns.
// ---------------------------------------------------------------------------

const maxPinsPerSlot = 4

type pinEntry struct {
	Key    string `json:"key"`    // "synth:cutoff"
	Module string `json:"module"` // the module it was pinned on
}

type pinDoc struct {
	Version int                   `json:"version"`
	Slots   map[string][]pinEntry `json:"slots"`
}

type wsMixerPins struct {
	Type string   `json:"type"` // "mixer_pins"
	Slot uint8    `json:"slot"`
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

// writePins replaces the active set's pins file atomically.
func writePins(basePath string, doc pinDoc) error {
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

// slotPins is one slot's pins in the active set (none on any error: a pin
// that cannot be read is simply not shown).
func (ru *RemoteUI) slotPins(slot int) []pinEntry {
	ru.pins.mu.Lock()
	defer ru.pins.mu.Unlock()
	doc, err := readPins(ru.basePath)
	if err != nil {
		return nil
	}
	return doc.Slots[strconv.Itoa(slot)]
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

// broadcastPins tells every subscriber of slot (or of every slot, -1).
func (ru *RemoteUI) broadcastPins(ctx context.Context, slot int) {
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

// handlePinMixer pins (value "1") or unpins ("0") key on a slot.
func (ru *RemoteUI) handlePinMixer(ctx context.Context, c *ruClient, msg wsMessage) {
	fail := func(why string) { ru.sendError(ctx, c, "pin_mixer: "+why) }
	if msg.Slot == nil || *msg.Slot >= remoteUISlots {
		fail("needs a slot 0-" + strconv.Itoa(remoteUISlots-1))
		return
	}
	slot := *msg.Slot
	comp, param, ok := strings.Cut(msg.Key, ":")
	if !ok || param == "" || !isChainComponent(comp) {
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
		mods, err := readSlotModules(ru.blockingGetter(), slot)
		if err != nil {
			fail("could not read the slot: " + err.Error())
			return
		}
		module = mods[comp]
		if module == "" {
			fail("nothing is loaded in " + componentLabel(comp))
			return
		}
		params, ok := ru.moduleParams(module, comp, slot, ru.blockingGetter())
		if !ok {
			fail("could not read " + module + "'s parameters")
			return
		}
		if _, ok := findUsableParam(params, param); !ok {
			fail(param + " is not a numeric parameter of " + module)
			return
		}
	}

	ru.pins.mu.Lock()
	doc, err := readPins(ru.basePath)
	if err == nil {
		key := strconv.Itoa(int(slot))
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
		if len(list) == 0 {
			delete(doc.Slots, key)
		} else {
			doc.Slots[key] = list
		}
		err = writePins(ru.basePath, doc)
	}
	ru.pins.mu.Unlock()
	if err != nil {
		fail("could not save: " + err.Error())
		return
	}
	ru.logger.Info("ws pin_mixer", "slot", slot, "key", msg.Key, "pinned", pin)
	ru.broadcastPins(ctx, int(slot))
	// The strip follows: the knob list is rebuilt from the pins.
	ru.mixerScheduleModules(ctx, int(slot), 0)
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

package main

import (
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// The Mixer's STRIPS: the eight chain slots, then the three FX buses.
//
// A strip is an int so the effect knobs, the pins and the reads are one code
// path whichever kind it is: 0..7 are chain slots, mixerSlots+i is fxBuses[i]
// (Master FX, Send A, Send B). What differs is small and lives here:
//
//   - a slot's keys are "fx1:mix" on that slot; a bus's are "send1:fx1:mix",
//     always on slot 0 -- the key carries the bus;
//   - a bus has only audio FX positions;
//   - a bus's knob VALUES are global (its key is unique device-wide), so they
//     ride in the Mixer's Global map, beside the returns.
//
// On the wire and in the pins file a strip is named "0".."7" or by its bus id.
// ---------------------------------------------------------------------------

var mixerStrips = mixerSlots + len(fxBuses)

// stripBus is the bus a strip is, if it is one.
func stripBus(strip int) (fxBus, bool) {
	if strip >= mixerSlots && strip < mixerStrips {
		return fxBuses[strip-mixerSlots], true
	}
	return fxBus{}, false
}

// busStrip is a bus's strip.
func busStrip(bus fxBus) int {
	for i, b := range fxBuses {
		if b.ID == bus.ID {
			return mixerSlots + i
		}
	}
	return -1
}

// stripName: "0".."7" for a slot, the bus id for a bus.
func stripName(strip int) string {
	if bus, ok := stripBus(strip); ok {
		return bus.ID
	}
	return strconv.Itoa(strip)
}

// readSlotOf is the param-channel slot a strip's keys are read on: the slot
// itself, or 0 for a bus (and for a global key, strip -1).
func readSlotOf(strip int) uint8 {
	if strip < 0 || strip >= mixerSlots {
		return 0
	}
	return uint8(strip)
}

// stripComp is a position's full key prefix: "fx1" on a slot, "send1:fx1" on
// a bus.
func stripComp(strip int, comp string) string {
	if bus, ok := stripBus(strip); ok {
		return bus.ID + ":" + comp
	}
	return comp
}

// splitStripKey splits a strip's parameter key into its position (as the
// strip names it, "fx1") and parameter.
func splitStripKey(strip int, key string) (comp, param string, ok bool) {
	if bus, isBus := stripBus(strip); isBus {
		b, c, p, ok := splitBusKey(key)
		if !ok || b.ID != bus.ID {
			return "", "", false
		}
		return c, p, true
	}
	comp, param, ok = strings.Cut(key, ":")
	if !ok || param == "" || !isChainComponent(comp) {
		return "", "", false
	}
	return comp, param, true
}

// stripOfBusKey is the bus strip a key belongs to, if it is a bus position's.
func stripOfBusKey(key string) (int, bool) {
	bus, _, _, ok := splitBusKey(key)
	if !ok {
		return -1, false
	}
	return busStrip(bus), true
}

// readStripModules reads what a strip holds -- one read either way.
func readStripModules(get paramGetter, strip int) (slotModules, error) {
	if bus, ok := stripBus(strip); ok {
		return readBusModules(get, bus)
	}
	return readSlotModules(get, uint8(strip))
}

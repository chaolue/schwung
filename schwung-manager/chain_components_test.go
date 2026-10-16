package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestComponentPrefixesCoverEveryPosition(t *testing.T) {
	if len(componentPrefixes) != 1+chainFxPositions+chainMidiFxPositions {
		t.Fatalf("got %d components: %v", len(componentPrefixes), componentPrefixes)
	}
	if len(masterFxSlots) != masterFxPositions || masterFxSlots[7] != "fx8" {
		t.Fatalf("master fx: %v", masterFxSlots)
	}
	for _, c := range componentPrefixes {
		if !isChainComponent(c) {
			t.Errorf("%s is not a chain component", c)
		}
	}
	for _, bad := range []string{"fx0", "fx9", "fx01", "midi_fx9", "midi_fx", "fx", "master_fx:fx1", "lfo1", ""} {
		if isChainComponent(bad) {
			t.Errorf("%q accepted as a chain component", bad)
		}
	}
}

func TestComponentLabel(t *testing.T) {
	for in, want := range map[string]string{
		"synth": "Synth", "fx1": "Audio FX 1", "fx8": "Audio FX 8",
		"midi_fx1": "MIDI FX 1", "midi_fx8": "MIDI FX 8", "weird": "weird",
	} {
		if got := componentLabel(in); got != want {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
}

func TestParseChainModules(t *testing.T) {
	m, ok := parseChainModules(`{"synth":"obxd","midi_fx":["arp","chord"],"fx":["freeverb","","tape"]}`)
	if !ok {
		t.Fatal("did not parse")
	}
	want := slotModules{"synth": "obxd", "midi_fx1": "arp", "midi_fx2": "chord", "fx1": "freeverb", "fx3": "tape"}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %v, want %v", m, want)
	}
	// Signal flow: MIDI FX, synth, audio FX.
	if got := m.orderedComponents(); !reflect.DeepEqual(got,
		[]string{"midi_fx1", "midi_fx2", "synth", "fx1", "fx3"}) {
		t.Errorf("order: %v", got)
	}
	empty, ok := parseChainModules(`{"synth":"","midi_fx":[],"fx":[]}`)
	if !ok || len(empty) != 0 {
		t.Errorf("an empty chain is an empty map: %v %v", empty, ok)
	}
	// Anything that is not the whole shape is not an answer.
	for _, bad := range []string{``, `{}`, `{"synth":"x"}`, `[1]`, `not json`} {
		if _, ok := parseChainModules(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParseMasterFxModules(t *testing.T) {
	m, ok := parseMasterFxModules(`[{"id":"","path":""},{"id":"","path":"/a/b/echo/echo.so"},` +
		`{"id":"verb","path":""},{"id":"","path":""},{"id":"","path":""},{"id":"","path":""},` +
		`{"id":"","path":""},{"id":"lim","path":"/x/lim/lim.so"}]`)
	if !ok {
		t.Fatal("did not parse")
	}
	want := slotModules{"fx2": "echo", "fx3": "verb", "fx8": "lim"}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got %v, want %v", m, want)
	}
}

// A read that did not complete is an ERROR, never an empty slot: the poller
// would otherwise broadcast "nothing loaded" and restore it a tick later.
func TestReadSlotModulesTimeoutIsNotEmpty(t *testing.T) {
	busy := func(uint8, string) (string, error) { return "", errBusy }
	if _, err := readSlotModules(busy, 0); err == nil {
		t.Fatal("a busy channel must be an error")
	}
	if _, err := readMasterFxModules(busy); err == nil {
		t.Fatal("a busy channel must be an error for Master FX too")
	}
	// A timeout part-way through the fallback is an error too.
	calls := 0
	partial := func(slot uint8, key string) (string, error) {
		calls++
		switch key {
		case "chain_modules":
			return "", errors.New("param get error: not served")
		case "synth_module":
			return "obxd", nil
		}
		return "", errBusy
	}
	if _, err := readSlotModules(partial, 0); err == nil || calls < 3 {
		t.Fatalf("a timeout during the fallback must surface (err=%v, calls=%d)", err, calls)
	}
}

// A slot with no chain instance answers every key with an error: that is a
// real "empty", not a failure.
func TestReadSlotModulesNoInstanceIsEmpty(t *testing.T) {
	none := func(uint8, string) (string, error) { return "", errors.New("param get error: no instance") }
	m, err := readSlotModules(none, 3)
	if err != nil || len(m) != 0 {
		t.Fatalf("got %v %v, want empty and no error", m, err)
	}
}

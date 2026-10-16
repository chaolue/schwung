package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// What is loaded where — every position, in as few reads as possible.
//
// A chain slot has a synth, eight audio FX (MAX_AUDIO_FX) and eight MIDI FX
// (MAX_MIDI_FX); Master FX has eight positions (MASTER_FX_SLOTS). The Remote UI
// used to know four of a slot's seventeen (synth, fx1, fx2, midi_fx1) and four
// of Master FX's eight, so a module in any other position loaded, ran and made
// sound with no controls in the browser at all.
//
// Reading seventeen "<comp>_module" keys per slot per poll would make the
// Remote UI the busiest reader on the channel shadow_ui draws with (~2.8 ms
// each). So a slot is ONE read — the chain's "chain_modules" — and Master FX is
// ONE read — the shim's "master_fx:modules". Either falls back to per-position
// reads only when the key is not served at all (an older build), never on a
// timeout.
// ---------------------------------------------------------------------------

const (
	chainFxPositions     = 8 // MAX_AUDIO_FX
	chainMidiFxPositions = 8 // MAX_MIDI_FX
	masterFxPositions    = 8 // MASTER_FX_SLOTS
)

// componentPrefixes is every component a chain slot can hold.
var componentPrefixes = func() []string {
	out := []string{"synth"}
	for i := 1; i <= chainFxPositions; i++ {
		out = append(out, "fx"+strconv.Itoa(i))
	}
	for i := 1; i <= chainMidiFxPositions; i++ {
		out = append(out, "midi_fx"+strconv.Itoa(i))
	}
	return out
}()

// masterFxSlots is every Master FX position.
var masterFxSlots = func() []string {
	out := make([]string, masterFxPositions)
	for i := range out {
		out[i] = "fx" + strconv.Itoa(i+1)
	}
	return out
}()

// componentPosition parses "fx3" / "midi_fx2" -> (kind, 1-based position).
// kind is "synth", "fx" or "midi_fx"; ok=false for anything else.
func componentPosition(comp string) (kind string, pos int, ok bool) {
	if comp == "synth" {
		return "synth", 0, true
	}
	var rest string
	switch {
	case strings.HasPrefix(comp, "midi_fx"):
		kind, rest = "midi_fx", comp[len("midi_fx"):]
	case strings.HasPrefix(comp, "fx"):
		kind, rest = "fx", comp[len("fx"):]
	default:
		return "", 0, false
	}
	if rest == "" || rest[0] == '0' {
		return "", 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return "", 0, false
	}
	limit := chainFxPositions
	if kind == "midi_fx" {
		limit = chainMidiFxPositions
	}
	if n < 1 || n > limit {
		return "", 0, false
	}
	return kind, n, true
}

// isChainComponent reports whether comp names a slot component. Master FX keys
// ("master_fx:fx1:...") split differently and are excluded.
func isChainComponent(comp string) bool {
	_, _, ok := componentPosition(comp)
	return ok
}

// componentLabel is the name a person reads: "Synth", "Audio FX 3", "MIDI FX 1".
func componentLabel(comp string) string {
	kind, pos, ok := componentPosition(comp)
	switch {
	case !ok:
		return comp
	case kind == "synth":
		return "Synth"
	case kind == "fx":
		return fmt.Sprintf("Audio FX %d", pos)
	default:
		return fmt.Sprintf("MIDI FX %d", pos)
	}
}

// slotModules is what a slot holds: component -> module id, LOADED positions
// only. An absent component is empty.
type slotModules map[string]string

// parseChainModules decodes the chain's "chain_modules" answer.
func parseChainModules(raw string) (slotModules, bool) {
	var doc struct {
		Synth  *string   `json:"synth"`
		MidiFx *[]string `json:"midi_fx"`
		Fx     *[]string `json:"fx"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil ||
		doc.Synth == nil || doc.MidiFx == nil || doc.Fx == nil {
		return nil, false
	}
	out := slotModules{}
	if *doc.Synth != "" {
		out["synth"] = *doc.Synth
	}
	for i, id := range *doc.Fx {
		if i < chainFxPositions && id != "" {
			out["fx"+strconv.Itoa(i+1)] = id
		}
	}
	for i, id := range *doc.MidiFx {
		if i < chainMidiFxPositions && id != "" {
			out["midi_fx"+strconv.Itoa(i+1)] = id
		}
	}
	return out, true
}

// errNotServed: the key is not answered by this build at all.
var errNotServed = errors.New("not served")

// readSlotModules reads what a slot holds. get is a blocking or non-blocking
// reader; any error that is not an ANSWERED refusal (a timeout, a busy
// channel) is returned as-is, so a caller never mistakes "could not ask" for
// "the slot is empty".
func readSlotModules(get paramGetter, slot uint8) (slotModules, error) {
	raw, err := get(slot, "chain_modules")
	if err == nil {
		if m, ok := parseChainModules(raw); ok {
			return m, nil
		}
		// Served but not ours to parse: treat as unserved and ask the old way.
	} else if !paramAnswered(err) {
		return nil, err
	}

	// An older chain: the synth, the two counts, then each position.
	out := slotModules{}
	synth, err := get(slot, "synth_module")
	if err != nil {
		if paramAnswered(err) {
			return out, nil // no chain instance answers here: an empty slot
		}
		return nil, err
	}
	if synth != "" {
		out["synth"] = synth
	}
	for _, sec := range []struct {
		kind  string
		count string
		limit int
	}{{"fx", "fx_count", chainFxPositions}, {"midi_fx", "midi_fx_count", chainMidiFxPositions}} {
		n := sec.limit // a count that is not served: probe every position
		if v, err := get(slot, sec.count); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(v))
		} else if !paramAnswered(err) {
			return nil, err
		}
		if n > sec.limit {
			n = sec.limit
		}
		for i := 1; i <= n; i++ {
			comp := sec.kind + strconv.Itoa(i)
			id, err := get(slot, comp+"_module")
			if err != nil {
				if paramAnswered(err) {
					continue
				}
				return nil, err
			}
			if id != "" {
				out[comp] = id
			}
		}
	}
	return out, nil
}

// parseMasterFxModules decodes the shim's "master_fx:modules": a positional
// array of {"id","path"}, eight long, holes included. Keys are "fx1".."fx8".
func parseMasterFxModules(raw string) (slotModules, bool) {
	var doc []struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || doc == nil {
		return nil, false
	}
	out := slotModules{}
	for i, p := range doc {
		if i >= masterFxPositions {
			break
		}
		id := p.ID
		if id == "" {
			id = masterFxModuleID(p.Path)
		}
		if id != "" {
			out["fx"+strconv.Itoa(i+1)] = masterFxModuleID(id)
		}
	}
	return out, true
}

// readMasterFxModules reads what Master FX holds, with the same error rule as
// readSlotModules.
func readMasterFxModules(get paramGetter) (slotModules, error) {
	raw, err := get(0, "master_fx:modules")
	if err == nil {
		if m, ok := parseMasterFxModules(raw); ok {
			return m, nil
		}
	} else if !paramAnswered(err) {
		return nil, err
	}
	out := slotModules{}
	for _, fx := range masterFxSlots {
		v, err := get(0, "master_fx:"+fx+":module")
		if err != nil {
			if paramAnswered(err) {
				continue
			}
			return nil, err
		}
		if id := masterFxModuleID(v); id != "" {
			out[fx] = id
		}
	}
	return out, nil
}

// componentsOfKind lists every position of one kind ("fx" or "midi_fx").
func componentsOfKind(kind string) []string {
	var out []string
	for _, c := range componentPrefixes {
		if k, _, _ := componentPosition(c); k == kind {
			out = append(out, c)
		}
	}
	return out
}

// orderedComponents lists a slot's loaded components in signal-flow order:
// MIDI FX, then the synth, then audio FX — the order the Remote UI draws them.
func (m slotModules) orderedComponents() []string {
	var out []string
	for i := 1; i <= chainMidiFxPositions; i++ {
		if c := "midi_fx" + strconv.Itoa(i); m[c] != "" {
			out = append(out, c)
		}
	}
	if m["synth"] != "" {
		out = append(out, "synth")
	}
	for i := 1; i <= chainFxPositions; i++ {
		if c := "fx" + strconv.Itoa(i); m[c] != "" {
			out = append(out, c)
		}
	}
	return out
}

// tryGetter adapts the non-blocking TryGetParam to a paramGetter: a busy
// channel is an error that is NOT an answered refusal, so the reader backs off.
func tryGetter(shm *ShmParams) paramGetter {
	return func(slot uint8, key string) (string, error) {
		v, ok, err := shm.TryGetParam(slot, key)
		if !ok {
			return "", errBusy
		}
		return v, err
	}
}

var errBusy = errors.New("param channel busy")

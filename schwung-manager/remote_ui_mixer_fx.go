package main

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// The Mixer's EFFECT knobs: one per loaded audio FX on each slot's strip,
// driving that effect's dry/wet.
//
// WHICH PARAMETER. A module may say, in module.json:
//
//	"capabilities": { "mixer_params": ["wet"] }
//
// -- keys as its chain_params names them, without the component prefix, at most
// mixerFxMaxPerModule. Without a declaration the Mixer takes the FIRST exact
// match of mixerFxGuessKeys from the module's chain_params and marks the knob
// guessed. Exact, never a substring: "early_mix", "noise_mix", "carrier_mix" and
// "fx1_amount" are all real fleet parameters that are not the effect's mix.
// Measured over the 40 audio FX in tests/fixtures/module-contracts.json: 26
// have "mix", 3 "wet" and 1 "dry_wet"; the other 10 have no single mix.
//
// WHAT IT COSTS. Which effects a slot holds is ONE read (chain_modules), taken
// on subscribe, when the notify ring shows a module or chain-shape write on that
// slot, and in the backstop rotation. A module's chain_params is read ONCE per
// module id for the life of the manager; module.json is read from disk.
// ---------------------------------------------------------------------------

const (
	mixerFxMaxPerModule = 4
	mixerModulesDelay   = 300 * time.Millisecond // after a module / shape write
)

var mixerFxGuessKeys = []string{"mix", "dry_wet", "wet"}

// mixerFxKnob is one knob on a strip.
type mixerFxKnob struct {
	Comp    string  `json:"comp"`   // "fx3"
	Module  string  `json:"module"` // "freeverb"
	Key     string  `json:"key"`    // the real param key, "fx3:wet"
	Name    string  `json:"name"`   // "Wet"
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Step    float64 `json:"step"`
	Int     bool    `json:"int,omitempty"`
	Unit    string  `json:"unit,omitempty"`
	Guessed bool    `json:"guessed,omitempty"`
	// Pinned: every knob on a strip is a pin now (remote_ui_pins.go), so
	// every one can be unpinned. Effect: this pin is its module's mix -- drawn
	// under "Effects" rather than "Pinned".
	Pinned bool `json:"pinned,omitempty"`
	Effect bool `json:"effect,omitempty"`
	// Options: a choice parameter (an enum) -- drawn as a dropdown, its value
	// the option's index.
	Options []string `json:"options,omitempty"`
}

// fxParamMeta is the slice of a chain_params entry a knob needs.
type fxParamMeta struct {
	Key       string   `json:"key"`
	Name      string   `json:"name"`
	ShortName string   `json:"short_name"`
	Type      string   `json:"type"`
	Min       *float64 `json:"min"`
	Max       *float64 `json:"max"`
	Step      float64  `json:"step"`
	Unit      string   `json:"unit"`
	Options   []string `json:"options"`
}

// numeric: a parameter with a real range a fader can drive.
func (m fxParamMeta) numeric() bool {
	if m.Key == "" || m.Min == nil || m.Max == nil || !(*m.Max > *m.Min) {
		return false
	}
	return m.Type == "" || m.Type == "float" || m.Type == "int"
}

// choice: an enum with at least two options, drawn as a dropdown.
func (m fxParamMeta) choice() bool {
	return m.Key != "" && m.Type == "enum" && len(m.Options) >= 2
}

// usable: something a strip can draw -- a fader or a dropdown.
func (m fxParamMeta) usable() bool { return m.numeric() || m.choice() }

// fxKnobTemplate is a knob before it is placed in a position.
type fxKnobTemplate struct {
	param   fxParamMeta
	guessed bool
}

// pickFxKnobs chooses a module's mixer knobs from its declaration (may be nil)
// and its chain_params.
func pickFxKnobs(declared []string, params []fxParamMeta) []fxKnobTemplate {
	byKey := make(map[string]fxParamMeta, len(params))
	for _, p := range params {
		if p.usable() {
			if _, dup := byKey[p.Key]; !dup {
				byKey[p.Key] = p
			}
		}
	}
	var out []fxKnobTemplate
	if len(declared) > 0 {
		for _, k := range declared {
			if p, ok := byKey[k]; ok && len(out) < mixerFxMaxPerModule {
				out = append(out, fxKnobTemplate{param: p})
			}
		}
		return out // declared: exactly what was declared, nothing guessed beside it
	}
	for _, k := range mixerFxGuessKeys {
		if p, ok := byKey[k]; ok && p.numeric() {
			return []fxKnobTemplate{{param: p, guessed: true}}
		}
	}
	return nil
}

func (t fxKnobTemplate) place(comp, module string) mixerFxKnob {
	name := t.param.Name
	if name == "" {
		name = t.param.Key
	}
	if t.param.choice() {
		return mixerFxKnob{Comp: comp, Module: module, Key: comp + ":" + t.param.Key, Name: name,
			Min: 0, Max: float64(len(t.param.Options) - 1), Step: 1, Int: true,
			Options: append([]string{}, t.param.Options...), Guessed: t.guessed}
	}
	step := t.param.Step
	isInt := t.param.Type == "int"
	if step <= 0 {
		if isInt {
			step = 1
		} else {
			step = (*t.param.Max - *t.param.Min) / 100
		}
	}
	return mixerFxKnob{Comp: comp, Module: module, Key: comp + ":" + t.param.Key, Name: name,
		Min: *t.param.Min, Max: *t.param.Max, Step: step, Int: isInt, Unit: t.param.Unit,
		Guessed: t.guessed}
}

// parseFxParams decodes chain_params entry by entry: one entry the struct
// cannot hold costs that entry, not the declaration.
func parseFxParams(raw string) ([]fxParamMeta, bool) {
	var entries []json.RawMessage
	if raw == "" || json.Unmarshal([]byte(raw), &entries) != nil {
		return nil, false
	}
	out := make([]fxParamMeta, 0, len(entries))
	for _, e := range entries {
		var m fxParamMeta
		if json.Unmarshal(e, &m) == nil {
			out = append(out, m)
		}
	}
	return out, true
}

// moduleMixerParams reads capabilities.mixer_params from a module's
// module.json on disk; nil when absent or unreadable.
func moduleMixerParams(basePath, moduleID string) []string {
	if basePath == "" || moduleID == "" || strings.ContainsAny(moduleID, `/\`) {
		return nil
	}
	for _, dir := range moduleInstallDirs(basePath) {
		raw, err := os.ReadFile(filepath.Join(dir, moduleID, "module.json"))
		if err != nil {
			continue
		}
		var doc struct {
			Capabilities struct {
				MixerParams []string `json:"mixer_params"`
			} `json:"capabilities"`
			MixerParams []string `json:"mixer_params"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			return nil
		}
		if len(doc.Capabilities.MixerParams) > 0 {
			return doc.Capabilities.MixerParams
		}
		return doc.MixerParams
	}
	return nil
}

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

type mixerFxState struct {
	mu      sync.Mutex
	knobs   map[int][]mixerFxKnob       // slot -> knobs, in position order
	read    map[int]bool                // slot -> its module list has been read
	dirty   map[int]bool                // slot -> knobs changed since the last flush
	byMod   map[string][]fxKnobTemplate // module id -> templates (definitive reads only)
	params  map[string][]fxParamMeta    // module id -> its chain_params (definitive reads only)
	timers  map[int]*time.Timer         // pending module refresh per slot
	cursor  int                         // backstop: which slot's module list next
	running sync.WaitGroup
}

func (fs *mixerFxState) init() {
	if fs.knobs == nil {
		fs.knobs = make(map[int][]mixerFxKnob)
		fs.read = make(map[int]bool)
		fs.dirty = make(map[int]bool)
		fs.byMod = make(map[string][]fxKnobTemplate)
		fs.params = make(map[string][]fxParamMeta)
		fs.timers = make(map[int]*time.Timer)
	}
}

// knob returns the slot's knob for key, if the Mixer is showing one.
func (fs *mixerFxState) knob(slot int, key string) (mixerFxKnob, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, k := range fs.knobs[slot] {
		if k.Key == key {
			return k, true
		}
	}
	return mixerFxKnob{}, false
}

// keys lists the knob keys of one slot, or of every slot for slot -1.
func (fs *mixerFxState) keys(slot int) []mixerKeyRef {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []mixerKeyRef
	for s := 0; s < mixerStrips; s++ {
		if slot >= 0 && s != slot {
			continue
		}
		for _, k := range fs.knobs[s] {
			out = append(out, mixerKeyRef{s, k.Key})
		}
	}
	return out
}

// takeDirty returns the changed slots' knob lists and clears the set.
func (fs *mixerFxState) takeDirty() map[string][]mixerFxKnob {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.dirty) == 0 {
		return nil
	}
	out := make(map[string][]mixerFxKnob, len(fs.dirty))
	for s := range fs.dirty {
		out[stripName(s)] = append([]mixerFxKnob{}, fs.knobs[s]...)
	}
	fs.dirty = make(map[int]bool)
	return out
}

// snapshot returns every read slot's knob list.
func (fs *mixerFxState) snapshot() map[string][]mixerFxKnob {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make(map[string][]mixerFxKnob, len(fs.read))
	for s := range fs.read {
		out[stripName(s)] = append([]mixerFxKnob{}, fs.knobs[s]...)
	}
	return out
}

// normalizeFxValue clamps a reported value into the knob's range, or refuses it.
// A choice may be reported by NAME; it is stored as its index.
func normalizeFxValue(k mixerFxKnob, value string) (string, bool) {
	if len(k.Options) > 0 {
		for i, o := range k.Options {
			if o == strings.TrimSpace(value) {
				return strconv.Itoa(i), true
			}
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return "", false
	}
	if f < k.Min || f > k.Max {
		return "", false
	}
	if k.Int {
		return strconv.Itoa(int(math.Round(f))), true
	}
	return strconv.FormatFloat(f, 'f', -1, 64), true
}

// validFxWrite checks a browser write against the knob's range.
func validFxWrite(k mixerFxKnob, value string) bool {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < k.Min || f > k.Max {
		return false
	}
	return !k.Int || f == math.Trunc(f)
}

// ---------------------------------------------------------------------------
// Refreshing a slot's knobs
// ---------------------------------------------------------------------------

// mixerFxModuleKey: does this notified write change which modules a slot holds?
func mixerFxModuleKey(key string) bool {
	switch key {
	case "load_file", "load_patch", "patch", "clear", "fx:insert", "fx:remove", "fx:move",
		"synth:module", "update_patch", "update_patch_by_name":
		return true
	}
	if strings.HasSuffix(key, ":module") {
		return isChainComponent(strings.TrimSuffix(key, ":module"))
	}
	return false
}

// mixerScheduleModules re-reads slot's module list after delay, joining a
// pending request rather than pushing it out (see mixerScheduleRefresh).
func (ru *RemoteUI) mixerScheduleModules(ctx context.Context, slot int, delay time.Duration) {
	fs := &ru.mix.fx
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.init()
	if ctx.Err() != nil {
		return
	}
	if _, ok := fs.timers[slot]; ok {
		return
	}
	fs.running.Add(1)
	fs.timers[slot] = time.AfterFunc(delay, func() {
		defer fs.running.Done()
		fs.mu.Lock()
		delete(fs.timers, slot)
		fs.mu.Unlock()
		if ctx.Err() == nil {
			ru.mixerRefreshModules(ctx, slot, ru.blockingGetterIfShm())
		}
	})
}

func (ru *RemoteUI) blockingGetterIfShm() paramGetter {
	shm := ru.ensureShm()
	if shm == nil {
		return nil
	}
	return func(slot uint8, key string) (string, error) { return shm.GetParam(slot, key) }
}

// mixerRefreshModules reads what a strip holds (a slot, or an FX bus),
// rebuilds its knobs, and reads the value of any knob that is new. get may be
// the non-blocking reader (backstop).
func (ru *RemoteUI) mixerRefreshModules(ctx context.Context, slot int, get paramGetter) bool {
	if get == nil || slot < 0 || slot >= mixerStrips {
		return false
	}
	mods, err := readStripModules(get, slot)
	if err != nil {
		return false
	}
	// Each effect's mix is pinned by default the first time it is seen in its
	// position (remote_ui_pins.go); after that it is an ordinary pin.
	if !ru.offerDefaultPins(ctx, slot, mods, get) {
		return false
	}
	// No set to keep pins in (off-device, or active_set.txt unreadable): show
	// the effects' mixes as before, just not removable, rather than nothing.
	ru.pins.mu.Lock()
	_, pinsErr := readPins(ru.basePath)
	ru.pins.mu.Unlock()
	if pinsErr != nil {
		var fallback []mixerFxKnob
		for i := 1; i <= chainFxPositions; i++ {
			comp := "fx" + strconv.Itoa(i)
			if id := mods[comp]; id != "" {
				tmpl, ok := ru.mixerFxTemplates(id, stripComp(slot, comp), readSlotOf(slot), get)
				if !ok {
					return false
				}
				for _, t := range tmpl {
					k := placeOnStrip(t, slot, comp, id)
					k.Effect = true
					fallback = append(fallback, k)
				}
			}
		}
		return ru.mixerSetKnobs(ctx, slot, fallback, get)
	}

	// The strip is the slot's PINS. A pin made on a module that is no longer
	// in its position is hidden, never pointed at a stranger.
	var effects, others []mixerFxKnob
	have := map[string]bool{}
	for _, pin := range ru.slotPins(slot) {
		comp, param, ok := splitStripKey(slot, pin.Key)
		if !ok {
			continue
		}
		id := mods[comp]
		if id == "" || (pin.Module != "" && pin.Module != id) || have[pin.Key] {
			continue
		}
		params, ok := ru.moduleParams(id, stripComp(slot, comp), readSlotOf(slot), get)
		if !ok {
			return false
		}
		meta, ok := findUsableParam(params, param)
		if !ok {
			continue
		}
		k := placeOnStrip(fxKnobTemplate{param: meta}, slot, comp, id)
		k.Pinned = true
		have[pin.Key] = true
		// Its module's mix? Then it reads as one of the slot's effects.
		if kind, _, _ := componentPosition(comp); kind == "fx" {
			if tmpl, ok := ru.mixerFxTemplates(id, stripComp(slot, comp), readSlotOf(slot), get); ok {
				for _, t := range tmpl {
					if t.param.Key == param {
						k.Effect, k.Guessed = true, t.guessed
					}
				}
			}
		}
		if k.Effect {
			effects = append(effects, k)
		} else {
			others = append(others, k)
		}
	}
	// Effects in signal-flow order, then the other pins in the order pinned.
	sort.SliceStable(effects, func(i, j int) bool {
		_, pi, _ := componentPosition(effects[i].Comp)
		_, pj, _ := componentPosition(effects[j].Comp)
		return pi < pj
	})
	knobs := append(effects, others...)

	return ru.mixerSetKnobs(ctx, slot, knobs, get)
}

// placeOnStrip places a knob on a strip: its key is the strip's full key
// ("send1:fx1:mix" on a bus), its Comp the position as the strip names it.
func placeOnStrip(t fxKnobTemplate, strip int, comp, module string) mixerFxKnob {
	k := t.place(stripComp(strip, comp), module)
	k.Comp = comp
	return k
}

// mixerSetKnobs installs a slot's knob list and reads the values of new ones.
func (ru *RemoteUI) mixerSetKnobs(ctx context.Context, slot int, knobs []mixerFxKnob, get paramGetter) bool {
	fs := &ru.mix.fx
	fs.mu.Lock()
	fs.init()
	old := fs.knobs[slot]
	changed := !fs.read[slot] || !sameKnobs(old, knobs)
	fs.read[slot] = true
	if changed {
		fs.knobs[slot] = knobs
		fs.dirty[slot] = true
	}
	fs.mu.Unlock()

	if changed {
		// Values of knobs that went away are not the Mixer's any more.
		keep := map[string]bool{}
		for _, k := range knobs {
			keep[k.Key] = true
		}
		for _, k := range old {
			if !keep[k.Key] {
				ru.mix.store.del(mixerStoreKey(slot, k.Key))
			}
		}
		var fresh []mixerKeyRef
		for _, k := range knobs {
			fresh = append(fresh, mixerKeyRef{slot, k.Key})
		}
		ru.mix.kick()
		ru.mixerReadFx(ctx, fresh, get)
	}
	return true
}

// moduleParams returns a module's chain_params, read through position comp
// once per module id for the life of the manager. ok=false: the read did not
// complete (never cached, asked again next time).
func (ru *RemoteUI) moduleParams(id, comp string, slot uint8, get paramGetter) ([]fxParamMeta, bool) {
	fs := &ru.mix.fx
	fs.mu.Lock()
	fs.init()
	p, ok := fs.params[id]
	fs.mu.Unlock()
	if ok {
		return p, true
	}
	raw, err := get(slot, comp+":chain_params")
	if err != nil {
		if paramAnswered(err) {
			return nil, true // answered: this module has no parameters to offer
		}
		return nil, false
	}
	params, parsed := parseFxParams(raw)
	if parsed {
		fs.mu.Lock()
		fs.params[id] = params
		fs.mu.Unlock()
	}
	return params, true
}

// mixerFxTemplates returns a module's mix knob templates: cached per module
// id, else from its declaration and its chain_params.
func (ru *RemoteUI) mixerFxTemplates(id, comp string, slot uint8, get paramGetter) ([]fxKnobTemplate, bool) {
	fs := &ru.mix.fx
	fs.mu.Lock()
	fs.init()
	t, ok := fs.byMod[id]
	fs.mu.Unlock()
	if ok {
		return t, true
	}
	params, ok := ru.moduleParams(id, comp, slot, get)
	if !ok {
		return nil, false
	}
	t = pickFxKnobs(moduleMixerParams(ru.basePath, id), params)
	fs.mu.Lock()
	_, definitive := fs.params[id]
	if definitive {
		fs.byMod[id] = t
	}
	fs.mu.Unlock()
	return t, true
}

// mixerReadFx reads knob values one at a time, yielding between them.
func (ru *RemoteUI) mixerReadFx(ctx context.Context, keys []mixerKeyRef, get paramGetter) {
	for i, k := range keys {
		if ctx.Err() != nil {
			return
		}
		knob, ok := ru.mix.fx.knob(k.slot, k.key)
		if !ok {
			continue
		}
		since := ru.mix.store.begin()
		raw, err := get(readSlotOf(k.slot), k.key)
		if v, ok := normalizeFxValue(knob, raw); err == nil && ok {
			if ru.mix.store.putRead(k.storeKey(), v, since) {
				ru.mix.kick()
			}
		}
		if i < len(keys)-1 {
			time.Sleep(mixerReadYield)
		}
	}
}

func sameKnobs(a, b []mixerFxKnob) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

package main

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// The Mixer tab — the four slots' levels, pans and sends, and the two send
// returns, on one page.
//
// It writes the SAME keys as the device's Slot Settings and the E16/EC4 Mixer
// (surfaceMixerIo in shadow_ui.js), so the browser, a hardware surface, the
// slot grid and Move's own track knob all move one value. Nothing here is a
// second model of the mix.
//
// WHERE EACH VALUE COMES FROM, and why there is exactly one source per key:
//
//   name, slot:volume, slot:muted, slot:soloed
//       /schwung-ui (shmuistate.go), polled every mixerPollInterval. Free to
//       read, and the only place Move-driven changes show up at all -- Move's
//       own track volume/mute/solo never touch the notify ring. These are
//       deliberately NOT taken from the notify ring as well: two sources that
//       report at different moments (and, against a v2 shim, at different
//       precision) move the fader back and forth on every turn.
//
//   slot:pan, buses:main_send1/2, send1:return, send2:return, send1:to_send2
//       Not in any segment, so: the notify ring for every write made through
//       the param channel or the web set ring; one full read when a browser
//       subscribes and after a set switch (set_aligned) or a slot's patch
//       changes; and a ONE-READ-PER-SECOND round-robin backstop for whatever
//       moved without telling anyone (a patch's saved send levels landing).
//       A param read is ~2.8 ms on the channel shadow_ui draws with, so these
//       are never read in a burst and never per frame.
//
// ONE WRITER. Every mixer message to a browser is sent by mixerLoop. Notify
// events and reads only update the store and kick the loop. That is what makes
// the ordering safe: a browser that just subscribed is sent a FULL snapshot by
// the same goroutine that sends deltas, so it can never receive a snapshot that
// is older than a delta it has already applied. A delta that could not be
// delivered (the client's previous write still in flight) marks that client for
// a full resync rather than being lost.
// ---------------------------------------------------------------------------

const (
	mixerSlots = uiStateSlots

	mixerPollInterval   = 50 * time.Millisecond  // /schwung-ui poll + flush
	mixerBackstopEvery  = 1 * time.Second        // one param read per this
	mixerSetSwitchDelay = 1 * time.Second        // after set_aligned
	mixerSlotPatchDelay = 500 * time.Millisecond // after a slot's patch name changes
	mixerReadYield      = 10 * time.Millisecond  // between reads of one refresh

	mixerNameKey = "name" // read-only, from /schwung-ui

	mixerSendLevelMax = 127 // BUS_MIX_SEND_LEVEL_MAX
	mixerVolumeMax    = 4.0 // slot:volume clamp in shadow_handle_slot_param_set
)

// Per-slot keys served from /schwung-ui.
var mixerSlotUIKeys = []string{"slot:volume", "slot:muted", "slot:soloed"}

// Per-slot keys that have to be READ over the param channel.
var mixerSlotParamKeys = []string{"slot:pan", "buses:main_send1", "buses:main_send2"}

// Device-wide keys (slot index ignored by the shim; sent as 0).
var mixerGlobalKeys = []string{"send1:return", "send2:return", "send1:to_send2"}

// mixerKeyRef names one readable mixer value. slot is -1 for a global key.
type mixerKeyRef struct {
	slot int
	key  string
}

func (k mixerKeyRef) storeKey() string { return mixerStoreKey(k.slot, k.key) }

// mixerStoreKey: "<slot>|<key>" for a slot value, "g|<key>" for a global one.
func mixerStoreKey(slot int, key string) string {
	if slot < 0 {
		return "g|" + key
	}
	return strconv.Itoa(slot) + "|" + key
}

// mixerReadKeys lists every value that comes from the param channel, slot by
// slot and then the globals. slot >= 0 limits it to that slot.
func mixerReadKeys(slot int) []mixerKeyRef {
	var out []mixerKeyRef
	for s := 0; s < mixerSlots; s++ {
		if slot >= 0 && s != slot {
			continue
		}
		for _, k := range mixerSlotParamKeys {
			out = append(out, mixerKeyRef{s, k})
		}
	}
	if slot < 0 {
		for _, k := range mixerGlobalKeys {
			out = append(out, mixerKeyRef{-1, k})
		}
	}
	return out
}

func containsKey(list []string, k string) bool {
	for _, s := range list {
		if s == k {
			return true
		}
	}
	return false
}

// mixerWriteScope says whether the browser may write key, and whether it is
// per-slot. Anything else is refused: the Mixer is not a general param
// channel, and the set_param path already exists for that.
func mixerWriteScope(key string) (perSlot bool, ok bool) {
	if containsKey(mixerSlotUIKeys, key) || containsKey(mixerSlotParamKeys, key) {
		return true, true
	}
	if containsKey(mixerGlobalKeys, key) {
		return false, true
	}
	return false, false
}

// validMixerValue checks a value against the key's real range. The shim clamps
// too; this is so a malformed write is an error the browser hears about rather
// than an atoi() that quietly becomes 0.
func validMixerValue(key, value string) bool {
	switch key {
	case "slot:muted", "slot:soloed":
		return value == "0" || value == "1"
	case "slot:volume":
		f, err := strconv.ParseFloat(value, 64)
		return err == nil && !math.IsNaN(f) && f >= 0 && f <= mixerVolumeMax
	case "slot:pan":
		f, err := strconv.ParseFloat(value, 64)
		return err == nil && !math.IsNaN(f) && f >= -1 && f <= 1
	case "buses:main_send1", "buses:main_send2",
		"send1:return", "send2:return", "send1:to_send2":
		n, err := strconv.Atoi(value)
		return err == nil && n >= 0 && n <= mixerSendLevelMax
	}
	return false
}

// normalizeMixerValue turns a value the DEVICE reported (a read, or a write
// echoed on the notify ring) into one the Mixer can show, or refuses it.
//
// Lenient where the shim is: it atoi()s the int levels, so a device-side write
// of "64.0" is 64 to the mix and is 64 here. "" is served-and-empty, which none
// of these keys legitimately answer, and a value out of the key's range is not
// one to draw -- either way the key keeps its last known value, or stays
// unknown, rather than becoming a picture of a failed read.
func normalizeMixerValue(key, value string) (string, bool) {
	if value == "" {
		return "", false
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return "", false
	}
	switch key {
	case "slot:pan":
		if f < -1 || f > 1 {
			return "", false
		}
		return value, true
	case "slot:volume":
		if f < 0 || f > mixerVolumeMax {
			return "", false
		}
		return value, true
	case "slot:muted", "slot:soloed":
		return boolDigit(f != 0), true
	case "buses:main_send1", "buses:main_send2",
		"send1:return", "send2:return", "send1:to_send2":
		n := int(f) // atoi truncates
		if n < 0 || n > mixerSendLevelMax {
			return "", false
		}
		return strconv.Itoa(n), true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

// mixerStore holds the last known value of every mixer key, plus which keys
// changed since the loop last flushed.
//
// Values carry a sequence stamp so a slow READ cannot overwrite a newer
// NOTIFIED value: a read records the sequence before it starts and only lands
// if nothing newer has been put since (putRead). Notifications and the
// /schwung-ui poll are authoritative and always land (put).
type mixerStore struct {
	mu    sync.Mutex
	seq   uint64
	vals  map[string]mixerVal
	dirty map[string]bool
}

type mixerVal struct {
	v   string
	seq uint64
}

func (m *mixerStore) init() {
	if m.vals == nil {
		m.vals = make(map[string]mixerVal)
		m.dirty = make(map[string]bool)
	}
}

// begin returns the sequence a read should pass to putRead.
func (m *mixerStore) begin() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq
}

// put stores an authoritative value. Reports whether it changed.
func (m *mixerStore) put(k, v string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.seq++
	old, had := m.vals[k]
	m.vals[k] = mixerVal{v: v, seq: m.seq}
	if had && old.v == v {
		return false
	}
	m.dirty[k] = true
	return true
}

// putRead stores a value from a read that began at `since`, unless something
// newer was put for that key in the meantime. Reports whether it changed.
func (m *mixerStore) putRead(k, v string, since uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	old, had := m.vals[k]
	if had && old.seq > since {
		return false // a notification overtook this read
	}
	m.seq++
	m.vals[k] = mixerVal{v: v, seq: m.seq}
	if had && old.v == v {
		return false
	}
	m.dirty[k] = true
	return true
}

// get returns a stored value.
func (m *mixerStore) get(k string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.vals[k]
	return v.v, ok
}

// takeDirty returns the changed keys' current values and clears the set.
func (m *mixerStore) takeDirty() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.dirty) == 0 {
		return nil
	}
	out := make(map[string]string, len(m.dirty))
	for k := range m.dirty {
		out[k] = m.vals[k].v
	}
	m.dirty = make(map[string]bool)
	return out
}

// snapshot returns every known value.
func (m *mixerStore) snapshot() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.vals))
	for k, v := range m.vals {
		out[k] = v.v
	}
	return out
}

// ---------------------------------------------------------------------------
// Wire format
// ---------------------------------------------------------------------------

// wsMixer is both messages:
//
//	mixer_state   the whole mixer. A key that is ABSENT is UNKNOWN — the
//	              browser shows it as such and does not let it be dragged.
//	mixer_update  only what changed; an absent key is unchanged.
//
// Values are the strings the device serves, keyed by the real param key, so
// what the browser shows and what it writes back are the same names.
type wsMixer struct {
	Type   string                       `json:"type"`
	Slots  map[string]map[string]string `json:"slots,omitempty"`  // "0".."3" -> key -> value
	Global map[string]string            `json:"global,omitempty"` // send returns, A->B
	// Live: whether /schwung-ui could be read. Without it there is no volume,
	// mute, solo or name; sent on every mixer_state and whenever it changes.
	Live *bool `json:"live,omitempty"`
}

// mixerMessage turns store keys into the wire shape.
func mixerMessage(typ string, vals map[string]string) wsMixer {
	msg := wsMixer{Type: typ}
	for k, v := range vals {
		i := 0
		for i < len(k) && k[i] != '|' {
			i++
		}
		if i >= len(k) {
			continue
		}
		scope, key := k[:i], k[i+1:]
		if scope == "g" {
			if msg.Global == nil {
				msg.Global = make(map[string]string)
			}
			msg.Global[key] = v
			continue
		}
		if msg.Slots == nil {
			msg.Slots = make(map[string]map[string]string)
		}
		if msg.Slots[scope] == nil {
			msg.Slots[scope] = make(map[string]string)
		}
		msg.Slots[scope][key] = v
	}
	return msg
}

// ---------------------------------------------------------------------------
// RemoteUI state for the Mixer
// ---------------------------------------------------------------------------

// mixerState is RemoteUI's Mixer half. The zero value is ready to use.
type mixerState struct {
	store mixerStore

	uiMu       sync.Mutex
	ui         *ShmUIState // lazily attached; see ensureUIState
	uiAttempts time.Time   // last failed attach, so absence costs one open a second

	// Touched only by mixerLoop.
	uiSeen   bool
	lastLive bool
	cursor   int // backstop round-robin position

	kickOnce sync.Once
	kickCh   chan struct{}

	backstopBusy atomic.Bool

	refreshMu     sync.Mutex
	refreshTimers map[int]*time.Timer // slot, or -1 for everything

	// inflight counts scheduled refreshes and running backstop reads, so a
	// shutdown (or a test) can wait for the param channel to go quiet.
	inflight sync.WaitGroup
}

// waitIdle blocks until no refresh or backstop read is pending or running.
func (ms *mixerState) waitIdle() { ms.inflight.Wait() }

func (ms *mixerState) kickChan() chan struct{} {
	ms.kickOnce.Do(func() { ms.kickCh = make(chan struct{}, 1) })
	return ms.kickCh
}

// kick asks mixerLoop to flush now rather than at its next tick.
func (ms *mixerState) kick() {
	select {
	case ms.kickChan() <- struct{}{}:
	default:
	}
}

// ensureUIState attaches /schwung-ui on demand. The manager can start before
// the shim creates the segment (measured for /schwung-perf and
// /schwung-param), so a single attempt at startup would leave the Mixer blind
// for the life of the process.
func (ru *RemoteUI) ensureUIState() *ShmUIState {
	ru.mix.uiMu.Lock()
	defer ru.mix.uiMu.Unlock()
	if ru.mix.ui == nil && time.Since(ru.mix.uiAttempts) >= time.Second {
		if ru.mix.ui = OpenShmUIState(); ru.mix.ui != nil {
			ru.logger.Info("slot ui state: connected")
		} else {
			ru.mix.uiAttempts = time.Now()
		}
	}
	return ru.mix.ui
}

func (ru *RemoteUI) mixerClients() []*ruClient {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	var out []*ruClient
	for c := range ru.clients {
		c.mu.Lock()
		sub := c.mixerSub
		c.mu.Unlock()
		if sub {
			out = append(out, c)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// WebSocket handlers
// ---------------------------------------------------------------------------

func (ru *RemoteUI) handleSubscribeMixer(ctx context.Context, c *ruClient) {
	c.mu.Lock()
	c.mixerSub = true
	c.mixerSynced = false // mixerLoop sends the full state
	c.mu.Unlock()
	ru.logger.Info("ws subscribe mixer")

	// The param-channel values may be stale (nobody was watching) or absent
	// (first subscriber). Read them now, off this goroutine; the full state
	// goes out on the loop's next tick with what /schwung-ui already says, and
	// these fill in as they land.
	ru.mixerScheduleRefresh(ctx, -1, 0)
	ru.mix.kick()
}

func (ru *RemoteUI) handleUnsubscribeMixer(c *ruClient) {
	c.mu.Lock()
	c.mixerSub = false
	c.mixerSynced = false
	c.mu.Unlock()
	ru.logger.Info("ws unsubscribe mixer")
}

func (ru *RemoteUI) handleSetMixerParam(ctx context.Context, c *ruClient, msg wsMessage) {
	perSlot, ok := mixerWriteScope(msg.Key)
	if !ok {
		ru.sendError(ctx, c, "set_mixer_param: not a mixer key: "+msg.Key)
		return
	}
	if !validMixerValue(msg.Key, msg.Value) {
		ru.sendError(ctx, c, fmt.Sprintf("set_mixer_param: bad value %q for %s", msg.Value, msg.Key))
		return
	}
	var slot uint8
	if perSlot {
		if msg.Slot == nil || *msg.Slot >= mixerSlots {
			ru.sendError(ctx, c, "set_mixer_param: "+msg.Key+" needs a slot 0-"+strconv.Itoa(mixerSlots-1))
			return
		}
		slot = *msg.Slot
	}
	if !ru.requireShm(ctx, c) {
		return
	}
	// Through the web set ring (shadow_direct_set_param), like every other
	// Remote UI write. The device's answer comes back by itself: slot:* writes
	// land in /schwung-ui, everything else is echoed on the notify ring.
	if err := ru.setParam(slot, msg.Key, msg.Value); err != nil {
		ru.logger.Error("set_mixer_param failed", "slot", slot, "key", msg.Key, "err", err)
		ru.sendError(ctx, c, "set_mixer_param failed: "+err.Error())
	}
}

// mixerNotify takes one notify-ring change. Called for every change the ring
// carries; anything that is not a mixer value is ignored here.
func (ru *RemoteUI) mixerNotify(ctx context.Context, slot uint8, key, value string) {
	switch {
	case containsKey(mixerSlotParamKeys, key):
		if v, ok := normalizeMixerValue(key, value); ok && int(slot) < mixerSlots {
			if ru.mix.store.put(mixerStoreKey(int(slot), key), v) {
				ru.mix.kick()
			}
		}
	case containsKey(mixerGlobalKeys, key):
		if v, ok := normalizeMixerValue(key, value); ok && ru.mix.store.put(mixerStoreKey(-1, key), v) {
			ru.mix.kick()
		}
	case key == "set_aligned":
		// shadow_ui finished a set switch. Every slot's sends and the returns
		// were just restored from the new set, and none of that is notified.
		if len(ru.mixerClients()) > 0 {
			ru.mixerScheduleRefresh(ctx, -1, mixerSetSwitchDelay)
		}
	}
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

// mixerScheduleRefresh re-reads slot's param-channel values (or everything,
// for slot -1) after delay. A request that arrives while one is already
// pending JOINS it rather than pushing it out: the pending read has not
// started, so it will see whatever prompted the second request too. (Resetting
// the timer instead could re-arm one that had already fired and whose callback
// was waiting on refreshMu, reading everything twice.)
func (ru *RemoteUI) mixerScheduleRefresh(ctx context.Context, slot int, delay time.Duration) {
	ms := &ru.mix
	ms.refreshMu.Lock()
	defer ms.refreshMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if ms.refreshTimers == nil {
		ms.refreshTimers = make(map[int]*time.Timer)
	}
	if _, ok := ms.refreshTimers[slot]; ok {
		return
	}
	ms.inflight.Add(1)
	ms.refreshTimers[slot] = time.AfterFunc(delay, func() {
		defer ms.inflight.Done()
		ms.refreshMu.Lock()
		delete(ms.refreshTimers, slot)
		ms.refreshMu.Unlock()
		if ctx.Err() != nil {
			return
		}
		ru.mixerRead(ctx, mixerReadKeys(slot))
	})
}

// mixerRead reads keys one at a time over the param channel, yielding between
// them so shadow_ui's own reads interleave rather than queue behind a burst.
func (ru *RemoteUI) mixerRead(ctx context.Context, keys []mixerKeyRef) {
	shm := ru.ensureShm()
	if shm == nil {
		return
	}
	for i, k := range keys {
		if ctx.Err() != nil {
			return
		}
		slot := uint8(0)
		if k.slot >= 0 {
			slot = uint8(k.slot)
		}
		since := ru.mix.store.begin()
		raw, err := shm.GetParam(slot, k.key)
		if v, ok := normalizeMixerValue(k.key, raw); err == nil && ok {
			if ru.mix.store.putRead(k.storeKey(), v, since) {
				ru.mix.kick()
			}
		}
		if i < len(keys)-1 {
			time.Sleep(mixerReadYield)
		}
	}
}

// mixerBackstop reads ONE param-channel value, the next in round-robin order,
// with TryGetParam: if the channel is busy this second, it waits for the next.
// Fifteen keys at one a second is every value re-checked every 15 s, for a
// steady cost of one read a second.
func (ru *RemoteUI) mixerBackstop() {
	ms := &ru.mix
	if !ms.backstopBusy.CompareAndSwap(false, true) {
		return
	}
	keys := mixerReadKeys(-1)
	k := keys[ms.cursor%len(keys)]
	ms.cursor++
	ms.inflight.Add(1)
	go func() {
		defer ms.inflight.Done()
		defer ms.backstopBusy.Store(false)
		shm := ru.ensureShm()
		if shm == nil {
			return
		}
		slot := uint8(0)
		if k.slot >= 0 {
			slot = uint8(k.slot)
		}
		since := ms.store.begin()
		raw, served, err := shm.TryGetParam(slot, k.key)
		if v, ok := normalizeMixerValue(k.key, raw); served && err == nil && ok {
			if ms.store.putRead(k.storeKey(), v, since) {
				ms.kick()
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// The loop — the ONLY sender of mixer messages
// ---------------------------------------------------------------------------

func (ru *RemoteUI) mixerLoop(ctx context.Context) {
	ticker := time.NewTicker(mixerPollInterval)
	defer ticker.Stop()
	kick := ru.mix.kickChan()
	var lastBackstop time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-kick:
		}

		clients := ru.mixerClients()
		if len(clients) == 0 {
			continue // nobody is looking: never touch the param channel
		}

		live := ru.mixerPollUI(ctx)

		if now := time.Now(); now.Sub(lastBackstop) >= mixerBackstopEvery {
			lastBackstop = now
			ru.mixerBackstop()
		}

		ru.mixerFlush(ctx, clients, live)
	}
}

// mixerPollUI reads /schwung-ui into the store and reports whether it could.
// A slot whose patch name changed has just had a patch loaded, which brings its
// own saved send levels with it -- so its param-channel values are re-read.
func (ru *RemoteUI) mixerPollUI(ctx context.Context) bool {
	ui := ru.ensureUIState()
	slots, err := ui.Read()
	if err != nil {
		return false
	}
	ms := &ru.mix
	for i, s := range slots {
		if !s.Present {
			continue // a slot this shim does not publish stays unknown
		}
		nameChanged := ms.store.put(mixerStoreKey(i, mixerNameKey), s.Name)
		ms.store.put(mixerStoreKey(i, "slot:volume"), formatUIVolume(s.Volume))
		if s.Muted != nil {
			ms.store.put(mixerStoreKey(i, "slot:muted"), boolDigit(*s.Muted))
		}
		if s.Soloed != nil {
			ms.store.put(mixerStoreKey(i, "slot:soloed"), boolDigit(*s.Soloed))
		}
		if nameChanged && ms.uiSeen {
			ru.mixerScheduleRefresh(ctx, i, mixerSlotPatchDelay)
		}
	}
	ms.uiSeen = true
	return true
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// mixerFlush sends each subscribed client either the full state (if it has
// not had one, or missed a delta) or what changed since the last flush.
func (ru *RemoteUI) mixerFlush(ctx context.Context, clients []*ruClient, live bool) {
	ms := &ru.mix
	dirty := ms.store.takeDirty()
	liveChanged := live != ms.lastLive
	ms.lastLive = live

	var update *wsMixer
	if len(dirty) > 0 || liveChanged {
		u := mixerMessage("mixer_update", dirty)
		if liveChanged {
			l := live
			u.Live = &l
		}
		update = &u
	}

	var full *wsMixer
	for _, c := range clients {
		c.mu.Lock()
		synced := c.mixerSynced
		c.mu.Unlock()

		if !synced {
			if full == nil {
				f := mixerMessage("mixer_state", ms.store.snapshot())
				l := live
				f.Live = &l
				full = &f
			}
			if ru.writeJSONTry(ctx, c, full) {
				c.mu.Lock()
				c.mixerSynced = true
				c.mu.Unlock()
			}
			continue
		}
		if update != nil && !ru.writeJSONTry(ctx, c, update) {
			// Its previous write is still in flight. A dropped delta is a lost
			// mute or a stuck fader, so it gets the whole state next tick.
			c.mu.Lock()
			c.mixerSynced = false
			c.mu.Unlock()
		}
	}
}

package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// Renaming a slot from the Remote UI.
//
// A slot's name is its preset's name, so a rename rewrites the preset file
// (exactly what Save under an edited name does on Move). Only shadow_ui can do
// that -- it alone can assemble the slot's whole patch -- so the request is
// relayed: the browser sends rename_slot, this writes "<seq>:<name>" to
// `slot:rename_request` (the shim holds it and raises SHADOW_UI_FLAG_SLOT_RENAME),
// shadow_ui does the rename and writes `slot:rename_status` "<seq>:ok" or
// "<seq>:err:<reason>", and that write comes back here on the notify ring and
// is answered to the browser that asked as rename_result. The new name itself
// arrives on its own, as `slot:name` (Slot tabs) and in /schwung-ui (Mixer).
// ---------------------------------------------------------------------------

const slotRenameMaxLen = 63 // SHADOW_UI_NAME_LEN - 1

// slotRenameTimeout: shadow_ui answers on its next tick, so this only ends a
// request nobody is going to answer. A var so a test need not wait it out.
var slotRenameTimeout = 8 * time.Second

type wsRenameResult struct {
	Type  string `json:"type"` // "rename_result"
	Slot  uint8  `json:"slot"`
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type slotRenamePending struct {
	client *ruClient
	slot   uint8
	name   string
	timer  *time.Timer
}

type slotRenames struct {
	seq     atomic.Int64
	mu      sync.Mutex
	pending map[int64]*slotRenamePending
}

// validSlotName says why a name cannot be a preset name, or "" if it can. The
// same rules shadow_ui applies; checked here so the browser hears at once.
func validSlotName(name string) string {
	switch {
	case name == "":
		return "The name is empty."
	case !utf8.ValidString(name):
		return "The name is not valid text."
	case utf8.RuneCountInString(name) > slotRenameMaxLen || len(name) > slotRenameMaxLen:
		return fmt.Sprintf("The name is too long (%d characters at most).", slotRenameMaxLen)
	case name == "Untitled":
		return `"Untitled" is the name of an unsaved slot.`
	}
	for _, r := range name {
		if r == '"' || r == '\\' || unicode.IsControl(r) {
			return "The name can't contain quotes, backslashes or control characters."
		}
	}
	return ""
}

func (ru *RemoteUI) handleRenameSlot(ctx context.Context, c *ruClient, msg wsMessage) {
	name := strings.TrimSpace(msg.Value)
	reply := func(errText string) {
		slot := uint8(0)
		if msg.Slot != nil {
			slot = *msg.Slot
		}
		ru.writeJSONAsync(ctx, c, wsRenameResult{Type: "rename_result", Slot: slot, Name: name, Error: errText})
	}
	if msg.Slot == nil || *msg.Slot >= remoteUISlots {
		reply("rename_slot needs a slot 0-" + strconv.Itoa(remoteUISlots-1) + ".")
		return
	}
	if why := validSlotName(name); why != "" {
		reply(why)
		return
	}
	if !ru.requireShm(ctx, c) {
		return
	}
	slot := *msg.Slot
	rn := &ru.renames
	seq := rn.seq.Add(1)
	p := &slotRenamePending{client: c, slot: slot, name: name}
	rn.mu.Lock()
	if rn.pending == nil {
		rn.pending = make(map[int64]*slotRenamePending)
	}
	rn.pending[seq] = p
	p.timer = time.AfterFunc(slotRenameTimeout, func() {
		if q := rn.take(seq); q != nil {
			ru.writeJSONAsync(ctx, q.client, wsRenameResult{Type: "rename_result", Slot: q.slot, Name: q.name,
				Error: "Move didn't answer. Is Schwung's UI running on the device?"})
		}
	})
	rn.mu.Unlock()

	ru.logger.Info("ws rename_slot", "slot", slot, "name", name, "seq", seq)
	if err := ru.sendRenameRequest(slot, strconv.FormatInt(seq, 10)+":"+name); err != nil {
		if q := rn.take(seq); q != nil {
			q.timer.Stop()
			reply("Could not reach Move: " + err.Error())
		}
	}
}

// sendRenameRequest writes the request through the web set ring, or -- with no
// ring -- the BLOCKING mailbox write. Not ru.setParam: its fallback is the
// 10 ms fire-and-forget write meant for knob drags, where a lost write is
// overtaken by the next one. A rename is one gesture; a busy channel at that
// instant must wait for it, not fail.
func (ru *RemoteUI) sendRenameRequest(slot uint8, value string) error {
	if ring := ru.ensureSetRing(); ring != nil && len(value) < webValueLen {
		if err := ring.SetParam(slot, "slot:rename_request", value); err == nil {
			return nil
		}
	}
	if shm := ru.ensureShm(); shm != nil {
		return shm.SetParam(slot, "slot:rename_request", value)
	}
	return fmt.Errorf("no shared memory available")
}

// take removes and returns a pending request, or nil if it already ended.
func (rn *slotRenames) take(seq int64) *slotRenamePending {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	p := rn.pending[seq]
	delete(rn.pending, seq)
	return p
}

// parseRenameStatus splits "<seq>:ok" / "<seq>:err:<reason>".
func parseRenameStatus(v string) (seq int64, ok bool, reason string, valid bool) {
	head, rest, found := strings.Cut(v, ":")
	if !found {
		return 0, false, "", false
	}
	n, err := strconv.ParseInt(head, 10, 64)
	if err != nil {
		return 0, false, "", false
	}
	if rest == "ok" {
		return n, true, "", true
	}
	if r, isErr := strings.CutPrefix(rest, "err:"); isErr {
		if r == "" {
			r = "Move refused the rename."
		}
		return n, false, r, true
	}
	return 0, false, "", false
}

// renameNotify takes one notify-ring change; true if it was a rename answer
// (which is for the browser that asked, not for every subscriber).
func (ru *RemoteUI) renameNotify(ctx context.Context, key, value string) bool {
	if key == "slot:rename_request" {
		return true // our own request echoed back: nothing to show
	}
	if key != "slot:rename_status" {
		return false
	}
	seq, ok, reason, valid := parseRenameStatus(value)
	if !valid {
		return true
	}
	p := ru.renames.take(seq)
	if p == nil {
		return true // timed out already, or another manager's
	}
	p.timer.Stop()
	ru.writeJSONAsync(ctx, p.client, wsRenameResult{Type: "rename_result", Slot: p.slot, Name: p.name,
		OK: ok, Error: reason})
	return true
}

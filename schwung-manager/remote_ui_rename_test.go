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
	"nhooyr.io/websocket/wsjson"
)

func TestValidSlotName(t *testing.T) {
	good := []string{"Pad", "OB-Xd + FV", "Bass 2", strings.Repeat("a", slotRenameMaxLen), "Café"}
	for _, n := range good {
		if why := validSlotName(n); why != "" {
			t.Errorf("%q refused: %s", n, why)
		}
	}
	bad := []string{"", strings.Repeat("a", slotRenameMaxLen+1), `say "hi"`, `a\b`, "tab\there",
		"Untitled", string([]byte{0xff, 0xfe})}
	for _, n := range bad {
		if validSlotName(n) == "" {
			t.Errorf("%q accepted", n)
		}
	}
}

func TestParseRenameStatus(t *testing.T) {
	cases := []struct {
		in     string
		seq    int64
		ok     bool
		reason string
		valid  bool
	}{
		{"7:ok", 7, true, "", true},
		{"12:err:A preset named \"X\" already exists.", 12, false, "A preset named \"X\" already exists.", true},
		{"3:err:", 3, false, "Move refused the rename.", true},
		{"3:maybe", 0, false, "", false},
		{"x:ok", 0, false, "", false},
		{"ok", 0, false, "", false},
	}
	for _, c := range cases {
		seq, ok, reason, valid := parseRenameStatus(c.in)
		if seq != c.seq || ok != c.ok || reason != c.reason || valid != c.valid {
			t.Errorf("%q: got %d %v %q %v", c.in, seq, ok, reason, valid)
		}
	}
}

// renameHarness: a RemoteUI over a real WebSocket, with a FILE standing in for
// the param channel so the request the manager writes can be read back.
type renameHarness struct {
	t    *testing.T
	ru   *RemoteUI
	conn *websocket.Conn
	ctx  context.Context

	mu       sync.Mutex
	lastSlot byte
	lastKey  string
	lastVal  string
}

// fakeShim answers the param channel the way the shim does: take the request,
// record it, publish response_id + response_ready and clear request_type in
// one word. Enough for SetParam's blocking wait to complete.
func (h *renameHarness) fakeShim(ctx context.Context, path string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		h.t.Error(err)
		return
	}
	defer f.Close()
	b, err := syscall.Mmap(int(f.Fd()), 0, shmParamSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		h.t.Error(err)
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
		if t := byte(w); t == 1 || t == 2 {
			h.mu.Lock()
			if t == 1 {
				h.lastSlot, h.lastKey = b[paramOffSlot], cut(b[paramOffKey:paramOffKey+paramKeyLen])
				h.lastVal = cut(b[paramOffValue : paramOffValue+256])
			}
			h.mu.Unlock()
			id := binary.LittleEndian.Uint32(b[paramOffRequestID:])
			binary.LittleEndian.PutUint32(b[paramOffResponseID:], id)
			// request_type 0, response_ready 1, in one word, as the shim publishes.
			atomic.StoreUint32(word, (w&^uint32(0xFF))|0x00010000)
		}
		time.Sleep(200 * time.Microsecond)
	}
}

func newRenameHarness(t *testing.T) *renameHarness {
	t.Helper()
	dir := t.TempDir()
	origParam := shmParamPath
	shmParamPath = filepath.Join(dir, "schwung-param")
	if err := os.WriteFile(shmParamPath, make([]byte, shmParamSize), 0o644); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ru := NewRemoteUI(&App{logger: logger}, nil, dir, logger)
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(ru)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &renameHarness{t: t, ru: ru, conn: conn, ctx: ctx}
	shimDone := make(chan struct{})
	go func() { h.fakeShim(ctx, shmParamPath); close(shimDone) }()
	t.Cleanup(func() {
		conn.Close(websocket.StatusNormalClosure, "")
		srv.Close()
		cancel()
		<-shimDone
		shmParamPath = origParam
	})
	return h
}

func (h *renameHarness) result() map[string]any {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(h.ctx, 3*time.Second)
	defer cancel()
	for {
		var m map[string]any
		if err := wsjson.Read(ctx, h.conn, &m); err != nil {
			h.t.Fatalf("waiting for rename_result: %v", err)
		}
		if m["type"] == "rename_result" {
			return m
		}
	}
}

// lastRequest is the most recent SET the fake shim served.
func (h *renameHarness) lastRequest() (slot byte, key, value string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastSlot, h.lastKey, h.lastVal
}

func TestRenameSlotRoundTrip(t *testing.T) {
	h := newRenameHarness(t)
	slot := uint8(5) // an aux slot: renames reach all eight
	if err := wsjson.Write(h.ctx, h.conn, map[string]any{"type": "rename_slot", "slot": slot, "value": "  Choir Pad "}); err != nil {
		t.Fatal(err)
	}
	// The request must reach the channel as "<seq>:<trimmed name>" on that slot.
	var key, value string
	var got byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, key, value = h.lastRequest(); key != "" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if key != "slot:rename_request" || got != slot || !strings.HasSuffix(value, ":Choir Pad") {
		t.Fatalf("request on the wire: slot %d %q=%q", got, key, value)
	}
	seq := strings.SplitN(value, ":", 2)[0]

	// A status for some OTHER request is not this one's answer.
	h.ru.renameNotify(h.ctx, "slot:rename_status", "999:ok")
	// shadow_ui's answer arrives on the notify ring.
	if !h.ru.renameNotify(h.ctx, "slot:rename_status", seq+":ok") {
		t.Fatal("renameNotify must claim a rename status")
	}
	m := h.result()
	if m["ok"] != true || m["name"] != "Choir Pad" || m["slot"] != float64(slot) {
		t.Fatalf("result: %v", m)
	}

	// A refusal is reported with the Move's reason.
	if err := wsjson.Write(h.ctx, h.conn, map[string]any{"type": "rename_slot", "slot": 0, "value": "Bass"}); err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline.Add(2 * time.Second)) {
		if _, _, value = h.lastRequest(); strings.HasSuffix(value, ":Bass") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	seq = strings.SplitN(value, ":", 2)[0]
	h.ru.renameNotify(h.ctx, "slot:rename_status", seq+`:err:A preset named "Bass" already exists.`)
	m = h.result()
	if m["ok"] == true || !strings.Contains(m["error"].(string), "already exists") {
		t.Fatalf("refusal: %v", m)
	}
}

func TestRenameSlotRefusedBeforeTheWire(t *testing.T) {
	h := newRenameHarness(t)
	for _, msg := range []map[string]any{
		{"type": "rename_slot", "slot": 0, "value": ""},
		{"type": "rename_slot", "slot": 0, "value": `bad "quote"`},
		{"type": "rename_slot", "slot": 8, "value": "Fine"},
		{"type": "rename_slot", "value": "No slot"},
	} {
		if err := wsjson.Write(h.ctx, h.conn, msg); err != nil {
			t.Fatal(err)
		}
		if m := h.result(); m["ok"] == true || m["error"] == "" {
			t.Errorf("%v: want a refusal, got %v", msg, m)
		}
	}
	if _, key, _ := h.lastRequest(); key != "" {
		t.Errorf("a refused rename must write nothing, wrote %q", key)
	}
}

// Nobody answers (shadow_ui not running): the browser is told, not left waiting.
func TestRenameSlotTimesOut(t *testing.T) {
	orig := slotRenameTimeout
	slotRenameTimeout = 50 * time.Millisecond
	t.Cleanup(func() { slotRenameTimeout = orig })
	h := newRenameHarness(t)
	if err := wsjson.Write(h.ctx, h.conn, map[string]any{"type": "rename_slot", "slot": 1, "value": "Keys"}); err != nil {
		t.Fatal(err)
	}
	m := h.result()
	if m["ok"] == true || !strings.Contains(m["error"].(string), "didn't answer") {
		t.Fatalf("timeout: %v", m)
	}
	// A late answer after the timeout is ignored, not sent as a second result.
	_, _, value := h.lastRequest()
	h.ru.renameNotify(h.ctx, "slot:rename_status", strings.SplitN(value, ":", 2)[0]+":ok")
	ctx, cancel := context.WithTimeout(h.ctx, 150*time.Millisecond)
	defer cancel()
	var extra map[string]any
	if err := wsjson.Read(ctx, h.conn, &extra); err == nil && extra["type"] == "rename_result" {
		t.Fatalf("a late answer produced a second result: %v", extra)
	}
}

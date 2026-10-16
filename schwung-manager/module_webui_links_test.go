// The module page's web UI links. A web_ui.html opened with no query string
// drives Track 1's synth (schwung-remote-api.js defaults slot 0 / "synth"), so
// every link must name the place it drives -- and a read that did not complete
// must not be mistaken for "loaded nowhere".

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testWebUIBase = "/api/remote-ui/module-assets/jvx/web_ui.html"

// fakeParams answers from a map; a key absent from it is an ANSWERED error,
// as the shim replies for a position that serves nothing.
type fakeParams struct {
	vals  map[string]string // "slot/key" -> value
	fail  string            // "slot/key" that times out
	reads []string
}

func (f *fakeParams) get(slot uint8, key string) (string, error) {
	k := fmt.Sprintf("%d/%s", slot, key)
	f.reads = append(f.reads, k)
	if k == f.fail {
		return "", errors.New("param channel busy (timeout waiting for idle)")
	}
	if v, ok := f.vals[k]; ok {
		return v, nil
	}
	return "", errors.New("param get error: not served")
}

// chainMods is the chain's "chain_modules" answer for one slot.
func chainMods(synth string, midi, fx []string) string {
	q := func(xs []string) string {
		out := make([]string, len(xs))
		for i, x := range xs {
			out[i] = `"` + x + `"`
		}
		return "[" + strings.Join(out, ",") + "]"
	}
	return `{"synth":"` + synth + `","midi_fx":` + q(midi) + `,"fx":` + q(fx) + `}`
}

// allSlots answers chain_modules for every slot: an empty chain unless given.
func allSlots(given map[uint8]string) map[string]string {
	vals := map[string]string{}
	for slot := uint8(0); slot < webUIChainSlots; slot++ {
		v, ok := given[slot]
		if !ok {
			v = chainMods("", nil, nil)
		}
		vals[fmt.Sprintf("%d/chain_modules", slot)] = v
	}
	return vals
}

func TestWebUILinksNameTheSlotTheSynthIsIn(t *testing.T) {
	f := &fakeParams{vals: allSlots(map[uint8]string{
		0: chainMods("obxd", nil, nil), // Track 1 holds a DIFFERENT synth
		1: chainMods("jvx", nil, nil),
	})}
	got := findModuleWebUILinks(f.get, "jvx", "sound_generator", testWebUIBase)

	if got.Unknown || !got.Present {
		t.Fatalf("got %+v, want a known answer", got)
	}
	if len(got.Links) != 1 {
		t.Fatalf("got %d links, want 1 (Track 2): %+v", len(got.Links), got.Links)
	}
	want := testWebUIBase + "?component=synth&schwungStandalone=1&slot=1"
	if got.Links[0].URL != want {
		t.Errorf("URL = %q, want %q", got.Links[0].URL, want)
	}
	if got.Links[0].Where != "Track 2 Synth" {
		t.Errorf("Where = %q", got.Links[0].Where)
	}
	// ONE read per slot, all eight, whatever the number of positions; a synth
	// needs no master or tool reads.
	if len(f.reads) != webUIChainSlots {
		t.Errorf("made %d reads for a sound generator, want %d: %v",
			len(f.reads), webUIChainSlots, f.reads)
	}
}

func TestWebUILinksCoverChainFXAndMasterFX(t *testing.T) {
	vals := allSlots(map[uint8]string{
		2: chainMods("obxd", nil, []string{"tape", "echo"}),
		// An aux slot, in a position the Remote UI used not to know.
		6: chainMods("", []string{"arp"}, []string{"a", "b", "c", "d", "e", "f", "g", "echo"}),
	})
	// Master FX answers a .so PATH, not an id (masterFxModuleID).
	vals["0/master_fx:modules"] = `[{"id":"","path":""},{"id":"","path":""},` +
		`{"id":"","path":"/data/UserData/schwung/modules/audio_fx/echo/echo.so"},` +
		`{"id":"","path":""},{"id":"","path":""},{"id":"","path":""},{"id":"","path":""},` +
		`{"id":"echo","path":"/x/echo/echo.so"}]`
	// And the send buses, which are the same shape under their own prefix.
	vals["0/send1:modules"] = `[]`
	vals["0/send2:modules"] = `[{"id":"","path":""},{"id":"echo","path":"/x/echo/echo.so"}]`
	f := &fakeParams{vals: vals}
	got := findModuleWebUILinks(f.get, "echo", "audio_fx", testWebUIBase)

	want := []WebUILink{
		{Where: "Track 3 Audio FX 2", URL: testWebUIBase + "?component=fx2&schwungStandalone=1&slot=2"},
		{Where: "Slot 7 Audio FX 8", URL: testWebUIBase + "?component=fx8&schwungStandalone=1&slot=6"},
		{Where: "Master FX 3", URL: testWebUIBase + "?component=master_fx%3Afx3&schwungStandalone=1&master_fx=1"},
		{Where: "Master FX 8", URL: testWebUIBase + "?component=master_fx%3Afx8&schwungStandalone=1&master_fx=1"},
		{Where: "Send B FX 2", URL: testWebUIBase + "?component=send2%3Afx2&schwungStandalone=1&bus=send2"},
	}
	if len(got.Links) != len(want) {
		t.Fatalf("got %+v, want %+v", got.Links, want)
	}
	for i := range want {
		if got.Links[i] != want[i] {
			t.Errorf("link %d = %+v, want %+v", i, got.Links[i], want[i])
		}
	}
	if len(f.reads) != webUIChainSlots+len(fxBuses) {
		t.Errorf("want one read per slot + one per FX bus, made %d: %v", len(f.reads), f.reads)
	}
}

// An older chain serves no chain_modules: the positions are read one by one,
// and still found.
func TestWebUILinksOlderChainFallsBackPerPosition(t *testing.T) {
	f := &fakeParams{vals: map[string]string{
		"2/synth_module":  "obxd",
		"2/fx_count":      "3",
		"2/fx3_module":    "echo",
		"2/midi_fx_count": "0",
	}}
	got := findModuleWebUILinks(f.get, "echo", "audio_fx", testWebUIBase)
	if got.Unknown || len(got.Links) != 1 || got.Links[0].Where != "Track 3 Audio FX 3" {
		t.Fatalf("got %+v, want Track 3 Audio FX 3", got)
	}
}

func TestWebUILinksFindARunningTool(t *testing.T) {
	f := &fakeParams{vals: map[string]string{"0/overtake_dsp:module_id": "davebox"}}
	got := findModuleWebUILinks(f.get, "davebox", "tool", testWebUIBase)
	if len(got.Links) != 1 || got.Links[0].URL != testWebUIBase+"?schwungStandalone=1&tool=1" {
		t.Fatalf("got %+v, want one tool-channel link", got.Links)
	}
	if len(f.reads) != 1 {
		t.Errorf("a tool needs one read, made %v", f.reads)
	}
}

// Loaded nowhere is a real answer: present, no links, not unknown.
func TestWebUILinksLoadedNowhere(t *testing.T) {
	f := &fakeParams{vals: allSlots(map[uint8]string{0: chainMods("obxd", nil, nil)})}
	got := findModuleWebUILinks(f.get, "jvx", "sound_generator", testWebUIBase)
	if got.Unknown || !got.Present || len(got.Links) != 0 {
		t.Fatalf("got %+v, want present, known, no links", got)
	}
}

// A timed-out read withholds EVERYTHING -- including a placement already
// found -- because a partial list reads as the whole answer.
func TestWebUILinksTimeoutIsUnknownNotNowhere(t *testing.T) {
	f := &fakeParams{
		vals: allSlots(map[uint8]string{0: chainMods("jvx", nil, nil)}),
		fail: "2/chain_modules",
	}
	got := findModuleWebUILinks(f.get, "jvx", "sound_generator", testWebUIBase)
	if !got.Unknown || len(got.Links) != 0 {
		t.Fatalf("got %+v, want Unknown with no links", got)
	}
	if got := findModuleWebUILinks(nil, "jvx", "sound_generator", testWebUIBase); !got.Unknown {
		t.Fatalf("no param channel gave %+v, want Unknown", got)
	}
}

func writeWebUIModule(t *testing.T, base, subdir, id, componentType string, withDSP bool) {
	t.Helper()
	dir := filepath.Join(base, "modules", subdir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"module.json": `{"id":"` + id + `","name":"` + id + `","version":"0.1.0",` +
			`"component_type":"` + componentType + `"}`,
		"web_ui.html": "<!doctype html>",
	}
	if withDSP {
		files["dsp.so"] = ""
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func renderModulePage(t *testing.T, base, id string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(catalogHost140))
	}))
	defer srv.Close()
	app := newModulesTestApp(t, srv.URL, base, "1.4.0")
	req := httptest.NewRequest(http.MethodGet, "/modules/"+id, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	app.handleModuleDetail(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// The defect #512 shipped, rendered: a synth's page off the device (no param
// channel) must NOT link its web UI bare, and must say why there is no button.
func TestModulePageNeverLinksAWebUIBare(t *testing.T) {
	if OpenShmParams() != nil {
		t.Skip("a live param channel is attached; this pins the no-channel page")
	}
	base := t.TempDir()
	writeWebUIModule(t, base, "sound_generators", "jvx", "sound_generator", true)
	body := renderModulePage(t, base, "jvx")

	if strings.Contains(body, `web_ui.html"`) {
		t.Error("page links web_ui.html with no query: that page drives Track 1's synth, not this module")
	}
	if strings.Contains(body, "Open web UI") {
		t.Error("page offers a web UI button without knowing where the module is loaded")
	}
	if !strings.Contains(body, "did not answer") {
		t.Error("page is silent about the web UI when the placement read failed")
	}
}

// The case #512 was written for: an overtake module with no DSP has no place
// to find, and gets its link on the tool channel.
func TestModulePageLinksADSPLessOvertakeModule(t *testing.T) {
	base := t.TempDir()
	writeWebUIModule(t, base, "tools", "m8x", "overtake", false)
	body := renderModulePage(t, base, "m8x")

	want := `href="/api/remote-ui/module-assets/m8x/web_ui.html?schwungStandalone=1&amp;tool=1"`
	if !strings.Contains(body, want) {
		t.Errorf("DSP-less overtake page lacks its tool-channel link %s", want)
	}
}

#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/../.."

# THE AUX SLOTS: eight chain slots, of which only the first four are Move's
# tracks (shadow_constants.h, "EIGHT CHAIN SLOTS").
#
# Two kinds of claim, and both fail silently when broken:
#
#   1. The slot count is written down in C, the shadow UI, the CC map, the web
#      manager (Go and its two front-end scripts and template) and the stem
#      table. A copy left at 4 does not error: that surface simply never
#      shows slots 5-8. So every copy is derived from source and compared --
#      the number is never restated here.
#
#   2. The paths that reach for a MOVE TRACK must stop at four. The Link Audio
#      read is the dangerous one: Move's channel count includes Main, so a read
#      bounded by the slot count would sum Move's whole master mix into aux
#      slot 5. The lane clock must tell an aux slot it has no track at all.
#
# The runtime half (shared-memory layout, the state file, set loads) is
# tests/host/test_aux_slots.c.

node - <<'NODE'
const fs = require("fs");
const read = (p) => fs.readFileSync(p, "utf8");
const fails = [];
const check = (ok, msg) => { if (!ok) fails.push(msg); };
const num = (src, re, what) => {
    const m = src.match(re);
    if (!m) { fails.push("could not find " + what); return NaN; }
    return parseInt(m[1], 10);
};

const CONST = read("src/host/shadow_constants.h");
const SHIM = read("src/schwung_shim.c");
const UIJS = read("src/shadow/shadow_ui.js");
const SAMPLER_C = read("src/host/shadow_sampler.c");
const PERF_H = read("src/host/perf_snapshot.h");
const LA_H = read("src/host/link_audio.h");
const MM_H = read("src/host/move_model.h");
const CTRL = read("src/shared/control_target.mjs");
const GO_CTRL = read("schwung-manager/controls.go");
const GO_RU = read("schwung-manager/remote_ui.go");
const GO_PERF = read("schwung-manager/perf_shm.go");
const RU_JS = read("schwung-manager/static/remote-ui.js");
const CTRL_JS = read("schwung-manager/static/controls.js");
const RU_HTML = read("schwung-manager/templates/remote_ui.html");

/* ---- 1. one slot count, everywhere it is written down ------------------ */

const slots = num(CONST, /^#define SHADOW_CHAIN_INSTANCES\s+(\d+)/m, "SHADOW_CHAIN_INSTANCES");
const moveSlots = num(CONST, /^#define SHADOW_MOVE_SLOTS\s+(\d+)/m, "SHADOW_MOVE_SLOTS");
check(slots > moveSlots, "there are no aux slots: SHADOW_CHAIN_INSTANCES " + slots +
      " is not above SHADOW_MOVE_SLOTS " + moveSlots);
check(slots <= 9, "scene targets name a slot with ONE digit (slot1..slot9)");

const copies = [
    ["shadow_ui.js SHADOW_UI_SLOTS", num(UIJS, /^const SHADOW_UI_SLOTS = (\d+);/m, "the UI slot count")],
    ["control_target.mjs SLOTS", num(CTRL, /^export const SLOTS = (\d+);/m, "the CC map slot count")],
    ["perf_snapshot.h PERF_CHAIN_SLOTS", num(PERF_H, /^#define PERF_CHAIN_SLOTS\s+(\d+)/m, "PERF_CHAIN_SLOTS")],
    ["controls.go controlsSlots", num(GO_CTRL, /controlsSlots\s+=\s+(\d+)/, "controlsSlots")],
    ["remote_ui.go remoteUISlots", num(GO_RU, /const remoteUISlots = (\d+)/, "remoteUISlots")],
    ["perf_shm.go perfChainSlots", num(GO_PERF, /perfChainSlots\s+=\s+(\d+)/, "perfChainSlots")],
    ["remote-ui.js SLOT_COUNT", num(RU_JS, /var SLOT_COUNT = (\d+);/, "the Remote UI slot count")],
    ["controls.js CHAIN_SLOTS", num(CTRL_JS, /const CHAIN_SLOTS = (\d+);/, "the controls page slot count")],
    ["remote_ui.html slot tabs",
        [...RU_HTML.matchAll(/data-slot="(\d+)"/g)].length],
];
const stemNames = (SAMPLER_C.match(/sampler_stem_names\[SAMPLER_STEM_COUNT\]\s*=\s*\{([^}]*)\}/) || [, ""])[1];
copies.push(["sampler_stem_names Slot<N>", (stemNames.match(/"Slot\d+"/g) || []).length]);
for (const [what, n] of copies) {
    check(n === slots, what + " is " + n + " but SHADOW_CHAIN_INSTANCES is " + slots +
          " -- that surface would not reach every slot");
}

/* Move's side: four tracks, everywhere that says so. */
for (const [what, n] of [
    ["shadow_ui.js SHADOW_MOVE_SLOTS", num(UIJS, /^const SHADOW_MOVE_SLOTS = (\d+);/m, "the UI Move slot count")],
    ["link_audio.h LINK_AUDIO_SHADOW_CHANNELS", num(LA_H, /^#define LINK_AUDIO_SHADOW_CHANNELS\s+(\d+)/m, "LINK_AUDIO_SHADOW_CHANNELS")],
    ["move_model.h MM_TRACKS", num(MM_H, /^#define MM_TRACKS\s+(\d+)/m, "MM_TRACKS")],
]) {
    check(n === moveSlots, what + " is " + n + " but SHADOW_MOVE_SLOTS is " + moveSlots);
}

/* No per-slot array in the UI is a four-element literal any more: slot 5's
 * entry would read undefined. */
const literals = UIJS.match(/^\s*(?:let|const)\s+\w+\s*=\s*\[(null|false|""), \1, \1, \1\];/gm) || [];
check(literals.length === 0, "shadow_ui.js still sizes per-slot state as four literals: " +
      literals.map((s) => s.trim()).join(" | "));

/* ---- 2. what reaches for a Move track stops at four -------------------- */

check(/for \(int s = 0; s < SHADOW_MOVE_SLOTS && s < la_channel_count; s\+\+\)/.test(SHIM),
      "the Link Audio read is not bounded by SHADOW_MOVE_SLOTS");
check(!/s < SHADOW_CHAIN_INSTANCES && s < la_channel_count/.test(SHIM),
      "a Link Audio read is bounded by the SLOT count -- Move's channel count includes " +
      "Main, so aux slot 5 would receive Move's whole master mix");
check(/shadow_slot_is_move_track\(s\) \? s : -1,/.test(SHIM),
      "the lane clock no longer tells an aux slot it has no track (-1)");
/* Under Move->Schwung the stem tap must not sit inside the publisher guard,
 * or an aux slot is in the master file and in no stem. */
{
    const at = SHIM.indexOf("/* Capture for Link Audio publisher -- Move's tracks only */");
    const tap = SHIM.lastIndexOf("shadow_stem_store_slot(s, fx_buf,", at);
    check(at > 0 && tap > 0 && at - tap < 400,
          "the Move->Schwung stem tap is not just ahead of the Move-only publisher guard");
}
for (const m of SHIM.matchAll(/shadow_pub_audio_shm->slots\[s\]/g)) {
    const before = SHIM.slice(Math.max(0, m.index - 1600), m.index);
    check(/s < LINK_AUDIO_SHADOW_CHANNELS/.test(before),
          "a Link Audio PUBLISH indexed by slot is not guarded to Move's channels (offset " + m.index + ")");
}

/* ---- 3. the UI: reaching an aux slot, and loading a set ---------------- */

/* Lift a top-level function by name, up to the next line that is a bare "}". */
const lift = (name) => {
    const m = UIJS.match(new RegExp("^function " + name + "\\([^]*?^}", "m"));
    if (!m) { fails.push("could not lift " + name + "() from shadow_ui.js"); return ""; }
    return m[0];
};
const constLine = (name) => (UIJS.match(new RegExp("^const " + name + " = [^;]+;", "m")) || [""])[0];

/* The Track-tap flip. */
{
    const make = new Function("VIEWS", "env", [
        constLine("SHADOW_MOVE_SLOTS"),
        constLine("TRACK_TAP_ON_SCREEN_TICKS"),
        "let view, selectedSlot;",
        lift("trackTapTarget"),
        "return (slot, onScreen, v, sel) => { view = v; selectedSlot = sel; return trackTapTarget(slot, onScreen); };",
    ].join("\n"));
    const VIEWS = { CHAIN_EDIT: "chainedit", PARAM_PAGES: "parampages", SLOTS: "slots" };
    const tap = make(VIEWS);
    check(tap(1, 50, VIEWS.CHAIN_EDIT, 1) === 1 + moveSlots,
          "a second Track 2 tap on slot 2 chain editor must flip to its aux slot");
    check(tap(1, 50, VIEWS.CHAIN_EDIT, 1 + moveSlots) === 1,
          "a Track 2 tap from its aux slot must flip back to slot 2");
    check(tap(1, 50, VIEWS.CHAIN_EDIT, 0) === 1,
          "a Track 2 tap from slot 1 must land on slot 2, as it always has");
    check(tap(1, 50, VIEWS.PARAM_PAGES, 1) === 1,
          "a Track tap from a deeper page of the same slot must return to that slot, not flip");
    check(tap(1, 0, VIEWS.CHAIN_EDIT, 1) === 1,
          "the gesture that OPENS the UI from Move must land on the track slot, not flip");
    check(tap(5, 50, VIEWS.CHAIN_EDIT, 5) === 5, "an aux slot jump is taken as it is");
}

/* Slot lists are always full length. */
{
    const pad = new Function([
        constLine("SHADOW_UI_SLOTS"),
        "const DEFAULT_SLOTS = Array.from({ length: SHADOW_UI_SLOTS }, (_, i) => ({ channel: i + 1, name: \"\" }));",
        lift("padSlotList"),
        "return padSlotList;",
    ].join("\n"))();
    const four = pad([{ channel: 1, name: "a" }, { channel: 2, name: "b" }, { channel: 3, name: "c" }, { channel: 4, name: "d" }]);
    check(four.length === slots && four[3].name === "d" && four[slots - 1].channel === slots,
          "a four-entry slot list must be padded with the aux defaults");
    check(pad(null).length === slots, "no list at all must still give every slot");
}

/* A set saved before the aux slots resets them; Move owns only its four. */
{
    const run = (json, ownsMix) => {
        const writes = [];
        const f = new Function("host_read_file", "moveModelOwnsMix", "setSlotParamWithTimeout", "debugLog", [
            constLine("SHADOW_UI_SLOTS"),
            constLine("SHADOW_MOVE_SLOTS"),
            (UIJS.match(/^function isAuxSlot\(slot\) \{.*\}$/m) || [""])[0],
            lift("loadChainConfigFromDir"),
            "return loadChainConfigFromDir;",
        ].join("\n"))(() => json, () => ownsMix, (slot, key, value) => { writes.push([slot, key, value]); return true; }, () => {});
        f("/set");
        return writes;
    };
    const oldSet = JSON.stringify({ slots: [0, 1, 2, 3].map((i) => ({ channel: i + 1, volume: 0.5, muted: 1, soloed: 0 })) });
    const w = run(oldSet, true);
    const at = (slot, key) => (w.find((x) => x[0] === slot && x[1] === key) || [])[2];
    check(at(5, "slot:soloed") === "0" && at(5, "slot:muted") === "0" && at(5, "slot:volume") === "1" &&
          at(5, "slot:receive_channel") === "6",
          "an aux slot missing from an old set file must be written its defaults, got " +
          JSON.stringify(w.filter((x) => x[0] === 5)));
    check(at(1, "slot:muted") === undefined,
          "with the live model owning the mix, Move's four must not take mute from the file");
    const newSet = JSON.stringify({ slots: Array.from({ length: slots }, (_, i) => ({ channel: i + 1, volume: 1, muted: 0, soloed: i === 6 ? 1 : 0 })) });
    const w2 = run(newSet, true);
    check((w2.find((x) => x[0] === 6 && x[1] === "slot:soloed") || [])[2] === "1",
          "an aux slot takes its saved solo even when the live model owns Move's mix");
}

if (fails.length) {
    for (const f of fails) console.error("FAIL: " + f);
    process.exit(1);
}
console.log("PASS: aux slots -- " + slots + " chain slots on every surface, " + moveSlots +
            " Move tracks on every path that needs one, the Track-tap flip, full-length slot " +
            "lists and aux defaults on an old set");
NODE

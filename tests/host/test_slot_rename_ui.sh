#!/usr/bin/env bash
#
# Renaming a slot from the Remote UI -- shadow_ui's half, RUN, not grepped.
#
# A slot's name is its preset's name, so a rename must do exactly what Save
# under an edited name does on Move: update_patch on the slot's OWN preset with
# the new name (the file is rewritten in place, every set using it sees the
# name), or save_patch for a slot that was never saved. Refused rather than
# guessed at: an empty slot, an audition in progress, and a name ANOTHER preset
# already has (Move asks before overwriting; a browser must not silently do
# what Move would have asked about).
#
# The functions are lifted out of shadow_ui.js by name and run under node
# against stubbed device calls, as test_remote_ui_panel_visible.sh does.
# Also pinned: the flag constant agrees with the C header, and a save on Move
# publishes the name to the shim (the bug that started this: a resaved slot
# kept its old name in the browser until reboot).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
JS="$ROOT/src/shadow/shadow_ui.js"
H="$ROOT/src/host/shadow_constants.h"

c_flag="$(grep -E '^#define SHADOW_UI_FLAG_SLOT_RENAME' "$H" | awk '{print $3}')"
js_flag="$(grep -E '^const SHADOW_UI_FLAG_SLOT_RENAME *=' "$JS" | sed -E 's/.*= *(0x[0-9A-Fa-f]+).*/\1/')"
[ -n "$c_flag" ] && [ "$c_flag" = "$js_flag" ] || {
    echo "FAIL: SHADOW_UI_FLAG_SLOT_RENAME is '$c_flag' in C and '$js_flag' in shadow_ui.js"; exit 1; }

node - "$JS" <<'NODE'
const fs = require("fs");
const src = fs.readFileSync(process.argv[2], "utf8");

// Lift a top-level function by name, braces balanced (strings/regex-light: the
// functions lifted here contain no unbalanced braces in literals).
function lift(name) {
  const start = src.indexOf("\nfunction " + name + "(");
  if (start < 0) { console.error("FAIL: function " + name + " not found"); process.exit(1); }
  let i = src.indexOf("{", start), depth = 0;
  for (; i < src.length; i++) {
    if (src[i] === "{") depth++;
    else if (src[i] === "}" && --depth === 0) break;
  }
  return src.slice(start + 1, i + 1);
}
function liftLine(re) {
  const m = src.match(re);
  if (!m) { console.error("FAIL: " + re + " not found"); process.exit(1); }
  return m[0];
}

let n = 0;
function check(cond, msg) { n++; if (!cond) { console.error("FAIL: " + msg); process.exit(1); } }

function world(o) {
  const w = {
    writes: [], saved: 0, reloaded: 0,
    slots: [{ name: "Pad" }, { name: "Untitled" }, { name: "Lead" }, { name: "" }],
    presets: ["Bass", "Lead", "Pad"],          // what is on disk, in list order
    modules: [true, true, true, false],        // slot 3 is empty
    preview: false,
    requests: {}, readFails: {},
  };
  Object.assign(w, o || {});
  const code =
    liftLine(/const SLOT_RENAME_MAX = \d+;/) + "\n" +
    "let slotRenameRetries = 0;\n" +
    liftLine(/const PATCH_LIBRARY_TIMEOUT_MS = \d+;/) + "\n" + lift("writePatchLibrary") + "\n" +
    lift("publishSlotName") + "\n" + lift("slotRenameRefusal") + "\n" +
    lift("renameSlotPreset") + "\n" + lift("serviceSlotRenames") + "\n" +
    "return { publishSlotName, slotRenameRefusal, renameSlotPreset, serviceSlotRenames };";
  const SHADOW_UI_SLOTS = w.slots.length;
  const env = {
    slots: w.slots, SHADOW_UI_SLOTS, chainConfigs: w.modules.map((m) => m),
    needsRedraw: false,
    setSlotParam: (s, k, v) => {
      if (w.failStatus && k === "slot:rename_status") return false;
      w.writes.push([s, k, String(v)]); return true;
    },
    // The library writes, as the chain performs them: BY NAME, or nothing.
    setSlotParamWithTimeout: (s, k, v, ms) => {
      w.writes.push([s, k, String(v), ms]);
      if (w.refuseLibrary) return true;            // answered, but wrote nothing
      if (k === "update_patch_by_name") {
        const nl = String(v).indexOf("\n");
        const old = String(v).slice(0, nl), json = JSON.parse(String(v).slice(nl + 1));
        const i = w.presets.indexOf(old);
        if (i >= 0 && w.presets.lastIndexOf(old) === i) w.presets[i] = json.custom_name;
      } else if (k === "save_patch") {
        w.presets.push(JSON.parse(String(v)).custom_name);
      }
      return true;
    },
    getSlotParam: (s, k) => {
      if (k !== "slot:rename_request") return null;
      if (w.readFails[s]) return null;
      return w.requests[s] || "";
    },
    findPatchByName: (name) => w.presets.indexOf(name),
    buildSlotPatchJson: (s, name) => JSON.stringify({ custom_name: name, slot: s }),
    saveSlotsToConfig: () => { w.saved++; },
    loadPatchList: () => { w.reloaded++; },
    isPresetPreviewActive: () => w.preview,
    applySlotModuleSignature: () => {},
    getSlotModuleSignature: () => "",
    chainHasAnyModule: (has) => has,
    debugLog: () => {},
  };
  const names = Object.keys(env);
  const f = new Function(...names, code);
  w.fn = f(...names.map((k) => env[k]));
  return w;
}

// A saved slot is rewritten IN PLACE, addressed by its CURRENT NAME -- never
// by an index, which named a different file in a slot whose list was stale.
let w = world();
check(w.fn.renameSlotPreset(0, "Warm Pad") === "", "renaming a saved slot succeeds");
let up = w.writes.find((x) => x[1] === "update_patch_by_name");
check(up && up[0] === 0 && up[2].startsWith("Pad\n"), "update_patch_by_name on the slot's own preset, by name");
check(JSON.parse(up[2].slice(up[2].indexOf("\n") + 1)).custom_name === "Warm Pad", "carrying the new name as custom_name");
check(up[3] >= 1000, "and WAITED for (the write runs off the callback): " + up[3] + " ms");
check(!w.writes.some((x) => x[1] === "save_patch" || x[1] === "update_patch" || x[1] === "delete_patch"),
      "no index-addressed write, and no new preset");
check(w.presets.indexOf("Warm Pad") >= 0 && w.presets.indexOf("Pad") < 0, "the preset itself was renamed");
check(w.slots[0].name === "Warm Pad" && w.saved === 1, "the slot takes the name and the config is saved");
check(w.writes.some((x) => x[0] === 0 && x[1] === "slot:name" && x[2] === "Warm Pad"),
      "the new name is PUBLISHED to the shim (the Remote UI reads it there)");

// The chain refuses a missing or ambiguous name by writing NOTHING. The rename
// must notice from the folder, and must not claim the name.
w = world({ refuseLibrary: true });
const nothing = w.fn.renameSlotPreset(0, "Warm Pad");
check(/nothing was renamed/.test(nothing), "a write that changed nothing is reported: " + nothing);
check(w.slots[0].name === "Pad" && !w.writes.some((x) => x[1] === "slot:name"),
      "and the slot keeps its old name");

// An unsaved slot is saved as a NEW preset under that name.
w = world();
check(w.fn.renameSlotPreset(1, "Keys") === "", "naming an unsaved slot succeeds");
check(w.writes.some((x) => x[0] === 1 && x[1] === "save_patch") &&
      !w.writes.some((x) => x[1] === "update_patch"), "an unsaved slot is save_patch, never update_patch");

// A name another preset already has is REFUSED, with nothing written.
w = world();
const clash = w.fn.renameSlotPreset(0, "Bass");
check(/already exists/.test(clash), "renaming onto another preset's name is refused: " + clash);
check(w.writes.length === 0 && w.slots[0].name === "Pad", "and nothing is written");
// ...but keeping its own name is not a clash.
w = world();
check(w.fn.renameSlotPreset(2, "Lead") === "", "a slot may keep its own name");

// The other refusals.
w = world();
check(/empty/.test(w.fn.renameSlotPreset(3, "Anything")), "an empty slot is refused");
w = world({ preview: true });
check(/auditioned/.test(w.fn.renameSlotPreset(0, "X")), "an audition in progress is refused");
w = world();
check(/quotes/.test(w.fn.renameSlotPreset(0, 'say "x"')), "quotes are refused");
check(/Untitled/.test(w.fn.renameSlotPreset(0, "Untitled")), "\"Untitled\" is refused");
check(/too long/.test(w.fn.renameSlotPreset(0, "x".repeat(64))), "an over-long name is refused");
check(w.writes.length === 0, "no refusal writes anything");

// The service loop: every pending request is done and ANSWERED with its seq.
w = world({ requests: { 0: "7:Warm Pad", 2: "8:Bass" } });
check(w.fn.serviceSlotRenames() === true, "a complete pass reports complete");
const st = w.writes.filter((x) => x[1] === "slot:rename_status");
check(st.length === 2, "one answer per request: " + JSON.stringify(st));
check(st.some((x) => x[0] === 0 && x[2] === "7:ok"), "slot 1 answered 7:ok");
check(st.some((x) => x[0] === 2 && /^8:err:A preset named "Bass" already exists/.test(x[2])),
      "slot 3 answered 8:err with the reason");

// A read that did not answer is not "no request": the tick must try again.
w = world({ requests: { 1: "9:Keys" }, readFails: { 1: true } });
check(w.fn.serviceSlotRenames() === false, "a failed read reports incomplete (so the tick retries)");
check(!w.writes.some((x) => x[1] === "slot:rename_status"), "and answers nothing it did not read");

// An answer that could not be written is retried too: the rename landed, but
// the browser has not been told, and the request is still pending in the shim.
w = world({ requests: { 0: "10:Warm Pad" }, failStatus: true });
check(w.fn.serviceSlotRenames() === false, "a pass whose answer failed to land reports incomplete");
check(w.slots[0].name === "Warm Pad", "(the rename itself did land)");

// publishSlotName: an unsaved slot publishes "" -- "Untitled" is not a name.
w = world();
w.fn.publishSlotName(1);
check(w.writes.some((x) => x[0] === 1 && x[1] === "slot:name" && x[2] === ""), "Untitled publishes as empty");

console.log("  shadow_ui rename: " + n + " checks OK");
NODE

# The save on Move publishes the name too -- the reported bug.
body="$(awk '/^function doSavePreset\(/,/^}/' "$JS")"
echo "$body" | grep -q 'publishSlotName(slotIndex)' || {
    echo "FAIL: doSavePreset must publish the saved name to the shim (publishSlotName)"; exit 1; }
# And the tick services the flag.
grep -q 'SHADOW_UI_FLAG_SLOT_RENAME) || slotRenameRetries > 0' "$JS" || {
    echo "FAIL: the tick must service SHADOW_UI_FLAG_SLOT_RENAME (and retry a failed pass)"; exit 1; }

echo "test_slot_rename_ui: all passed"

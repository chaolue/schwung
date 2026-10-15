#!/usr/bin/env bash
#
# The Remote UI's Mixer tab (schwung-manager/static/remote-ui-mixer.js).
#
# 1. The fader maths, run for real under node: a taper that is not monotonic,
#    or does not round-trip, is a fader that jumps when it is touched.
# 2. The client and the server agree on the KEY SET. The server refuses any
#    key not on its allowlist (remote_ui_mixer.go), so a control the client
#    draws for a key the server does not know is a control that silently does
#    nothing -- and a server key no control draws is a value nobody can see.
# 3. A TAP must not move a value. A native range input jumps to wherever it is
#    touched, which on a live return was a write of 6 (-26 dB) before the
#    double-click's 127 -- audible. The track takes the pointer instead.
# 4. The mixer script loads BEFORE remote-ui.js, which creates the tab from
#    window.SchwungMixer at load; in the other order the tab says "did not
#    load" and every other tab still works, so nothing else would notice.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
MGR="$ROOT/schwung-manager"
JS="$MGR/static/remote-ui-mixer.js"
GO="$MGR/remote_ui_mixer.go"
CSS="$MGR/static/remote-ui.css"
TPL="$MGR/templates/remote_ui.html"
for f in "$JS" "$GO" "$CSS" "$TPL"; do [ -f "$f" ] || { echo "FAIL: $f not found"; exit 1; }; done

# --- 1. The maths ----------------------------------------------------------
node - "$JS" <<'NODE'
const M = require(process.argv[2]);
let n = 0;
function check(cond, msg) { n++; if (!cond) { console.error("FAIL: " + msg); process.exit(1); } }

for (const [name, max] of [["volume", M.VOLUME_MAX], ["return", 1]]) {
  const t = M.makeTaper(max);
  check(t.toGain(0) === 0 && t.toPos(0) === 0, name + ": the bottom is silence");
  check(t.toGain(1) === max && t.toPos(max) === 1, name + ": the top is the ceiling");
  let prev = -1;
  for (let i = 0; i <= 1000; i++) {
    const g = t.toGain(i / 1000);
    check(g >= prev, name + ": monotonic at step " + i);
    prev = g;
    const back = Math.round(t.toPos(g) * 1000);
    check(Math.abs(back - i) <= 1, name + ": step " + i + " round-trips (got " + back + ")");
  }
}
// Unity where a mixer puts it, and exact on the wire from that position.
const vk = M.KINDS.volume;
check(Math.abs(parseFloat(vk.toWire(vk.unity)) - 1) < 0.005, "a fader's unity position writes 1.0 (got " + vk.toWire(vk.unity) + ")");
check(vk.unity > 750 && vk.unity < 880, "unity sits about four fifths up (" + vk.unity + ")");
check(vk.toInput("1.0000") === vk.unity, "1.0 reads back at the unity position");
// Every wire value a kind can produce is one the server accepts.
for (let i = 0; i <= 1000; i++) {
  const v = parseFloat(vk.toWire(i));
  check(v >= 0 && v <= M.VOLUME_MAX, "volume wire value in range at " + i);
  const r = parseInt(M.KINDS.ret.toWire(i), 10);
  check(Number.isInteger(r) && r >= 0 && r <= M.SEND_MAX, "return wire value is an int 0..127 at " + i);
}
check(M.KINDS.ret.toWire(1000) === "127" && M.KINDS.ret.toWire(0) === "0", "return ends are 0 and 127");
check(M.KINDS.ret.toInput("127") === 1000, "a return of 127 reads back at the top");
for (let i = -100; i <= 100; i++) {
  const p = parseFloat(M.KINDS.pan.toWire(i));
  check(p >= -1 && p <= 1, "pan wire value in range at " + i);
}
check(M.KINDS.send.toWire(64) === "64", "a send writes the integer level");

// The text the page and screen readers show.
check(M.formatDb(0) === "−∞ dB", "silence is -inf");
check(M.formatDb(1) === "0.0 dB", "unity is 0.0 dB, unsigned");
check(M.formatDb(0.5) === "−6.0 dB", "half is -6.0 dB");
check(M.formatDb(4) === "+12.0 dB", "the ceiling is +12.0 dB");
check(M.formatPan(0) === "C" && M.formatPan(0.004) === "C", "centre is C (the device's text)");
check(M.formatPan(-0.35) === "L 35" && M.formatPan(1) === "R 100", "sides are L/R n");
console.log("  mixer maths: " + n + " checks OK");
NODE

# --- 2. One key set ----------------------------------------------------------
go_keys() {  # the strings in one Go var's []string{...}
  sed -n "/^var $1 = \[\]string{/,/}/p" "$GO" | grep -oE '"[^"]+"' | tr -d '"'
}
server="$( { go_keys mixerSlotUIKeys; go_keys mixerSlotParamKeys; go_keys mixerGlobalKeys; } | sort -u)"
[ -n "$server" ] || { echo "FAIL: could not read the server's key lists from $GO"; exit 1; }
client="$(grep -oE '(slider|toggle)\((slot|null), "[^"]+"' "$JS" | grep -oE '"[^"]+"' | tr -d '"' | sort -u)"
[ -n "$client" ] || { echo "FAIL: could not read the client's controls from $JS"; exit 1; }
if [ "$server" != "$client" ]; then
    echo "FAIL: the Mixer's client and server disagree on the key set"
    echo "  server only: $(comm -23 <(echo "$server") <(echo "$client") | tr '\n' ' ')"
    echo "  client only: $(comm -13 <(echo "$server") <(echo "$client") | tr '\n' ' ')"
    exit 1
fi
echo "  key set: client and server agree ($(echo "$server" | wc -l) keys)"

# Values from the device are TEXT on this page, never markup.
if grep -n 'innerHTML' "$JS"; then
    echo "FAIL: remote-ui-mixer.js must build with textContent, not innerHTML"; exit 1
fi

# --- 3. A tap does not move a value -----------------------------------------
css_block="$(sed -n '/^\.mixer-control input\[type="range"\] {/,/^}/p' "$CSS")"
echo "$css_block" | grep -q 'pointer-events: none' || {
    echo "FAIL: the mixer's range inputs must not take the pointer (a tap would jump them)"; exit 1; }
grep -q 'track.addEventListener("pointerdown"' "$JS" || {
    echo "FAIL: the track must take the pointer and move the value relatively"; exit 1; }
if grep -q 'addEventListener("dblclick"' "$JS"; then
    echo "FAIL: a dblclick reset on a range input passes through a jump to the click point"; exit 1
fi
echo "  pointer: relative drag, no jump on tap"

# --- 4. Load order -----------------------------------------------------------
mix_line="$(grep -n 'remote-ui-mixer.js' "$TPL" | cut -d: -f1)"
rui_line="$(grep -n 'src="/static/remote-ui.js"' "$TPL" | cut -d: -f1)"
[ -n "$mix_line" ] && [ -n "$rui_line" ] && [ "$mix_line" -lt "$rui_line" ] || {
    echo "FAIL: remote-ui-mixer.js must load before remote-ui.js"; exit 1; }
grep -q 'data-slot="mixer"' "$TPL" || { echo "FAIL: no Mixer tab in $TPL"; exit 1; }
echo "  template: Mixer tab present, script order right"

echo "test_remote_ui_mixer: all passed"

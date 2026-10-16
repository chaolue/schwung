#!/usr/bin/env bash
# Knobs 1-8 on the Remote UI's Slot tabs.
#
# 1. A WRITE to a mapped parameter moves that knob's accumulator. The mapping
#    keeps its own current_value -- where the next hardware turn moves FROM --
#    and it used to change only on a hardware turn or a patch load, so editing
#    the parameter on the device's pages or in the browser left it stale and the
#    next turn jumped the parameter back. All three component write branches of
#    the chain's set_param must call knob_sync_from_write beside
#    lane_on_set_param. (chain_host.c cannot be compiled natively; the
#    serializer is driven for real by test_knob_map_json.c, the manager by
#    remote_ui_knobs_test.go.)
# 2. Hardware turns write the module directly (knob_forward_value), never
#    through set_param, so the sync cannot echo a turn back into itself.
# 3. The Slot tab draws mapped knobs from the server's slot_knobs, never from
#    display strings.
set -euo pipefail
cd "$(dirname "$0")/../.."
host=src/modules/chain/dsp/chain_host.c
params=src/modules/chain/dsp/chain_params.c
js=schwung-manager/static/remote-ui.js

n=$(grep -c 'knob_sync_from_write(inst, ' "$host" || true)
[ "$n" = "3" ] || { echo "FAIL: expected knob_sync_from_write in the synth, fx and midi_fx write branches (found $n)"; exit 1; }
for target in '"synth"' 'fx_id' 'mfx_id'; do
  grep -A1 "lane_on_set_param(inst, $target, subkey, val);" "$host" | grep -q "knob_sync_from_write(inst, $target, subkey, val);" || {
    echo "FAIL: the $target write branch must sync the knob beside lane_on_set_param"; exit 1; }
done

fwd=$(awk '/^void knob_forward_value\(/,/^}/' "$params")
if echo "$fwd" | grep -q 'v2_set_param\|knob_sync_from_write'; then
  echo "FAIL: knob_forward_value must write the MODULE directly, or a hardware turn echoes into its own sync"; exit 1
fi
echo "$fwd" | grep -q 'plugin_v2->set_param\|plugins_v2\[fx\]->set_param' || {
  echo "FAIL: knob_forward_value no longer writes the module directly"; exit 1; }

grep -q 'case "slot_knobs":' "$js" || { echo "FAIL: the Slot tab does not take slot_knobs"; exit 1; }
body=$(awk '/^    function renderMappedKnobs\(s\) \{/,/^    \}$/' "$js")
if echo "$body" | grep -q 'knob_[0-9N"+ ]*_value\|_name"'; then
  echo "FAIL: renderMappedKnobs reads knob display strings again"; exit 1
fi
echo "$body" | grep -q 'renderKnob(m.comp' || { echo "FAIL: mapped knobs must be real knobs on their parameter"; exit 1; }

echo "test_slot_knobs: all passed"

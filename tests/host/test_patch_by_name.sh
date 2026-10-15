#!/usr/bin/env bash
# The patch library is written BY NAME, and its writes run OFF the SPI callback.
# Behaviour: test_patch_by_name.c drives the real chain_patch.c. Wiring: the
# chain's set_param routes the _by_name keys through rescan + unique lookup,
# the shim hands every library write to the loader, and nothing in shadow_ui
# writes the library by index any more.
set -euo pipefail
cd "$(dirname "$0")/../.."

bin="build/tests/test_patch_by_name"
mkdir -p "$(dirname "$bin")"
cc -std=gnu11 -Wall -Wextra -Wno-unused-parameter -Wno-unused-function -Wno-format-truncation \
  -Isrc/modules/chain/dsp -Isrc/host -Isrc \
  tests/host/test_patch_by_name.c src/modules/chain/dsp/chain_json.c -o "$bin"
"$bin"

host=src/modules/chain/dsp/chain_host.c
for k in update_patch_by_name delete_patch_by_name; do
  body=$(awk "/strcmp\(key, \"$k\"\) == 0/,/^    }/" "$host")
  [ -n "$body" ] || { echo "FAIL: chain set_param has no $k"; exit 1; }
  echo "$body" | grep -q 'v2_scan_patches(inst)' || { echo "FAIL: $k must rescan before it looks"; exit 1; }
  echo "$body" | grep -q 'v2_find_unique_patch(' || { echo "FAIL: $k must act on the ONE preset with that name"; exit 1; }
done

job=src/host/slot_load_job.h
for k in save_patch update_patch delete_patch update_patch_by_name delete_patch_by_name; do
  grep -q "\"$k\"" <(awk '/slot_load_key_is_patch_io\(const char/,/^}/' "$job") || {
    echo "FAIL: $k is not a patch-io key, so it would run on the SPI callback"; exit 1; }
done
grep -q 'slot_load_key_is_patch_io(key_copy)' src/host/shadow_chain_mgmt.c || {
  echo "FAIL: the param handler does not hand library writes to the loader"; exit 1; }

if grep -nE 'setSlotParam\w*\([^)]*"(update_patch|delete_patch)"' src/shadow/shadow_ui.js; then
  echo "FAIL: shadow_ui writes the patch library by INDEX"; exit 1
fi
echo "test_patch_by_name: all passed"

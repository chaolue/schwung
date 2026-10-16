#!/usr/bin/env bash
# The Remote UI Mixer's meters: the maths (test_mix_meter.c), and WHERE the
# shim takes them, which is what makes a meter mean what its strip says.
set -euo pipefail
cd "$(dirname "$0")/../.."

bin="build/tests/test_mix_meter"
mkdir -p "$(dirname "$bin")"
cc -std=gnu11 -O2 -Wall -Wextra -Isrc/host tests/host/test_mix_meter.c -o "$bin" -lm
"$bin"

shim=src/schwung_shim.c
fail() { echo "FAIL: $*"; exit 1; }
line() { grep -nF -- "$1" "$shim" | head -1 | cut -d: -f1; }

# A slot's meter is taken in shadow_stem_store_slot BEFORE its stems-wanted
# early return: stems are off by default, and the meter must not be.
f=$(line 'static inline void shadow_stem_store_slot(int s')
tap=$(awk -v a="$f" 'NR>a && /shadow_meter_tap\(s, src/ {print NR; exit}' "$shim")
ret=$(awk -v a="$f" 'NR>a && /shadow_stems_wanted/ {print NR; exit}' "$shim")
[ -n "$tap" ] && [ -n "$ret" ] && [ "$tap" -lt "$ret" ] || fail "the slot meter is not taken ahead of the stems-wanted return"

# The master meter: after the master Level / Mute glide, before the capture
# view is built -- the same point recordings hear.
lvl=$(line 'const float t = shadow_master_bus_muted ? 0.0f : shadow_master_bus_level;')
mtap=$(line 'shadow_meter_tap(SHADOW_METER_MASTER, fx_target')
uv=$(line 'int16_t unity_view[FRAMES_PER_BLOCK * 2];')
[ -n "$lvl" ] && [ -n "$mtap" ] && [ -n "$uv" ] && [ "$lvl" -lt "$mtap" ] && [ "$mtap" -lt "$uv" ] \
  || fail "the master meter must sit after the master Level and before unity_view ($lvl/$mtap/$uv)"

# The return meter: after the return mute glide, on what the master add gets.
mute=$(line 'const float t = shadow_send_return_muted[sb] ? 0.0f : 1.0f;')
rtap=$(line 'shadow_meter_tap(SHADOW_METER_RETURN0 + sb, send_out[sb]')
add=$(line 'bus_mix_send(fx_target, send_out[sb], FRAMES_PER_BLOCK * 2, send_lvl);')
[ -n "$mute" ] && [ -n "$rtap" ] && [ -n "$add" ] && [ "$mute" -lt "$rtap" ] && [ "$rtap" -lt "$add" ] \
  || fail "the return meter must sit after the return mute and before the master add"

# Every block publishes, including the one where nothing of ours played.
[ "$(grep -c 'shadow_meters_publish();' "$shim")" -ge 2 ] || fail "both mix exits must publish the meters"
echo "test_mix_meter.sh: PASS"

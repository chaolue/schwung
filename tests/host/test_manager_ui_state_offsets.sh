#!/usr/bin/env bash
# schwung-manager (Go) reads shadow_ui_state_t (/schwung-ui) BY BYTE OFFSET, for
# the Remote UI's Mixer, from constants written by hand in
# schwung-manager/shmuistate.go. test_manager_shm_offsets.sh exists because the
# same arrangement for shadow_control_t drifted once (skipback_seconds moved
# from 60 to 62 and the web UI wrote the wrong field for months).
#
# This asks the compiler for offsetof() of every field the Go code names, and
# for the two array dimensions and the version the Go code gates on. A new
# uiOff* constant that is not in the table below fails too, so the next one
# cannot drift silently either.
set -euo pipefail
cd "$(dirname "$0")/../.."
GO=schwung-manager/shmuistate.go
T=$(mktemp -d "${TMPDIR:-/tmp}/mgr_ui.XXXXXX"); trap 'rm -rf "$T"' EXIT

# Go constant -> C field of shadow_ui_state_t
MAP='
uiOffVersion version
uiOffSlotCount slot_count
uiOffSlotVolumes slot_volumes
uiOffSlotNames slot_names
uiOffSlotMuted slot_muted
uiOffSlotSoloed slot_soloed
uiOffSlotVolumeLin slot_volume_lin
'

{
  echo '#include <stdio.h>'
  echo '#include <stddef.h>'
  echo '#include "shadow_constants.h"'
  echo '#define O(g, f) printf("%s %zu %zu\n", g, offsetof(shadow_ui_state_t, f), sizeof(((shadow_ui_state_t *)0)->f[0]))'
  echo '#define S(g, f) printf("%s %zu 0\n", g, offsetof(shadow_ui_state_t, f))'
  echo 'int main(void) {'
  echo "$MAP" | while read -r g f; do
    if [ -z "$g" ]; then continue; fi
    # Scalars and the 2-D name table have no single element width to check.
    case "$f" in version|slot_count|slot_names) echo "  S(\"$g\", $f);";; *) echo "  O(\"$g\", $f);";; esac
  done
  echo '  printf("uiStateSlots %d 0\n", SHADOW_UI_SLOTS);'
  echo '  printf("uiStateNameLen %d 0\n", SHADOW_UI_NAME_LEN);'
  echo '  return 0; }'
} > "$T/o.c"
${CC:-cc} -Isrc/host -Isrc -o "$T/o" "$T/o.c"
"$T/o" > "$T/want"

fails=0
for g in $(grep -oE '^\s*uiOff[A-Za-z]+\s*=' "$GO" | tr -d ' \t='); do
  if ! echo "$MAP" | grep -qE "^$g "; then
    echo "FAIL $g is in $GO but not in this test's table -- add its C field"; fails=$((fails+1))
  fi
done
while read -r g off elem; do
  line=$(grep -E "^\s*$g\s*=" "$GO" | head -1)
  if [ -z "$line" ]; then echo "FAIL $g missing from $GO"; fails=$((fails+1)); continue; fi
  got=$(echo "$line" | sed -E 's/^[^=]*=\s*([0-9]+).*/\1/')
  if [ "$got" != "$off" ]; then
    echo "FAIL $g: Go says $got, shadow_constants.h says $off"; fails=$((fails+1)); continue
  fi
  # The element width the Go comment declares must be the C element's width.
  ty=$(echo "$line" | grep -oE '(uint8|uint16|uint32|float32|char)' | head -1 || true)
  case "$ty" in uint8|char) w=1;; uint16) w=2;; uint32|float32) w=4;; *) w="";; esac
  if [ "$elem" != "0" ] && [ -n "$w" ] && [ "$w" != "$elem" ]; then
    echo "FAIL $g: Go treats it as $ty, the C element is $elem byte(s)"; fails=$((fails+1)); continue
  fi
  echo "ok   $g = $off"
done < "$T/want"

# The versions the reader gates on must exist. A later append the reader does
# not know about is fine; a gate on a version the shim never publishes means
# that field is never read, and the Mixer falls back to whole percent forever.
cv=$(grep -E '^#define SHADOW_UI_STATE_VERSION' src/host/shadow_constants.h | awk '{print $3}')
for g in uiStateVersionMuteSolo uiStateVersionVolumeLin; do
  gv=$(grep -E "^\s*$g\s*=" "$GO" | sed -E 's/^[^=]*=\s*([0-9]+).*/\1/')
  if [ -z "$gv" ] || [ "$gv" -gt "$cv" ]; then
    echo "FAIL $g = ${gv:-missing}, but SHADOW_UI_STATE_VERSION is $cv"; fails=$((fails+1))
  else
    echo "ok   $g = $gv (<= $cv)"
  fi
done

[ "$fails" -eq 0 ] && echo "test_manager_ui_state_offsets: all passed"
[ "$fails" -eq 0 ]

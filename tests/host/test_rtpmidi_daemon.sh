#!/usr/bin/env bash
# The network MIDI sidecar, built natively and driven over loopback by a test
# that plays both an inviting and a listening RTP-MIDI peer and stands in for
# the shim on the SHM rings. See test_rtpmidi_daemon.c.
set -euo pipefail
cd "$(dirname "$0")/../.."
OUT=build/tests/host
mkdir -p "$OUT"
CC="${CC:-cc}"
"$CC" -O2 -g -Wall -Wextra -Isrc -Isrc/host \
    src/host/rtpmidi_daemon.c src/host/rtpmidi_proto.c src/host/unified_log.c \
    -o "$OUT/schwung-rtpmidi-host" -lrt -lpthread
"$CC" -O2 -g -Wall -Wextra -Wno-unused-parameter -Isrc/host \
    tests/host/test_rtpmidi_daemon.c src/host/rtpmidi_proto.c \
    -o "$OUT/test_rtpmidi_daemon" -lrt
"$OUT/test_rtpmidi_daemon" "$OUT/schwung-rtpmidi-host"

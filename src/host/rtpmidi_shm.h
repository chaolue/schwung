/* rtpmidi_shm.h — the shim <-> schwung-rtpmidi shared segment.
 *
 * Network MIDI (RTP-MIDI / AppleMIDI, RFC 6295) is served by a SIDECAR
 * (src/host/rtpmidi_daemon.c), never by the shim: sockets are syscalls, and
 * everything the shim does with MIDI happens on the SPI callback. The two
 * halves meet here, in two single-producer / single-consumer rings of 4-byte
 * USB-MIDI packets:
 *
 *   out   shim  -> daemon   every cable-2 packet in the FINAL MIDI_OUT region,
 *                           i.e. what also leaves the USB-A port
 *   in    daemon -> shim    network MIDI, delivered into MIDI_IN as cable 2,
 *                           i.e. as if it had arrived on the USB-A port
 *
 * So the network is a second EXTERNAL port, and nothing above the shim knows
 * it exists: an overtake DSP's midi_send_external, a chain's CC out, a JS
 * move_midi_external_send and Move's own track MIDI out all reach it, and
 * what arrives reaches onMidiMessageExternal, chain slots and Move's tracks
 * by the routes cable 2 already has.
 *
 * Pure: no allocation, no I/O, no locks. The shim side is SPI-callback-safe.
 * Either process may create the segment (the daemon is started before the
 * shim by schwung-entry.sh); /dev/shm zero-fills, and all-zero is a valid
 * empty state for both rings. Host-tested in tests/host/test_rtpmidi_shm.c.
 */
#ifndef RTPMIDI_SHM_H
#define RTPMIDI_SHM_H

#include <stdint.h>
#include <string.h>

#define RTPMIDI_SHM_NAME      "/schwung-rtpmidi"
#define RTPMIDI_SHM_MAGIC     0x52544D31u   /* "RTM1" */
#define RTPMIDI_SHM_VERSION   1u

#define RTPMIDI_RING_PACKETS  1024u         /* power of two */
#define RTPMIDI_RING_MASK     (RTPMIDI_RING_PACKETS - 1u)

/* The MIDI_OUT region the tap scans: 20 x 4-byte USB-MIDI packets. */
#define RTPMIDI_TAP_BYTES     80

typedef struct {
    uint32_t head;                          /* producer writes */
    uint32_t tail;                          /* consumer writes */
    uint32_t drops;                         /* ring-full count (producer) */
    uint32_t reserved;
    uint8_t  pkt[RTPMIDI_RING_PACKETS][4];
} rtpmidi_ring_t;

typedef struct {
    uint32_t magic;                         /* daemon writes */
    uint32_t version;                       /* daemon writes */
    /* Non-zero while at least one session is up AND output is enabled. The
     * shim taps MIDI_OUT only then, so an idle or absent daemon costs the
     * callback one load and a branch. */
    uint32_t out_active;
    uint32_t peers;                         /* connected sessions (diagnostic) */
    uint32_t reserved[12];
    rtpmidi_ring_t out;                     /* shim -> daemon */
    rtpmidi_ring_t in;                      /* daemon -> shim */
} rtpmidi_shm_t;

/* Enqueue one packet. Drop-newest on full. Returns 1 when queued, 0 when
 * dropped. Single producer. */
static inline int rtpmidi_ring_push(rtpmidi_ring_t *r, const uint8_t pkt[4])
{
    uint32_t head = __atomic_load_n(&r->head, __ATOMIC_RELAXED);
    uint32_t tail = __atomic_load_n(&r->tail, __ATOMIC_ACQUIRE);
    if ((uint32_t)(head - tail) >= RTPMIDI_RING_PACKETS) {
        r->drops++;
        return 0;
    }
    /* GCC 13 misreads `&shm->in` as a pointer to the ring's first member
     * and reports this in-bounds copy as an overflow of `head`. */
#if defined(__GNUC__) && !defined(__clang__)
#pragma GCC diagnostic push
#pragma GCC diagnostic ignored "-Wstringop-overflow"
#endif
    memcpy(r->pkt[head & RTPMIDI_RING_MASK], pkt, 4);
#if defined(__GNUC__) && !defined(__clang__)
#pragma GCC diagnostic pop
#endif
    __atomic_store_n(&r->head, head + 1, __ATOMIC_RELEASE);
    return 1;
}

/* Copy the packet `ahead` positions past the tail WITHOUT consuming it.
 * Returns 1 if one was there. Single consumer. */
static inline int rtpmidi_ring_peek_at(rtpmidi_ring_t *r, uint8_t out[4],
                                       uint32_t ahead)
{
    uint32_t tail = __atomic_load_n(&r->tail, __ATOMIC_RELAXED);
    uint32_t head = __atomic_load_n(&r->head, __ATOMIC_ACQUIRE);
    uint32_t avail = (uint32_t)(head - tail);
    /* A peer process owns `head`; a corrupt value must not become an
     * out-of-range walk. */
    if (avail > RTPMIDI_RING_PACKETS) avail = RTPMIDI_RING_PACKETS;
    if (ahead >= avail) return 0;
    memcpy(out, r->pkt[(tail + ahead) & RTPMIDI_RING_MASK], 4);
    return 1;
}

/* Consume the packet at the tail. Single consumer; call only after a
 * successful peek. */
static inline void rtpmidi_ring_pop(rtpmidi_ring_t *r)
{
    uint32_t tail = __atomic_load_n(&r->tail, __ATOMIC_RELAXED);
    __atomic_store_n(&r->tail, tail + 1, __ATOMIC_RELEASE);
}

/* Consumer-side: forget everything queued. */
static inline void rtpmidi_ring_flush(rtpmidi_ring_t *r)
{
    __atomic_store_n(&r->tail, __atomic_load_n(&r->head, __ATOMIC_ACQUIRE),
                     __ATOMIC_RELEASE);
}

/* ---- The MIDI_OUT tap -----------------------------------------------------
 *
 * Runs at the very end of shim_pre_transfer, over the region exactly as it is
 * about to be transmitted, and queues every cable-2 packet for the network.
 *
 * WHY IT COMPARES AGAINST THE PREVIOUS FRAME. The post-transfer sync copies
 * the output region back from hardware, so a packet placed this frame is
 * STILL THERE next frame unless somebody rewrites the slot (Move normally
 * does; see ui_midi_out_carry.h, which measured ~0.6 lingering packets/sec).
 * A tap that queued whatever it found would send those twice. A packet that
 * is byte-identical to what the SAME SLOT held at the previous tap is that
 * copy-back, and is skipped. The cost is that a genuinely repeated identical
 * packet landing in the same slot on consecutive 2.9 ms frames is skipped
 * too; nothing a sequencer emits does that (24 ppq clock at 300 BPM is 8 ms
 * apart).
 *
 * `prev` is caller-owned state, RTPMIDI_TAP_BYTES long, zero-initialised.
 * It is updated even when nothing is queued (inactive daemon), so the first
 * frame after a session comes up does not replay a stale region.
 * Returns packets queued. */
static inline int rtpmidi_tap_midi_out(rtpmidi_shm_t *shm, uint8_t *prev,
                                       const uint8_t *midi_out)
{
    int queued = 0;
    int active = shm && __atomic_load_n(&shm->out_active, __ATOMIC_RELAXED);
    for (int j = 0; j < RTPMIDI_TAP_BYTES; j += 4) {
        const uint8_t *p = midi_out + j;
        if (active && (p[0] >> 4) == 2 && (p[0] & 0x0F) >= 2 &&
            memcmp(p, prev + j, 4) != 0) {
            queued += rtpmidi_ring_push(&shm->out, p);
        }
    }
    memcpy(prev, midi_out, RTPMIDI_TAP_BYTES);
    return queued;
}

/* ---- Delivery into MIDI_IN ------------------------------------------------
 *
 * Places up to `budget` queued network packets into the FIRST EMPTY 8-byte
 * slots of a MIDI_IN region (`in_bytes` long), in `shadow` and — when given —
 * in `hw` too, so Move and Schwung's own post-ioctl scans see the same event.
 * A zeroed MIDI_IN slot is a terminator, so only the first empty slot is ever
 * legal, and a packet that cannot land in BOTH buffers stays queued.
 *
 * Same two-half contract as the shim's test bus (shim_inject_as_hardware):
 * the pre-ioctl half PEEKS (`pop` = 0) so the pre-ioctl readers see it, the
 * post-ioctl half POPS. The cable nibble is forced to 2 whatever the daemon
 * wrote: a cable-0 packet here would be read as a Move button.
 * Returns packets placed. */
static inline int rtpmidi_deliver_midi_in(rtpmidi_shm_t *shm, uint8_t *shadow,
                                          uint8_t *hw, int in_bytes, int budget,
                                          int pop)
{
    if (!shm || !shadow) return 0;
    int placed = 0;
    for (int n = 0; n < budget; n++) {
        uint8_t pkt[4];
        if (!rtpmidi_ring_peek_at(&shm->in, pkt, pop ? 0u : (uint32_t)n)) break;
        uint8_t cin = pkt[0] & 0x0F;
        if (cin < 2) {                      /* reserved CINs: never deliver */
            if (pop) rtpmidi_ring_pop(&shm->in);
            continue;
        }
        pkt[0] = (uint8_t)(0x20 | cin);
        int off_sh = -1, off_hw = -1;
        for (int j = 0; j + 8 <= in_bytes; j += 8)
            if (!shadow[j] && !shadow[j+1] && !shadow[j+2] && !shadow[j+3]) {
                off_sh = j; break;
            }
        if (hw) {
            for (int j = 0; j + 8 <= in_bytes; j += 8)
                if (!hw[j] && !hw[j+1] && !hw[j+2] && !hw[j+3]) {
                    off_hw = j; break;
                }
        }
        if (off_sh < 0 || (hw && off_hw < 0)) break;
        if (pop) rtpmidi_ring_pop(&shm->in);
        memcpy(shadow + off_sh, pkt, 4);
        memset(shadow + off_sh + 4, 0, 4);  /* the timestamp half */
        if (hw) {
            memcpy(hw + off_hw, pkt, 4);
            memset(hw + off_hw + 4, 0, 4);
        }
        placed++;
    }
    return placed;
}

#endif /* RTPMIDI_SHM_H */

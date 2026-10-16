#ifndef PARAM_FEED_H
#define PARAM_FEED_H

/*
 * The parameter change feed: /schwung-param-feed (param_feed_t).
 *
 * A module learns about NO parameter change it did not make. The browser does
 * -- the shim pushes every completed set onto the web notify ring -- but that
 * ring has exactly one reader, schwung-manager, which drains it by resetting
 * write_idx. A second reader would steal the browser's notifications. So a
 * module that follows a parameter (the Arranger's Perform knobs follow the
 * chain parameters they are mapped to) had to READ it back, one ~3 ms round
 * trip at a time, and could only afford to every so often.
 *
 * This is the same stream, for ANY number of readers. Every set that reaches
 * on_param_changed -- the Remote UI's, the shadow UI's editors', any module's
 * or tool's -- is also appended here with a sequence number. Nothing is
 * drained: a reader remembers the last sequence it saw and asks for what came
 * after (shadow_param_changes() in shadow_ui). Reading is a copy out of shared
 * memory, no round trip, so a module can afford to ask every tick.
 *
 * What it does NOT carry: writes that never pass through set_param -- a
 * hardware knob mapped by Schwung (knob_forward_value writes the module
 * directly), LFO modulation, a patch load's own internal state. It reports
 * what was ASKED for: a module may clamp or round the value it stores.
 *
 * WRITERS may be more than one thread (the SPI callback serves both the param
 * channel and the web set ring; a worker can publish a deferred response), so
 * a writer RESERVES its sequence with an atomic add and then fills its entry:
 *
 *   1. s = ++head                     (atomic: s is this writer's alone)
 *   2. entry[s % N].seq = 0           (release: "being written")
 *   3. fill slot/key/value/origin
 *   4. entry[s % N].seq = s           (release: published)
 *
 * READERS never write the segment. For each s after the one they last saw:
 *   - entry.seq == s  -> copy it, then re-check seq; still s = a clean copy.
 *   - entry.seq <  s  -> its writer is between steps 1 and 4: STOP, and report
 *                        s - 1 as the position, so the next read retries it.
 *   - entry.seq >  s  -> overwritten by a newer one: LOST, carry on.
 * and anything more than PARAM_FEED_ENTRIES behind head is lost outright.
 * A reader told it lost something should re-read what it follows directly.
 *
 * Sequences start at 1 (0 means "being written"), and all distances are taken
 * as (uint32_t)(a - b), which stays exact across the 2^32 wrap.
 *
 * Header-only and dependency-free so tests/host runs it natively
 * (tests/host/test_param_feed.c).
 */

#include <stdint.h>
#include <string.h>

#include "shadow_constants.h"

_Static_assert((PARAM_FEED_ENTRIES & (PARAM_FEED_ENTRIES - 1)) == 0,
               "PARAM_FEED_ENTRIES must be a power of two");
#define PARAM_FEED_MASK (PARAM_FEED_ENTRIES - 1)

/* One change, as a reader gets it. */
typedef struct {
    uint32_t seq;
    uint8_t slot;
    uint8_t origin;      /* PARAM_ORIGIN_* */
    uint8_t truncated;   /* the value was longer than PARAM_FEED_VALUE_LEN - 1 */
    char key[PARAM_FEED_KEY_LEN];
    char value[PARAM_FEED_VALUE_LEN];
} param_feed_change_t;

/* WRITER: append one change. RT-safe: no allocation, no locks, no I/O. */
static inline void param_feed_push(param_feed_t *f, uint8_t slot, const char *key,
                                   const char *value, uint8_t origin)
{
    if (!f || !key) return;
    uint32_t s = __atomic_add_fetch(&f->head, 1, __ATOMIC_ACQ_REL);
    if (s == 0) s = __atomic_add_fetch(&f->head, 1, __ATOMIC_ACQ_REL);  /* 0 is "being written" */
    param_feed_entry_t *e = &f->entries[s & PARAM_FEED_MASK];
    __atomic_store_n(&e->seq, 0, __ATOMIC_RELEASE);
    e->slot = slot;
    e->origin = origin;
    strncpy(e->key, key, PARAM_FEED_KEY_LEN - 1);
    e->key[PARAM_FEED_KEY_LEN - 1] = '\0';
    const char *v = value ? value : "";
    size_t vlen = strlen(v);
    e->truncated = vlen > PARAM_FEED_VALUE_LEN - 1;
    if (vlen > PARAM_FEED_VALUE_LEN - 1) vlen = PARAM_FEED_VALUE_LEN - 1;
    memcpy(e->value, v, vlen);
    e->value[vlen] = '\0';
    __atomic_store_n(&e->seq, s, __ATOMIC_RELEASE);
}

/* The newest sequence handed out: where a new reader starts. */
static inline uint32_t param_feed_head(const param_feed_t *f)
{
    return f ? __atomic_load_n(&f->head, __ATOMIC_ACQUIRE) : 0;
}

/*
 * READER: the changes after `since`, at most `max` of them, into `out`.
 * Returns how many; *pos is where to read from next time (pass it back as
 * `since`), and *lost is set when any change after `since` was missed.
 */
static inline int param_feed_read(const param_feed_t *f, uint32_t since,
                                  param_feed_change_t *out, int max,
                                  uint32_t *pos, int *lost)
{
    int n = 0;
    *lost = 0;
    *pos = since;
    if (!f || max <= 0) return 0;
    uint32_t head = __atomic_load_n(&f->head, __ATOMIC_ACQUIRE);
    uint32_t behind = (uint32_t)(head - since);
    if (behind == 0 || behind > 0x80000000u) return 0;  /* nothing new (or a stale future position) */
    uint32_t s = since + 1;
    if (behind > PARAM_FEED_ENTRIES) {
        *lost = 1;
        s = head - PARAM_FEED_ENTRIES + 1;
    }
    for (; (uint32_t)(head - s) < 0x80000000u && n < max; s++) {
        if (s == 0) continue;
        const param_feed_entry_t *e = &f->entries[s & PARAM_FEED_MASK];
        uint32_t s1 = __atomic_load_n(&e->seq, __ATOMIC_ACQUIRE);
        if (s1 != s) {
            if (s1 == 0 || (int32_t)(s1 - s) < 0) break;   /* still being written: retry it */
            *lost = 1;                                      /* overwritten */
            *pos = s;
            continue;
        }
        param_feed_change_t *c = &out[n];
        c->seq = s;
        c->slot = e->slot;
        c->origin = e->origin;
        c->truncated = e->truncated;
        memcpy(c->key, e->key, PARAM_FEED_KEY_LEN);
        c->key[PARAM_FEED_KEY_LEN - 1] = '\0';
        memcpy(c->value, e->value, PARAM_FEED_VALUE_LEN);
        c->value[PARAM_FEED_VALUE_LEN - 1] = '\0';
        __atomic_thread_fence(__ATOMIC_ACQUIRE);
        if (__atomic_load_n(&e->seq, __ATOMIC_ACQUIRE) != s) {   /* overwritten while copying */
            *lost = 1;
            *pos = s;
            continue;
        }
        n++;
        *pos = s;
    }
    return n;
}

#endif /* PARAM_FEED_H */

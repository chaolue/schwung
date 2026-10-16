/*
 * Unit test for param_feed.h — the param change feed (/schwung-param-feed).
 *
 * The feed is the web notify ring's stream for ANY number of readers: never
 * drained, each reader keeps its own position. What has to hold:
 *   - a reader gets every change after its position, in order, and its new
 *     position; two readers do not disturb each other;
 *   - a reader that fell more than PARAM_FEED_ENTRIES behind is TOLD (lost),
 *     and gets the newest PARAM_FEED_ENTRIES;
 *   - an entry whose writer has reserved but not published it STOPS the read
 *     (retried next time) rather than being skipped as lost;
 *   - a too-long value is truncated and flagged;
 *   - several writer threads and a reader racing them never yield a torn or
 *     out-of-order change, and nothing is missed unless reported lost.
 */
#include <assert.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "param_feed.h"

static param_feed_t feed;
static param_feed_change_t out[PARAM_FEED_ENTRIES * 2];

static void push_n(int from, int count) {
    char k[32], v[32];
    for (int i = from; i < from + count; i++) {
        snprintf(k, sizeof k, "synth:p%d", i);
        snprintf(v, sizeof v, "%d", i);
        param_feed_push(&feed, (uint8_t)(i % 8), k, v, PARAM_ORIGIN_WEB);
    }
}

static void test_basic_and_two_readers(void) {
    memset(&feed, 0, sizeof feed);
    uint32_t a = param_feed_head(&feed), b = a, pos;
    int lost;
    assert(param_feed_read(&feed, a, out, 64, &pos, &lost) == 0 && pos == a && !lost);
    push_n(0, 5);
    int n = param_feed_read(&feed, a, out, 64, &pos, &lost);
    assert(n == 5 && !lost && pos == 5);
    for (int i = 0; i < 5; i++) {
        char v[32]; snprintf(v, sizeof v, "%d", i);
        assert(out[i].seq == (uint32_t)(i + 1) && strcmp(out[i].value, v) == 0);
        assert(out[i].slot == i % 8 && out[i].origin == PARAM_ORIGIN_WEB && !out[i].truncated);
    }
    a = pos;
    push_n(5, 3);
    /* reader B still at 0: sees all 8; reader A: the 3 new ones */
    assert(param_feed_read(&feed, b, out, 64, &pos, &lost) == 8 && pos == 8);
    assert(param_feed_read(&feed, a, out, 64, &pos, &lost) == 3 && pos == 8 && out[0].seq == 6);
    /* max limits a read; the position lets the rest follow */
    assert(param_feed_read(&feed, 0, out, 2, &pos, &lost) == 2 && pos == 2);
    assert(param_feed_read(&feed, pos, out, 64, &pos, &lost) == 6 && pos == 8);
    printf("ok: readers keep their own positions\n");
}

static void test_lost(void) {
    memset(&feed, 0, sizeof feed);
    push_n(0, PARAM_FEED_ENTRIES + 10);
    uint32_t pos; int lost;
    int n = param_feed_read(&feed, 0, out, PARAM_FEED_ENTRIES * 2, &pos, &lost);
    assert(lost && n == PARAM_FEED_ENTRIES && pos == PARAM_FEED_ENTRIES + 10);
    assert(out[0].seq == 11);
    printf("ok: a reader too far behind is told it lost changes\n");
}

static void test_in_flight_stops(void) {
    memset(&feed, 0, sizeof feed);
    push_n(0, 3);
    /* A writer reserved seq 4 and has not published it; seq 5 is published. */
    __atomic_add_fetch(&feed.head, 1, __ATOMIC_ACQ_REL);
    feed.entries[4 & PARAM_FEED_MASK].seq = 0;
    param_feed_push(&feed, 0, "synth:late", "5", PARAM_ORIGIN_CHANNEL);
    uint32_t pos; int lost;
    int n = param_feed_read(&feed, 0, out, 64, &pos, &lost);
    assert(n == 3 && pos == 3 && !lost);
    /* now it lands */
    param_feed_entry_t *e = &feed.entries[4 & PARAM_FEED_MASK];
    strcpy(e->key, "synth:p3"); strcpy(e->value, "x");
    __atomic_store_n(&e->seq, 4, __ATOMIC_RELEASE);
    n = param_feed_read(&feed, pos, out, 64, &pos, &lost);
    assert(n == 2 && pos == 5 && out[0].seq == 4 && out[1].origin == PARAM_ORIGIN_CHANNEL);
    printf("ok: an entry still being written is retried, not lost\n");
}

static void test_truncation(void) {
    memset(&feed, 0, sizeof feed);
    char big[PARAM_FEED_VALUE_LEN * 2];
    memset(big, 'v', sizeof big - 1); big[sizeof big - 1] = '\0';
    param_feed_push(&feed, 1, "synth:state", big, PARAM_ORIGIN_CHANNEL);
    uint32_t pos; int lost;
    assert(param_feed_read(&feed, 0, out, 4, &pos, &lost) == 1);
    assert(out[0].truncated && strlen(out[0].value) == PARAM_FEED_VALUE_LEN - 1);
    printf("ok: a long value is truncated and flagged\n");
}

/* Stress: writers push "<writer>:<n>" in order; the reader checks each
 * writer's numbers never go backwards and accounts for every one it did not
 * see by a lost report. */
#define WRITERS 3
#define PER_WRITER 200000
static volatile int writers_done = 0;
static void *writer(void *arg) {
    int w = (int)(long)arg;
    char v[32];
    for (int i = 0; i < PER_WRITER; i++) {
        snprintf(v, sizeof v, "%d:%d", w, i);
        param_feed_push(&feed, (uint8_t)w, "k", v, PARAM_ORIGIN_CHANNEL);
    }
    __atomic_add_fetch(&writers_done, 1, __ATOMIC_ACQ_REL);
    return NULL;
}

static void test_stress(void) {
    memset(&feed, 0, sizeof feed);
    pthread_t th[WRITERS];
    for (long w = 0; w < WRITERS; w++) pthread_create(&th[w], NULL, writer, (void *)w);
    int last[WRITERS]; for (int w = 0; w < WRITERS; w++) last[w] = -1;
    uint32_t pos = 0; long seen = 0, torn = 0, losses = 0;
    for (;;) {
        int lost;
        int done = __atomic_load_n(&writers_done, __ATOMIC_ACQUIRE) == WRITERS;
        int n = param_feed_read(&feed, pos, out, 64, &pos, &lost);
        if (lost) losses++;
        for (int i = 0; i < n; i++) {
            int w, k;
            if (sscanf(out[i].value, "%d:%d", &w, &k) != 2 || w < 0 || w >= WRITERS || out[i].slot != w) { torn++; continue; }
            if (k <= last[w]) torn++;     /* out of order within a writer */
            last[w] = k;
            seen++;
        }
        if (done && n == 0 && pos == param_feed_head(&feed)) break;
    }
    for (int w = 0; w < WRITERS; w++) pthread_join(th[w], NULL);
    printf("info: stress saw %ld of %d, %ld lost reports\n", seen, WRITERS * PER_WRITER, losses);
    assert(torn == 0);
    assert(seen == (long)WRITERS * PER_WRITER || losses > 0);
    /* Caught up: the reader ends exactly at the head. (A writer's LAST change
     * is not guaranteed to be seen -- one that finished early can be
     * overwritten by the others before the reader gets there, which is a
     * reported loss, not a defect.) */
    assert(pos == param_feed_head(&feed));
    printf("ok: racing writers and a reader: no torn or reordered change\n");
}

int main(void) {
    test_basic_and_two_readers();
    test_lost();
    test_in_flight_stops();
    test_truncation();
    test_stress();
    printf("ALL TESTS PASSED\n");
    return 0;
}

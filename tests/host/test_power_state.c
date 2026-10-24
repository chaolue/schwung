/*
 * Unit test for power_state.h — the XMOS battery SysEx and Move's syslog line.
 *
 * What has to hold:
 *   - the captured message (F0 00 21 1D 01 01 3A 21 19 00 F7) parses as 25%,
 *     framed as the hardware framed it (three 0x04 packets and a 0x06 end);
 *   - the same message split across calls (frames) still parses;
 *   - other SysEx, other commands, a level above 100 and a message whose
 *     start was missed are refused;
 *   - non-SysEx packets in the middle of a message do not disturb it;
 *   - an oversized SysEx is ignored to its end and the next one still parses;
 *   - the flags' low nibble reads as the charging state (0x21 charging, 0x22
 *     full);
 *   - the LAST PowerState line in a syslog chunk wins, its fields are read,
 *     and a chunk without one, or with a cut-off line, is handled.
 */
#include <assert.h>
#include <stdio.h>
#include <string.h>

#include "power_state.h"

static int feed_all(power_sysex_t *p, const uint8_t (*pkts)[4], int n, power_report_t *r) {
    int got = 0;
    for (int i = 0; i < n; i++) got += power_sysex_feed(p, pkts[i], r);
    return got;
}

static void test_captured_message(void) {
    /* Exactly as the shim's log showed it, slot by slot. */
    const uint8_t pkts[][4] = {
        { 0x04, 0xF0, 0x00, 0x21 },
        { 0x04, 0x1D, 0x01, 0x01 },
        { 0x04, 0x3A, 0x21, 0x19 },
        { 0x06, 0x00, 0xF7, 0x00 },
    };
    power_sysex_t p = {0};
    power_report_t r = {0};
    assert(feed_all(&p, pkts, 4, &r) == 1);
    assert(r.level == 25 && r.flags == 0x21);
    assert(p.len == 0);
}

static void test_split_and_interleaved(void) {
    power_sysex_t p = {0};
    power_report_t r = {0};
    const uint8_t a[4] = { 0x04, 0xF0, 0x00, 0x21 };
    const uint8_t b[4] = { 0x04, 0x1D, 0x01, 0x01 };
    const uint8_t note[4] = { 0x09, 0x90, 0x44, 0x7F };   /* a pad in between */
    const uint8_t cc[4] = { 0x0B, 0xB0, 0x47, 0x01 };
    const uint8_t c[4] = { 0x04, 0x3A, 0x01, 0x64 };      /* 100% */
    const uint8_t d[4] = { 0x06, 0x00, 0xF7, 0x00 };
    assert(power_sysex_feed(&p, a, &r) == 0);
    assert(power_sysex_feed(&p, note, &r) == 0);
    assert(power_sysex_feed(&p, b, &r) == 0);
    assert(power_sysex_feed(&p, cc, &r) == 0);
    assert(power_sysex_feed(&p, c, &r) == 0);
    assert(power_sysex_feed(&p, d, &r) == 1);
    assert(r.level == 100 && r.flags == 0x01);
}

static void test_refusals(void) {
    power_report_t r = {0};
    /* Another Ableton command (3B is the RGB LED SysEx's). */
    {
        const uint8_t pkts[][4] = {
            { 0x04, 0xF0, 0x00, 0x21 }, { 0x04, 0x1D, 0x01, 0x01 },
            { 0x04, 0x3B, 0x21, 0x19 }, { 0x06, 0x00, 0xF7, 0x00 },
        };
        power_sysex_t p = {0};
        assert(feed_all(&p, pkts, 4, &r) == 0);
    }
    /* Another manufacturer. */
    {
        const uint8_t pkts[][4] = {
            { 0x04, 0xF0, 0x00, 0x20 }, { 0x04, 0x29, 0x01, 0x01 },
            { 0x04, 0x3A, 0x21, 0x19 }, { 0x06, 0x00, 0xF7, 0x00 },
        };
        power_sysex_t p = {0};
        assert(feed_all(&p, pkts, 4, &r) == 0);
    }
    /* A level that is not a percentage. */
    {
        const uint8_t pkts[][4] = {
            { 0x04, 0xF0, 0x00, 0x21 }, { 0x04, 0x1D, 0x01, 0x01 },
            { 0x04, 0x3A, 0x21, 0x65 }, { 0x06, 0x00, 0xF7, 0x00 },
        };
        power_sysex_t p = {0};
        assert(feed_all(&p, pkts, 4, &r) == 0);
    }
    /* Too short: ends straight after the command byte. */
    {
        const uint8_t pkts[][4] = {
            { 0x04, 0xF0, 0x00, 0x21 }, { 0x04, 0x1D, 0x01, 0x01 },
            { 0x06, 0x3A, 0xF7, 0x00 },
        };
        power_sysex_t p = {0};
        assert(feed_all(&p, pkts, 3, &r) == 0);
    }
    /* Start missed (the shim attached mid-message): only the tail is seen. */
    {
        const uint8_t pkts[][4] = {
            { 0x04, 0x3A, 0x21, 0x19 }, { 0x06, 0x00, 0xF7, 0x00 },
        };
        power_sysex_t p = {0};
        assert(feed_all(&p, pkts, 2, &r) == 0);
        assert(p.len == 0);
    }
}

static void test_oversized_then_valid(void) {
    power_sysex_t p = {0};
    power_report_t r = {0};
    const uint8_t start[4] = { 0x04, 0xF0, 0x00, 0x21 };
    const uint8_t mid[4] = { 0x04, 0x1D, 0x01, 0x01 };
    const uint8_t fill[4] = { 0x04, 0x10, 0x10, 0x10 };
    const uint8_t end[4] = { 0x06, 0x00, 0xF7, 0x00 };
    assert(power_sysex_feed(&p, start, &r) == 0);
    for (int i = 0; i < 20; i++) assert(power_sysex_feed(&p, fill, &r) == 0);
    assert(power_sysex_feed(&p, end, &r) == 0);
    assert(p.len == 0 && p.overflow == 0);

    const uint8_t c[4] = { 0x04, 0x3A, 0x21, 0x0A };   /* 10% */
    assert(power_sysex_feed(&p, start, &r) == 0);
    assert(power_sysex_feed(&p, mid, &r) == 0);
    assert(power_sysex_feed(&p, c, &r) == 0);
    assert(power_sysex_feed(&p, end, &r) == 1 && r.level == 10);
}

static void test_restart_on_new_f0(void) {
    /* A message abandoned mid-way and a new one started: the new one wins. */
    power_sysex_t p = {0};
    power_report_t r = {0};
    const uint8_t pkts[][4] = {
        { 0x04, 0xF0, 0x00, 0x21 }, { 0x04, 0x1D, 0x01, 0x01 },
        { 0x04, 0xF0, 0x00, 0x21 }, { 0x04, 0x1D, 0x01, 0x01 },
        { 0x04, 0x3A, 0x20, 0x05 }, { 0x06, 0x00, 0xF7, 0x00 },
    };
    assert(feed_all(&p, pkts, 6, &r) == 1);
    assert(r.level == 5 && r.flags == 0x20);
}

static void test_syslog(void) {
    power_log_t l;
    const char *log =
        "Oct 18 07:09:09 move user.info MoveOriginal: PowerState{  isPsuConnected: true,  "
        "chargingState: charging,  batteryCharge: 24,  lowPowerAlarm: false,  shortPressed: false,  "
        "xmosInShutdown: false }\n"
        "Oct 18 07:10:00 move user.info MoveOriginal: something else\n"
        "Oct 18 07:11:19 move user.info MoveOriginal: PowerState{  isPsuConnected: false,  "
        "chargingState: discharging,  batteryCharge: 7,  lowPowerAlarm: true,  shortPressed: false,  "
        "xmosInShutdown: false }\n"
        "Oct 18 07:12:00 move user.info MoveOriginal: Power state update\n";
    assert(power_syslog_parse(log, &l) == 1);
    assert(l.level == 7 && l.psu == 0 && l.charging == 0);

    const char *charging =
        "x PowerState{  isPsuConnected: true,  chargingState: charging,  batteryCharge: 100,  lowPowerAlarm: false }\n";
    assert(power_syslog_parse(charging, &l) == 1);
    assert(l.level == 100 && l.psu == 1 && l.charging == 1);

    /* "fullyCharged" and "notCharging" are not "charging"; "fullyCharged" is full. */
    const char *full =
        "x PowerState{  isPsuConnected: true,  chargingState: fullyCharged,  batteryCharge: 100 }\n";
    assert(power_syslog_parse(full, &l) == 1 && l.charging == 0 && l.full == 1 && l.psu == 1);

    /* No line at all. */
    assert(power_syslog_parse("Oct 18 nothing here\n", &l) == 0 && l.level == -1);
    assert(power_syslog_parse(NULL, &l) == 0);

    /* The read cut the last line off before its charge: the field is missing,
     * so that line is refused rather than an earlier one being taken. */
    const char *cut =
        "a PowerState{  isPsuConnected: true,  chargingState: charging,  batteryCharge: 40 }\n"
        "b PowerState{  isPsuConnected: true,  chargingSt";
    assert(power_syslog_parse(cut, &l) == 0);

    /* Cut after the charge: still read. */
    const char *cut2 = "b PowerState{  isPsuConnected: true,  chargingState: charging,  batteryCharge: 41,  low";
    assert(power_syslog_parse(cut2, &l) == 1 && l.level == 41);
}

static void test_flags_state(void) {
    /* Both captured on the charger: charging, then fully charged. */
    assert(power_flags_state(0x21) == POWER_STATE_CHARGING);
    assert(power_flags_state(0x22) == POWER_STATE_FULL);
    assert(power_flags_state(0x00) != POWER_STATE_CHARGING && power_flags_state(0x00) != POWER_STATE_FULL);
}

int main(void) {
    test_captured_message();
    test_flags_state();
    test_split_and_interleaved();
    test_refusals();
    test_oversized_then_valid();
    test_restart_on_new_f0();
    test_syslog();
    printf("test_power_state: all passed\n");
    return 0;
}

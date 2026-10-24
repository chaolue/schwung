#ifndef POWER_STATE_H
#define POWER_STATE_H

/*
 * The battery: what the XMOS reports and what Move logs.
 *
 * The Move has no Linux power-supply driver -- /sys/class/power_supply is
 * empty. The XMOS (the control MCU) owns the battery and reports it to Move as
 * an Ableton SysEx on MIDI_IN, cable 0, about once per 1% change:
 *
 *   F0 00 21 1D 01 01 3A <flags> <level> <x> F7
 *
 * Captured with log_xmos_sysex_on on 2026-10-18 as `F0 00 21 1D 01 01 3A 21 19
 * 00 F7`, in the same second Move logged "batteryCharge: 25". <level> is the
 * charge in percent. <flags>' low nibble is Move's chargingState: 0x21 while
 * Move logged "chargingState: charging", 0x22 (read from shadow_control_t on
 * 2026-10-24) while it logged "fullyCharged", both on the charger -- see
 * power_flags_state. Its other bits, and the nibble on battery, are not
 * captured yet: the byte is still passed through raw.
 *
 * The shim only WATCHES the message (power_sysex_feed below); it still goes to
 * Move untouched. Until the first one arrives -- up to a few minutes after a
 * restart -- the level is unknown, and the last line Move wrote to syslog is
 * the fallback:
 *
 *   PowerState{  isPsuConnected: true,  chargingState: charging,
 *                batteryCharge: 25,  lowPowerAlarm: false, ... }
 *
 * (power_syslog_parse below; shadow_ui reads the file, see js_host_battery.)
 *
 * Header-only and dependency-free so tests/host runs it natively
 * (tests/host/test_power_state.c).
 */

#include <stdint.h>
#include <string.h>
#include <stdlib.h>

#define POWER_LEVEL_UNKNOWN 255

/* The fixed head of the power message, F0 through the 3A command byte. */
static const uint8_t POWER_SYSEX_HEAD[7] = { 0xF0, 0x00, 0x21, 0x1D, 0x01, 0x01, 0x3A };
#define POWER_SYSEX_MAX 16   /* the message is 11 bytes; anything longer is not it */

/* Reassembles SysEx from USB-MIDI packets. A message can in principle span
 * SPI frames, so the state lives across calls. */
typedef struct {
    uint8_t buf[POWER_SYSEX_MAX];
    int len;        /* bytes held; 0 = not inside a SysEx */
    int overflow;   /* this SysEx outgrew buf: ignore it to its end */
} power_sysex_t;

/* One parsed power message. */
typedef struct {
    uint8_t level;   /* 0..100 */
    uint8_t flags;   /* raw, undecoded */
} power_report_t;

/* <flags>' low nibble: Move's chargingState. Only these two are known (both
 * on the charger); anything else -- on battery, presumably -- is neither. */
#define POWER_STATE_CHARGING 1
#define POWER_STATE_FULL     2
static inline int power_flags_state(uint8_t flags) { return flags & 0x0F; }

/* Validate a complete SysEx (F0 .. F7). */
static inline int power_sysex_parse(const uint8_t *m, int len, power_report_t *out)
{
    if (len < 10 || m[len - 1] != 0xF7) return 0;
    if (memcmp(m, POWER_SYSEX_HEAD, sizeof POWER_SYSEX_HEAD) != 0) return 0;
    uint8_t flags = m[7], level = m[8];
    if (level > 100) return 0;
    out->flags = flags;
    out->level = level;
    return 1;
}

/*
 * Feed ONE USB-MIDI packet (the 4 bytes: cable/CIN, then 3 data bytes).
 * Returns 1 and fills *out when this packet completes a power message.
 * Packets that are not SysEx (CIN outside 4..7) are ignored without disturbing
 * a message in progress -- a running SysEx can be interleaved with realtime.
 * RT-safe: no allocation, no I/O.
 */
static inline int power_sysex_feed(power_sysex_t *p, const uint8_t pkt[4], power_report_t *out)
{
    uint8_t cin = pkt[0] & 0x0F;
    int n;
    switch (cin) {
        case 0x04: n = 3; break;   /* starts or continues */
        case 0x05: n = 1; break;   /* ends with one byte */
        case 0x06: n = 2; break;   /* ends with two */
        case 0x07: n = 3; break;   /* ends with three */
        default: return 0;
    }
    int ended = 0;
    for (int i = 0; i < n; i++) {
        uint8_t b = pkt[1 + i];
        if (b == 0xF0) { p->len = 0; p->overflow = 0; }
        else if (p->len == 0 && !p->overflow) continue;   /* mid-message: we missed its start */
        if (p->len < POWER_SYSEX_MAX) p->buf[p->len++] = b;
        else p->overflow = 1;
        if (b == 0xF7) { ended = 1; break; }
    }
    if (!ended && cin == 0x04) return 0;
    /* An end packet (or an F7 inside a 0x04): the message is over either way. */
    int ok = !p->overflow && p->len > 0 && power_sysex_parse(p->buf, p->len, out);
    p->len = 0;
    p->overflow = 0;
    return ok;
}

/* One syslog PowerState line, as far as it was found. */
typedef struct {
    int level;      /* batteryCharge, 0..100; -1 = no line found */
    int psu;        /* isPsuConnected: 1/0, -1 unknown */
    int charging;   /* chargingState == charging: 1/0, -1 unknown */
    int full;       /* chargingState == fullyCharged: 1/0, -1 unknown */
} power_log_t;

/* The value after `key` within [s, end), or NULL. */
static inline const char *power_log_field(const char *s, const char *end, const char *key)
{
    size_t kl = strlen(key);
    for (const char *p = s; p + kl <= end; p++) {
        if (memcmp(p, key, kl) == 0) {
            p += kl;
            while (p < end && *p == ' ') p++;
            return p < end ? p : NULL;
        }
    }
    return NULL;
}

/*
 * Parse the LAST "PowerState{" line in a NUL-terminated chunk of syslog.
 * Returns 1 and fills *out when one with a valid batteryCharge was found.
 */
static inline int power_syslog_parse(const char *text, power_log_t *out)
{
    out->level = -1;
    out->psu = -1;
    out->charging = -1;
    out->full = -1;
    if (!text) return 0;
    const char *last = NULL;
    for (const char *p = strstr(text, "PowerState{"); p; p = strstr(p + 1, "PowerState{")) last = p;
    if (!last) return 0;
    const char *end = strchr(last, '}');
    if (!end) end = last + strlen(last);   /* a line cut off by the read: take what is there */

    const char *v = power_log_field(last, end, "batteryCharge:");
    if (!v || *v < '0' || *v > '9') return 0;
    int level = atoi(v);
    if (level < 0 || level > 100) return 0;
    out->level = level;
    if ((v = power_log_field(last, end, "isPsuConnected:")))
        out->psu = strncmp(v, "true", 4) == 0 ? 1 : 0;
    if ((v = power_log_field(last, end, "chargingState:"))) {
        out->charging = strncmp(v, "charging", 8) == 0 && (v[8] == ',' || v[8] == ' ' || v[8] == '}') ? 1 : 0;
        out->full = strncmp(v, "fullyCharged", 12) == 0 ? 1 : 0;
    }
    return 1;
}

#endif /* POWER_STATE_H */

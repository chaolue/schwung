/* rtpmidi_proto.c — see rtpmidi_proto.h. Pure; host-tested. */
#include "rtpmidi_proto.h"

#include <string.h>

static void put16(uint8_t *p, uint16_t v) { p[0] = (uint8_t)(v >> 8); p[1] = (uint8_t)v; }
static void put32(uint8_t *p, uint32_t v) {
    p[0] = (uint8_t)(v >> 24); p[1] = (uint8_t)(v >> 16);
    p[2] = (uint8_t)(v >> 8);  p[3] = (uint8_t)v;
}
static void put64(uint8_t *p, uint64_t v) {
    put32(p, (uint32_t)(v >> 32)); put32(p + 4, (uint32_t)v);
}
static uint16_t get16(const uint8_t *p) { return (uint16_t)((p[0] << 8) | p[1]); }
static uint32_t get32(const uint8_t *p) {
    return ((uint32_t)p[0] << 24) | ((uint32_t)p[1] << 16) |
           ((uint32_t)p[2] << 8) | (uint32_t)p[3];
}
static uint64_t get64(const uint8_t *p) {
    return ((uint64_t)get32(p) << 32) | get32(p + 4);
}

/* ---- AppleMIDI ---------------------------------------------------------- */

int rtpmidi_cmd_parse(const uint8_t *buf, int len, rtpmidi_cmd_t *out)
{
    if (!buf || !out || len < 4 || buf[0] != 0xFF || buf[1] != 0xFF) return 0;
    memset(out, 0, sizeof(*out));
    out->cmd = get16(buf + 2);
    switch (out->cmd) {
    case RTPMIDI_CMD_IN: case RTPMIDI_CMD_OK:
    case RTPMIDI_CMD_NO: case RTPMIDI_CMD_BY: {
        if (len < 16) return 0;
        out->version = get32(buf + 4);
        out->token   = get32(buf + 8);
        out->ssrc    = get32(buf + 12);
        int n = len - 16;
        if (n > RTPMIDI_NAME_MAX - 1) n = RTPMIDI_NAME_MAX - 1;
        if (n > 0) memcpy(out->name, buf + 16, (size_t)n);
        out->name[n > 0 ? n : 0] = '\0';
        return 1;
    }
    case RTPMIDI_CMD_CK:
        if (len < 36) return 0;
        out->ssrc  = get32(buf + 4);
        out->count = buf[8];
        out->ts[0] = get64(buf + 12);
        out->ts[1] = get64(buf + 20);
        out->ts[2] = get64(buf + 28);
        return 1;
    case RTPMIDI_CMD_RS:
        if (len < 12) return 0;
        out->ssrc = get32(buf + 4);
        out->seq  = get16(buf + 8);
        return 1;
    default:
        return 0;
    }
}

int rtpmidi_cmd_build_session(uint8_t *buf, int cap, uint16_t cmd,
                              uint32_t token, uint32_t ssrc, const char *name)
{
    int nlen = 0;
    /* Only an invitation and its acceptance carry a name. */
    if (name && (cmd == RTPMIDI_CMD_IN || cmd == RTPMIDI_CMD_OK)) {
        nlen = (int)strlen(name);
        if (nlen > RTPMIDI_NAME_MAX - 1) nlen = RTPMIDI_NAME_MAX - 1;
        nlen += 1;                              /* the NUL travels */
    }
    if (cap < 16 + nlen) return 0;
    buf[0] = 0xFF; buf[1] = 0xFF;
    put16(buf + 2, cmd);
    put32(buf + 4, 2);                          /* protocol version */
    put32(buf + 8, token);
    put32(buf + 12, ssrc);
    if (nlen) {
        memcpy(buf + 16, name, (size_t)(nlen - 1));
        buf[16 + nlen - 1] = '\0';
    }
    return 16 + nlen;
}

int rtpmidi_cmd_build_ck(uint8_t *buf, int cap, uint32_t ssrc, uint8_t count,
                         const uint64_t ts[3])
{
    if (cap < 36) return 0;
    buf[0] = 0xFF; buf[1] = 0xFF;
    put16(buf + 2, RTPMIDI_CMD_CK);
    put32(buf + 4, ssrc);
    buf[8] = count; buf[9] = buf[10] = buf[11] = 0;
    put64(buf + 12, ts[0]);
    put64(buf + 20, ts[1]);
    put64(buf + 28, ts[2]);
    return 36;
}

int rtpmidi_cmd_build_rs(uint8_t *buf, int cap, uint32_t ssrc, uint16_t seq)
{
    if (cap < 12) return 0;
    buf[0] = 0xFF; buf[1] = 0xFF;
    put16(buf + 2, RTPMIDI_CMD_RS);
    put32(buf + 4, ssrc);
    put16(buf + 8, seq);
    buf[10] = buf[11] = 0;
    return 12;
}

/* ---- RTP-MIDI build ----------------------------------------------------- */

void rtpmidi_pkt_begin(rtpmidi_pkt_t *p)
{
    p->len = RTPMIDI_PKT_HDR;
    p->ncmd = 0;
}

int rtpmidi_pkt_add(rtpmidi_pkt_t *p, const uint8_t *msg, int len)
{
    if (!msg || len <= 0) return 1;             /* nothing to add */
    int need = len + (p->ncmd ? 1 : 0);         /* delta 0 before 2nd+ */
    if (p->len + need > RTPMIDI_PKT_MAX) return 0;
    if (p->ncmd) p->buf[p->len++] = 0x00;
    memcpy(p->buf + p->len, msg, (size_t)len);
    p->len += len;
    p->ncmd++;
    return 1;
}

int rtpmidi_pkt_finish(rtpmidi_pkt_t *p, uint16_t seq, uint32_t ts,
                       uint32_t ssrc)
{
    if (p->ncmd == 0) return 0;
    int list = p->len - RTPMIDI_PKT_HDR;
    p->buf[0] = 0x80;                           /* V=2 */
    p->buf[1] = 0x61;                           /* PT 97 */
    put16(p->buf + 2, seq);
    put32(p->buf + 4, ts);
    put32(p->buf + 8, ssrc);
    /* B=1 (12-bit length), J=0, Z=0 (first command has no delta), P=0. */
    p->buf[12] = (uint8_t)(0x80 | ((list >> 8) & 0x0F));
    p->buf[13] = (uint8_t)(list & 0xFF);
    return p->len;
}

int rtpmidi_sysex_segment(const uint8_t *sx, int len, int *pos, uint8_t *out)
{
    int n = 0;
    int first = (*pos == 0);
    int remain = len - *pos;
    int take = remain > RTPMIDI_SYSEX_CHUNK ? RTPMIDI_SYSEX_CHUNK : remain;
    int last = (take == remain);
    /* A continuation segment opens with F7; the first opens with the
     * message's own F0, which is already sx[0]. */
    if (!first) out[n++] = 0xF7;
    memcpy(out + n, sx + *pos, (size_t)take);
    n += take;
    *pos += take;
    /* A segment that is not the last closes with F0 ("more follows"); the
     * last closes with the message's own F7, already copied. */
    if (!last) out[n++] = 0xF0;
    return n;
}

/* ---- RTP-MIDI parse ----------------------------------------------------- */

static int chan_data_len(uint8_t status)
{
    uint8_t t = status & 0xF0;
    return (t == 0xC0 || t == 0xD0) ? 1 : 2;
}

int rtpmidi_rtp_parse(const uint8_t *buf, int len, rtpmidi_rx_t *rx,
                      uint16_t *seq, uint32_t *ssrc,
                      rtpmidi_bytes_fn emit, void *ctx)
{
    if (!buf || len < 13) return 0;
    if ((buf[0] & 0xC0) != 0x80) return 0;      /* RTP version 2 */
    if ((buf[1] & 0x7F) != 0x61) return 0;      /* payload type 97 */
    if (seq)  *seq  = get16(buf + 2);
    if (ssrc) *ssrc = get32(buf + 8);

    int off = 12 + 4 * (buf[0] & 0x0F);         /* CSRC list */
    if (buf[0] & 0x10) {                        /* header extension */
        if (off + 4 > len) return 1;
        off += 4 + 4 * get16(buf + off + 2);
    }
    if (off >= len) return 1;

    uint8_t flags = buf[off++];
    int list = flags & 0x0F;
    if (flags & 0x80) {
        if (off >= len) return 1;
        list = (list << 8) | buf[off++];
    }
    if (off + list > len) list = len - off;
    const uint8_t *p = buf + off;
    const uint8_t *end = p + list;
    int first = 1;
    uint8_t running = 0;

    while (p < end) {
        if (!first || (flags & 0x20)) {         /* delta time, 1-4 bytes */
            int k = 0;
            while (p < end && k < 4) {
                uint8_t b = *p++;
                k++;
                if (!(b & 0x80)) break;
            }
            if (p >= end) break;
        }
        first = 0;

        uint8_t b = *p;
        if (b >= 0xF8) {                        /* realtime */
            p++;
            if (emit) emit(ctx, &b, 1);
            continue;
        }
        if (b == 0xF0 || b == 0xF7) {           /* SysEx, whole or segment */
            int is_start = (b == 0xF0);
            const uint8_t *d = ++p;
            while (p < end && !(*p & 0x80)) p++;
            int dn = (int)(p - d);
            uint8_t term = (p < end) ? *p++ : 0xF0;   /* cut short = open */
            int deliver = is_start || (rx && rx->sysex_open);
            if (deliver && emit) {
                if (is_start) { uint8_t f0 = 0xF0; emit(ctx, &f0, 1); }
                if (dn) emit(ctx, d, dn);
                /* F7 ends it; F4 cancels it — either way the stream's
                 * message is closed. F0 means another segment follows. */
                if (term != 0xF0) { uint8_t f7 = 0xF7; emit(ctx, &f7, 1); }
            }
            if (rx) rx->sysex_open = (term == 0xF0) && deliver;
            running = 0;
            continue;
        }
        if (b >= 0xF0) {                        /* system common */
            int dn = (b == 0xF2) ? 2 : (b == 0xF1 || b == 0xF3) ? 1 : 0;
            if (p + 1 + dn > end) break;
            if (emit && b != 0xF4 && b != 0xF5) emit(ctx, p, 1 + dn);
            p += 1 + dn;
            running = 0;
            continue;
        }
        uint8_t msg[3];
        if (b & 0x80) {
            running = b;
            p++;
        } else if (!running) {
            break;                              /* data with no status */
        }
        int dn = chan_data_len(running);
        if (p + dn > end) break;
        msg[0] = running;
        msg[1] = p[0];
        msg[2] = dn > 1 ? p[1] : 0;
        if ((msg[1] & 0x80) || (msg[2] & 0x80)) break;
        p += dn;
        if (emit) emit(ctx, msg, 1 + dn);
    }
    return 1;
}

/* ---- USB-MIDI -> messages ----------------------------------------------- */

static void asm_sx_append(rtpmidi_usb_asm_t *a, const uint8_t *b, int n)
{
    if (a->sx_len + n > RTPMIDI_SYSEX_MAX) { a->overflow = 1; return; }
    memcpy(a->sx + a->sx_len, b, (size_t)n);
    a->sx_len += n;
}

static int asm_sx_finish(rtpmidi_usb_asm_t *a, const uint8_t **msg, int *len)
{
    int ok = a->in_sysex && !a->overflow && a->sx_len >= 2 &&
             a->sx[0] == 0xF0 && a->sx[a->sx_len - 1] == 0xF7;
    if (ok) { *msg = a->sx; *len = a->sx_len; }
    a->in_sysex = 0;
    a->overflow = 0;
    a->sx_len = 0;          /* the caller copies before the next feed */
    return ok;
}

int rtpmidi_usb_to_msg(rtpmidi_usb_asm_t *a, const uint8_t pkt[4],
                       const uint8_t **msg, int *len)
{
    uint8_t cin = pkt[0] & 0x0F;
    int n;
    switch (cin) {
    case 0x4:                                   /* SysEx start / continue */
        if (pkt[1] == 0xF0) { a->in_sysex = 1; a->sx_len = 0; a->overflow = 0; }
        if (a->in_sysex) asm_sx_append(a, pkt + 1, 3);
        return 0;
    case 0x5:                                   /* 1-byte end, or sys common */
        if (a->in_sysex || pkt[1] == 0xF7) {
            if (a->in_sysex) asm_sx_append(a, pkt + 1, 1);
            return asm_sx_finish(a, msg, len);
        }
        n = 1;
        break;
    case 0x6:
    case 0x7:
        n = (cin == 0x6) ? 2 : 3;
        if (pkt[1] == 0xF0) { a->in_sysex = 1; a->sx_len = 0; a->overflow = 0; }
        if (!a->in_sysex) return 0;
        asm_sx_append(a, pkt + 1, n);
        return asm_sx_finish(a, msg, len);
    case 0x2: n = 2; break;
    case 0x3: n = 3; break;
    case 0xC: case 0xD: n = 2; break;
    case 0x8: case 0x9: case 0xA: case 0xB: case 0xE: n = 3; break;
    case 0xF: n = 1; break;
    default: return 0;                          /* 0, 1: reserved */
    }
    if (!(pkt[1] & 0x80)) return 0;             /* not a status byte */
    memcpy(a->one, pkt + 1, 3);
    *msg = a->one;
    *len = n;
    return 1;
}

/* ---- MIDI bytes -> USB-MIDI --------------------------------------------- */

static void enc_emit(rtpmidi_usb_fn emit, void *ctx, uint8_t cable, uint8_t cin,
                     uint8_t b0, uint8_t b1, uint8_t b2)
{
    uint8_t pkt[4] = { (uint8_t)((cable << 4) | cin), b0, b1, b2 };
    if (emit) emit(ctx, pkt);
}

void rtpmidi_bytes_to_usb(rtpmidi_usb_enc_t *e, uint8_t cable,
                          const uint8_t *bytes, int n,
                          rtpmidi_usb_fn emit, void *ctx)
{
    for (int i = 0; i < n; i++) {
        uint8_t b = bytes[i];

        if (b >= 0xF8) {                        /* realtime: never disturbs */
            enc_emit(emit, ctx, cable, 0xF, b, 0, 0);
            continue;
        }

        if (e->in_sysex) {
            if (b == 0xF7) {
                e->sx[e->sxn++] = b;
                enc_emit(emit, ctx, cable, (uint8_t)(0x4 + e->sxn),
                         e->sx[0], e->sxn > 1 ? e->sx[1] : 0,
                         e->sxn > 2 ? e->sx[2] : 0);
                e->in_sysex = 0; e->sxn = 0;
                continue;
            }
            if (!(b & 0x80)) {
                e->sx[e->sxn++] = b;
                if (e->sxn == 3) {
                    enc_emit(emit, ctx, cable, 0x4, e->sx[0], e->sx[1], e->sx[2]);
                    e->sxn = 0;
                }
                continue;
            }
            /* A status byte inside a SysEx abandons it. Close what has been
             * sent so the receiver is not left assembling forever. */
            e->sx[e->sxn++] = 0xF7;
            enc_emit(emit, ctx, cable, (uint8_t)(0x4 + e->sxn),
                     e->sx[0], e->sxn > 1 ? e->sx[1] : 0,
                     e->sxn > 2 ? e->sx[2] : 0);
            e->in_sysex = 0; e->sxn = 0;
            /* fall through and handle `b` as a status byte */
        }

        if (b == 0xF0) {
            e->in_sysex = 1; e->sx[0] = b; e->sxn = 1; e->status = 0;
            continue;
        }
        if (b >= 0xF0) {                        /* system common */
            e->have = 0;
            if (b == 0xF2)                    { e->status = b; e->need = 2; }
            else if (b == 0xF1 || b == 0xF3)  { e->status = b; e->need = 1; }
            else {
                e->status = 0;
                if (b == 0xF6) enc_emit(emit, ctx, cable, 0x5, b, 0, 0);
            }
            continue;
        }
        if (b & 0x80) {                         /* channel status */
            e->status = b;
            e->have = 0;
            e->need = chan_data_len(b);
            continue;
        }
        if (!e->status) continue;               /* stray data byte */
        e->data[e->have++] = b;
        if (e->have < e->need) continue;
        e->have = 0;
        if (e->status >= 0xF0) {
            enc_emit(emit, ctx, cable, e->need == 2 ? 0x3 : 0x2, e->status,
                     e->data[0], e->need == 2 ? e->data[1] : 0);
            e->status = 0;
        } else {
            enc_emit(emit, ctx, cable, (uint8_t)(e->status >> 4), e->status,
                     e->data[0], e->need == 2 ? e->data[1] : 0);
        }
    }
}

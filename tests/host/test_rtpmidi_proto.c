/*
 * RTP-MIDI wire format and the shim-side rings, without a device or a socket.
 *
 * The properties worth proving are the ones that are silent when wrong:
 *   1. a message survives USB-MIDI -> RTP -> USB-MIDI byte for byte, for
 *      every packet shape (channel, 2-byte, system common, realtime, SysEx of
 *      each tail length);
 *   2. a foreign sender's RUNNING STATUS, DELTA TIMES and JOURNAL are all
 *      walked correctly -- we emit none of them, so only a hand-built packet
 *      exercises that code;
 *   3. a SysEx too big for one datagram is segmented and comes back whole;
 *   4. the MIDI_OUT tap takes cable 2 only, and does NOT re-send a packet the
 *      post-transfer copy-back left in its slot;
 *   5. delivery into MIDI_IN uses the first EMPTY slot (a zeroed slot is a
 *      terminator), forces cable 2, and never half-delivers.
 */
#include <stdio.h>
#include <string.h>

#include "rtpmidi_proto.h"
#include "rtpmidi_shm.h"

static int fails = 0;
#define CHECK(cond, ...) do { if (!(cond)) { fails++; \
    printf("FAIL %s:%d: ", __FILE__, __LINE__); printf(__VA_ARGS__); printf("\n"); } } while (0)

/* ---- collectors ---- */
static uint8_t g_bytes[40000]; static int g_nbytes;
static void collect_bytes(void *ctx, const uint8_t *b, int n) {
    (void)ctx; memcpy(g_bytes + g_nbytes, b, (size_t)n); g_nbytes += n;
}
static uint8_t g_usb[20000][4]; static int g_nusb;
static void collect_usb(void *ctx, const uint8_t pkt[4]) {
    (void)ctx; memcpy(g_usb[g_nusb++], pkt, 4);
}

static void test_session_cmds(void)
{
    uint8_t b[128]; rtpmidi_cmd_t c;
    int n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_IN, 0x11223344, 0xAABBCCDD, "Schwung");
    CHECK(n == 16 + 8, "IN length %d", n);
    CHECK(b[0] == 0xFF && b[1] == 0xFF && b[2] == 'I' && b[3] == 'N', "IN signature");
    CHECK(rtpmidi_cmd_parse(b, n, &c), "IN parses");
    CHECK(c.cmd == RTPMIDI_CMD_IN && c.version == 2 && c.token == 0x11223344 &&
          c.ssrc == 0xAABBCCDD && !strcmp(c.name, "Schwung"), "IN fields");

    n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_BY, 1, 2, "ignored");
    CHECK(n == 16, "BY carries no name (%d)", n);

    uint64_t ts[3] = { 0x0102030405060708ull, 9, 10 };
    n = rtpmidi_cmd_build_ck(b, sizeof(b), 7, 1, ts);
    CHECK(n == 36 && rtpmidi_cmd_parse(b, n, &c), "CK parses");
    CHECK(c.cmd == RTPMIDI_CMD_CK && c.ssrc == 7 && c.count == 1 &&
          c.ts[0] == ts[0] && c.ts[1] == 9 && c.ts[2] == 10, "CK fields");

    n = rtpmidi_cmd_build_rs(b, sizeof(b), 7, 0xBEEF);
    CHECK(n == 12 && rtpmidi_cmd_parse(b, n, &c) && c.seq == 0xBEEF, "RS");

    /* An RTP data packet is not a command, and a truncated command is not one. */
    uint8_t rtp[13] = { 0x80, 0x61 };
    CHECK(!rtpmidi_cmd_parse(rtp, sizeof(rtp), &c), "RTP is not a command");
    n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_IN, 1, 2, "x");
    CHECK(!rtpmidi_cmd_parse(b, 10, &c), "truncated IN rejected");
}

/* Push USB packets through assembler -> RTP packet(s) -> parser -> USB. */
static void roundtrip(const uint8_t (*in)[4], int n_in)
{
    static rtpmidi_usb_asm_t a; static rtpmidi_pkt_t pkt;
    rtpmidi_rx_t rx = {0}; rtpmidi_usb_enc_t enc = {0};
    memset(&a, 0, sizeof(a));
    g_nusb = 0;
    rtpmidi_pkt_begin(&pkt);
    uint16_t seq = 100;
    for (int i = 0; i <= n_in; i++) {
        const uint8_t *msg = NULL; int len = 0;
        int have = (i < n_in) && rtpmidi_usb_to_msg(&a, in[i], &msg, &len);
        int big = have && len > RTPMIDI_PKT_MAX - RTPMIDI_PKT_HDR - 1;
        if (i == n_in || big || (have && !rtpmidi_pkt_add(&pkt, msg, len))) {
            int n = rtpmidi_pkt_finish(&pkt, seq, 0, 0x5555);
            if (n) {
                uint16_t s = 0; uint32_t ssrc = 0;
                g_nbytes = 0;
                CHECK(rtpmidi_rtp_parse(pkt.buf, n, &rx, &s, &ssrc, collect_bytes, NULL), "parses");
                CHECK(s == seq && ssrc == 0x5555, "seq/ssrc");
                rtpmidi_bytes_to_usb(&enc, 2, g_bytes, g_nbytes, collect_usb, NULL);
                seq++;
            }
            rtpmidi_pkt_begin(&pkt);
            if (have && !big) rtpmidi_pkt_add(&pkt, msg, len);
        }
        if (big) {
            uint8_t seg[RTPMIDI_SYSEX_CHUNK + 2]; int pos = 0;
            while (pos < len) {
                int sn = rtpmidi_sysex_segment(msg, len, &pos, seg);
                rtpmidi_pkt_begin(&pkt);
                CHECK(rtpmidi_pkt_add(&pkt, seg, sn), "segment fits");
                int n = rtpmidi_pkt_finish(&pkt, seq++, 0, 0x5555);
                g_nbytes = 0;
                rtpmidi_rtp_parse(pkt.buf, n, &rx, NULL, NULL, collect_bytes, NULL);
                rtpmidi_bytes_to_usb(&enc, 2, g_bytes, g_nbytes, collect_usb, NULL);
            }
            rtpmidi_pkt_begin(&pkt);
        }
    }
    CHECK(g_nusb == n_in, "packet count %d != %d", g_nusb, n_in);
    for (int i = 0; i < n_in && i < g_nusb; i++)
        CHECK(!memcmp(g_usb[i], in[i], 4), "packet %d: %02x %02x %02x %02x != %02x %02x %02x %02x",
              i, g_usb[i][0], g_usb[i][1], g_usb[i][2], g_usb[i][3],
              in[i][0], in[i][1], in[i][2], in[i][3]);
}

static void test_roundtrip_shapes(void)
{
    static const uint8_t shapes[][4] = {
        {0x29, 0x90, 60, 100}, {0x28, 0x80, 60, 0},  {0x2B, 0xB3, 7, 127},
        {0x2C, 0xC1, 5, 0},    {0x2D, 0xD2, 99, 0},  {0x2E, 0xE0, 0, 64},
        {0x2A, 0xA5, 61, 3},   {0x2F, 0xF8, 0, 0},   {0x2F, 0xFA, 0, 0},
        {0x22, 0xF1, 0x23, 0}, {0x23, 0xF2, 1, 2},   {0x22, 0xF3, 9, 0},
        {0x25, 0xF6, 0, 0},
        /* SysEx with each tail length: F0 7D F7 / F0 1 2 3 F7 / F0 1 2 3 4 F7 */
        {0x27, 0xF0, 0x7D, 0xF7},
        {0x24, 0xF0, 1, 2}, {0x26, 3, 0xF7, 0},
        {0x24, 0xF0, 1, 2}, {0x27, 3, 4, 0xF7},
        {0x24, 0xF0, 1, 2}, {0x24, 3, 4, 5}, {0x25, 0xF7, 0, 0},
        {0x26, 0xF0, 0xF7, 0},
        {0x29, 0x90, 60, 100},
    };
    roundtrip(shapes, (int)(sizeof(shapes) / sizeof(shapes[0])));

    /* Enough notes to spill over one datagram. */
    static uint8_t many[2000][4];
    for (int i = 0; i < 2000; i++) {
        many[i][0] = 0x29; many[i][1] = (uint8_t)(0x90 | (i & 0x0F));
        many[i][2] = (uint8_t)(i & 0x7F); many[i][3] = (uint8_t)(1 + (i % 127));
    }
    roundtrip(many, 2000);
}

static void test_big_sysex_segments(void)
{
    /* 5000 data bytes: five segments. Packets of 3, ending exactly on F7. */
    static uint8_t sx[5002]; static uint8_t usb[1700][4];
    sx[0] = 0xF0;
    for (int i = 1; i <= 5000; i++) sx[i] = (uint8_t)((i * 7) & 0x7F);
    sx[5001] = 0xF7;
    int n = 0, pos = 0, total = 5002;
    while (total - pos > 3) {
        usb[n][0] = 0x24; memcpy(usb[n] + 1, sx + pos, 3); n++; pos += 3;
    }
    int tail = total - pos;
    usb[n][0] = (uint8_t)(0x24 + tail); usb[n][1] = usb[n][2] = usb[n][3] = 0;
    memcpy(usb[n] + 1, sx + pos, (size_t)tail); n++;
    roundtrip(usb, n);

    /* A middle segment arriving with no open SysEx (we missed the start)
     * must emit nothing rather than a headless message. */
    rtpmidi_pkt_t pkt; rtpmidi_rx_t rx = {0};
    uint8_t seg[] = { 0xF7, 1, 2, 3, 0xF0 };
    rtpmidi_pkt_begin(&pkt); rtpmidi_pkt_add(&pkt, seg, sizeof(seg));
    int len = rtpmidi_pkt_finish(&pkt, 1, 0, 1);
    g_nbytes = 0;
    rtpmidi_rtp_parse(pkt.buf, len, &rx, NULL, NULL, collect_bytes, NULL);
    CHECK(g_nbytes == 0 && !rx.sysex_open, "orphan segment dropped (%d)", g_nbytes);
}

static void test_foreign_packet(void)
{
    /* Short header (B=0), Z=1, J=1: delta, note on, delta, RUNNING-STATUS
     * note on, 2-byte delta, realtime, delta, CC -- then a journal the walk
     * must not read as MIDI. */
    uint8_t p[64]; int n = 0;
    uint8_t hdr[12] = { 0x80, 0xE1, 0x00, 0x2A, 0,0,0,0, 0,0,0,9 };
    memcpy(p, hdr, 12); n = 12;
    int lenpos = n++;
    int start = n;
    p[n++] = 0x05; p[n++] = 0x90; p[n++] = 60; p[n++] = 100;
    p[n++] = 0x00;                 p[n++] = 64; p[n++] = 90;       /* running */
    p[n++] = 0x81; p[n++] = 0x00;  p[n++] = 0xF8;
    p[n++] = 0x00; p[n++] = 0xB1; p[n++] = 7; p[n++] = 99;
    p[lenpos] = (uint8_t)(0x40 | 0x20 | (n - start));              /* J | Z | LEN */
    p[n++] = 0x90; p[n++] = 0x40; p[n++] = 0x7F;                   /* "journal" */

    rtpmidi_rx_t rx = {0}; uint16_t seq = 0; uint32_t ssrc = 0;
    g_nbytes = 0;
    CHECK(rtpmidi_rtp_parse(p, n, &rx, &seq, &ssrc, collect_bytes, NULL), "foreign parses");
    CHECK(seq == 0x2A && ssrc == 9, "foreign seq/ssrc");
    const uint8_t want[] = { 0x90,60,100, 0x90,64,90, 0xF8, 0xB1,7,99 };
    CHECK(g_nbytes == (int)sizeof(want) && !memcmp(g_bytes, want, sizeof(want)),
          "foreign stream (%d bytes)", g_nbytes);

    uint8_t junk[16] = { 0x00 };
    CHECK(!rtpmidi_rtp_parse(junk, sizeof(junk), &rx, NULL, NULL, collect_bytes, NULL), "not RTP");
}

static void test_tap(void)
{
    static rtpmidi_shm_t shm; uint8_t prev[RTPMIDI_TAP_BYTES] = {0};
    uint8_t out[RTPMIDI_TAP_BYTES] = {0}, got[4];
    const uint8_t led[4]  = { 0x09, 0x90, 68, 120 };   /* cable 0: a pad LED */
    const uint8_t note[4] = { 0x29, 0x90, 60, 100 };
    const uint8_t off[4]  = { 0x28, 0x80, 60, 0 };
    memcpy(out, led, 4); memcpy(out + 4, note, 4);

    CHECK(rtpmidi_tap_midi_out(&shm, prev, out) == 0, "inactive: nothing queued");
    out[4] = 0x29; out[6] = 62;                        /* a new note, same slot */
    shm.out_active = 1;
    CHECK(rtpmidi_tap_midi_out(&shm, prev, out) == 1, "cable 2 only");
    CHECK(rtpmidi_ring_peek_at(&shm.out, got, 0) && got[2] == 62, "queued the note");
    rtpmidi_ring_pop(&shm.out);

    /* Copy-back left it there: the same bytes in the same slot are not sent again. */
    CHECK(rtpmidi_tap_midi_out(&shm, prev, out) == 0, "lingering packet not re-sent");
    memcpy(out + 4, off, 4);
    CHECK(rtpmidi_tap_midi_out(&shm, prev, out) == 1, "a changed slot is sent");
    memset(out, 0, sizeof(out));
    CHECK(rtpmidi_tap_midi_out(&shm, prev, out) == 0, "empty region");
    memcpy(out + 4, off, 4);
    CHECK(rtpmidi_tap_midi_out(&shm, prev, out) == 1, "same packet after a gap IS sent");

    /* Full ring drops, never blocks or wraps. */
    rtpmidi_ring_flush(&shm.out);
    for (unsigned i = 0; i < RTPMIDI_RING_PACKETS; i++) CHECK(rtpmidi_ring_push(&shm.out, note), "fill");
    CHECK(!rtpmidi_ring_push(&shm.out, note) && shm.out.drops == 1, "full ring drops");
}

static void test_deliver(void)
{
    static rtpmidi_shm_t shm; uint8_t sh[248] = {0}, hw[248] = {0};
    const uint8_t hwevt[8] = { 0x09, 0x90, 70, 100, 1, 2, 3, 4 };
    memcpy(sh, hwevt, 8); memcpy(hw, hwevt, 8);
    const uint8_t a[4] = { 0x09, 0x90, 60, 100 };      /* daemon wrote cable 0?! */
    const uint8_t b[4] = { 0x28, 0x80, 60, 0 };
    rtpmidi_ring_push(&shm.in, a); rtpmidi_ring_push(&shm.in, b);

    /* Peek half: placed, not consumed. */
    CHECK(rtpmidi_deliver_midi_in(&shm, sh, hw, 248, 8, 0) == 2, "peek places 2");
    CHECK(!memcmp(sh, hwevt, 8), "hardware event untouched");
    CHECK(sh[8] == 0x29 && sh[9] == 0x90 && sh[16] == 0x28, "first empty slots, cable forced to 2");
    CHECK(shm.in.tail == 0, "peek does not consume");

    /* The ioctl wipes it; the pop half delivers the same two for real. */
    memset(sh, 0, sizeof(sh)); memset(hw, 0, sizeof(hw));
    memcpy(sh, hwevt, 8); memcpy(hw, hwevt, 8);
    CHECK(rtpmidi_deliver_midi_in(&shm, sh, hw, 248, 2, 1) == 2, "pop places 2");
    CHECK(!memcmp(sh, hw, 248), "both buffers agree");
    CHECK(sh[12] == 0 && sh[13] == 0 && sh[14] == 0 && sh[15] == 0, "timestamp zeroed");
    CHECK(shm.in.tail == 2, "pop consumes");
    CHECK(sh[24] == 0, "nothing past the delivered events");

    /* hw full: nothing lands in shadow alone, and the packet stays queued. */
    rtpmidi_ring_push(&shm.in, b);
    memset(hw, 0x29, sizeof(hw)); memset(sh, 0, sizeof(sh));
    CHECK(rtpmidi_deliver_midi_in(&shm, sh, hw, 248, 8, 1) == 0, "no half delivery");
    CHECK(sh[0] == 0 && shm.in.tail == 2, "still queued");
}

int main(void)
{
    test_session_cmds();
    test_roundtrip_shapes();
    test_big_sysex_segments();
    test_foreign_packet();
    test_tap();
    test_deliver();
    if (fails) { printf("test_rtpmidi_proto: %d FAILED\n", fails); return 1; }
    printf("test_rtpmidi_proto: PASS\n");
    return 0;
}

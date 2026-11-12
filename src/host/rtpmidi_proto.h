/* rtpmidi_proto.h — RTP-MIDI (RFC 6295) and the AppleMIDI session protocol,
 * as pure byte-shuffling.
 *
 * No sockets, no clock, no allocation, no globals: every function works on
 * caller-owned buffers, so tests/host/test_rtpmidi_proto.c runs the whole
 * wire format natively. The sockets and the session table live in
 * rtpmidi_daemon.c.
 *
 * WHAT IS AND IS NOT IMPLEMENTED
 *
 *   - AppleMIDI IN / OK / NO / BY / CK / RS, both directions.
 *   - The MIDI command section: delta times, running status, realtime bytes,
 *     and segmented SysEx in both directions.
 *   - NOT the recovery journal. We send J=0 and skip a received journal (the
 *     command section's LEN says where it ends). On the links this is for —
 *     USB-Ethernet and a local network — a lost datagram is rare, and a
 *     journal-less stream degrades to "that one message was lost", exactly
 *     like DIN. Apple's and rtpmidid's receivers both accept J=0.
 */
#ifndef RTPMIDI_PROTO_H
#define RTPMIDI_PROTO_H

#include <stdint.h>

/* ---- AppleMIDI session commands ---------------------------------------- */

#define RTPMIDI_CMD_IN 0x494E   /* invitation */
#define RTPMIDI_CMD_OK 0x4F4B   /* invitation accepted */
#define RTPMIDI_CMD_NO 0x4E4F   /* invitation rejected */
#define RTPMIDI_CMD_BY 0x4259   /* end session */
#define RTPMIDI_CMD_CK 0x434B   /* clock sync */
#define RTPMIDI_CMD_RS 0x5253   /* receiver feedback */

#define RTPMIDI_NAME_MAX 64

typedef struct {
    uint16_t cmd;
    uint32_t version;               /* IN/OK/NO/BY */
    uint32_t token;                 /* IN/OK/NO/BY */
    uint32_t ssrc;                  /* all */
    char     name[RTPMIDI_NAME_MAX];/* IN/OK, may be empty */
    uint8_t  count;                 /* CK */
    uint64_t ts[3];                 /* CK, 100 us units */
    uint16_t seq;                   /* RS */
} rtpmidi_cmd_t;

/* 1 if `buf` is a well-formed AppleMIDI command (and fills `out`), else 0. */
int rtpmidi_cmd_parse(const uint8_t *buf, int len, rtpmidi_cmd_t *out);

/* Builders return the packet length, or 0 if `cap` is too small. */
int rtpmidi_cmd_build_session(uint8_t *buf, int cap, uint16_t cmd,
                              uint32_t token, uint32_t ssrc, const char *name);
int rtpmidi_cmd_build_ck(uint8_t *buf, int cap, uint32_t ssrc, uint8_t count,
                         const uint64_t ts[3]);
int rtpmidi_cmd_build_rs(uint8_t *buf, int cap, uint32_t ssrc, uint16_t seq);

/* ---- RTP-MIDI data packets --------------------------------------------- */

#define RTPMIDI_PKT_MAX      1400   /* whole datagram, under any sane MTU */
#define RTPMIDI_PKT_HDR      14     /* 12 RTP + 2 long MIDI section header */
#define RTPMIDI_SYSEX_CHUNK  1024   /* data bytes per SysEx segment */

typedef struct {
    uint8_t buf[RTPMIDI_PKT_MAX];
    int     len;                    /* bytes used, including the header room */
    int     ncmd;
} rtpmidi_pkt_t;

void rtpmidi_pkt_begin(rtpmidi_pkt_t *p);
/* Append one complete MIDI message (status byte included). Returns 1, or 0
 * when it does not fit — finish and send the packet, begin again, re-add. */
int  rtpmidi_pkt_add(rtpmidi_pkt_t *p, const uint8_t *msg, int len);
/* Write the headers. Returns the datagram length (0 if no command). */
int  rtpmidi_pkt_finish(rtpmidi_pkt_t *p, uint16_t seq, uint32_t ts,
                        uint32_t ssrc);

/* Cut the next segment of a complete SysEx message (`sx` runs F0 .. F7) for a
 * message too large for one packet. `*pos` starts at 0 and is advanced; call
 * until it equals `len`. Each result is one MIDI command for rtpmidi_pkt_add
 * and wants a packet of its own. `out` must hold RTPMIDI_SYSEX_CHUNK + 2.
 * Returns the segment length. */
int rtpmidi_sysex_segment(const uint8_t *sx, int len, int *pos, uint8_t *out);

typedef void (*rtpmidi_bytes_fn)(void *ctx, const uint8_t *bytes, int n);

/* Receiver state that must survive between packets: only "a segmented SysEx
 * is open". Zero-initialise per session. */
typedef struct {
    int sysex_open;
} rtpmidi_rx_t;

/* Parse one RTP-MIDI datagram, emitting the MIDI it carries as a plain byte
 * stream (every channel message with its status byte restored). Returns 1 if
 * it was an RTP-MIDI packet (fills *seq, *ssrc), 0 otherwise. A malformed
 * command list stops the walk; what was already emitted stands. */
int rtpmidi_rtp_parse(const uint8_t *buf, int len, rtpmidi_rx_t *rx,
                      uint16_t *seq, uint32_t *ssrc,
                      rtpmidi_bytes_fn emit, void *ctx);

/* ---- USB-MIDI packets <-> MIDI messages -------------------------------- */

#define RTPMIDI_SYSEX_MAX 16384

/* USB-MIDI packets -> complete MIDI messages (SysEx reassembled). */
typedef struct {
    uint8_t sx[RTPMIDI_SYSEX_MAX];
    int     sx_len;
    int     in_sysex;
    int     overflow;
    uint8_t one[3];
} rtpmidi_usb_asm_t;

/* Feed one packet. Returns 1 when a complete message is ready, pointing
 * `msg` / `len` at storage inside `a` that is valid until the next call. */
int rtpmidi_usb_to_msg(rtpmidi_usb_asm_t *a, const uint8_t pkt[4],
                       const uint8_t **msg, int *len);

/* MIDI byte stream -> USB-MIDI packets. */
typedef struct {
    uint8_t status;                 /* running status, 0 = none */
    uint8_t data[2];
    int     have, need;
    int     in_sysex;
    uint8_t sx[3];
    int     sxn;
} rtpmidi_usb_enc_t;

typedef void (*rtpmidi_usb_fn)(void *ctx, const uint8_t pkt[4]);

/* `cable` is the USB-MIDI cable nibble to stamp (2 = external). */
void rtpmidi_bytes_to_usb(rtpmidi_usb_enc_t *e, uint8_t cable,
                          const uint8_t *bytes, int n,
                          rtpmidi_usb_fn emit, void *ctx);

#endif /* RTPMIDI_PROTO_H */

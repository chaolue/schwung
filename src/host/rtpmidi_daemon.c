/* schwung-rtpmidi — network MIDI (RTP-MIDI / AppleMIDI) sidecar.
 *
 * Bridges the shim's /schwung-rtpmidi rings (rtpmidi_shm.h) to UDP. To the
 * rest of Schwung the network is a second EXTERNAL MIDI port: whatever goes
 * out of USB-A also goes to every connected session, and whatever a session
 * sends arrives as cable-2 MIDI_IN.
 *
 * It is a sidecar for the reason link-subscriber is: the shim's MIDI lives on
 * the SPI callback, where a socket call is not allowed. Started by
 * schwung-entry.sh BEFORE MoveOriginal, so it is an ordinary SCHED_OTHER
 * process and inherits nothing realtime.
 *
 * SESSIONS, BOTH WAYS ROUND
 *   - It LISTENS on <port> (control) and <port>+1 (data) on every interface —
 *     Wi-Fi and the USB-C Ethernet gadget alike — and accepts any invitation.
 *     macOS Audio MIDI Setup, rtpMIDI on Windows and rtpmidid can all connect
 *     by address, which is what is needed where Bonjour does not reach.
 *   - It INVITES each `peer=` in the config, retrying for ever, so a computer
 *     that only listens needs no clicking after a reboot of either end.
 *   There is no mDNS advertisement; connect by address.
 *
 * Config (/data/UserData/schwung/rtpmidi.conf, every key optional):
 *     name=Schwung            session name shown to the peer
 *     port=5004               control port; data is port+1
 *     peer=172.16.254.2:5004  invite this host (repeatable, up to 8)
 *     output=1                Move/Schwung -> network
 *     input=1                 network -> Move/Schwung
 *
 * See docs/RTPMIDI.md.
 */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <errno.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

#include "rtpmidi_proto.h"
#include "rtpmidi_shm.h"
#include "unified_log.h"

#define LOG_SRC "rtpmidi"

#define MAX_PEERS        8
#define DEFAULT_CONF     "/data/UserData/schwung/rtpmidi.conf"

/* All in milliseconds. */
#define INVITE_RETRY_MS  2000    /* between invitations to a silent peer */
#define INVITE_GIVEUP_MS 5000    /* a half-finished handshake starts over */
#define SYNC_PERIOD_MS   10000   /* initiator clock sync, as Apple does */
#define PEER_TIMEOUT_MS  60000   /* nothing heard: the session is gone */
#define RS_PERIOD_MS     1000    /* receiver feedback rate limit */

enum { P_FREE = 0, P_IDLE, P_INVITE_CTRL, P_INVITE_DATA, P_WAIT_DATA, P_UP };

typedef struct {
    int state;
    int initiator;                  /* a configured peer= ; never freed */
    struct sockaddr_in ctrl, data;
    uint32_t ssrc;                  /* theirs */
    uint32_t token;
    uint64_t last_rx_ms;
    uint64_t state_ms;              /* when `state` was entered */
    uint64_t last_sync_ms;
    uint64_t last_rs_ms;
    uint16_t seq_out;
    uint16_t last_seq_in;
    int rs_due;
    rtpmidi_rx_t rx;
    rtpmidi_usb_enc_t enc;
    char name[RTPMIDI_NAME_MAX];
} peer_t;

static peer_t g_peers[MAX_PEERS];
static int g_ctrl_fd = -1, g_data_fd = -1;
static uint32_t g_ssrc;
static char g_name[RTPMIDI_NAME_MAX] = "Schwung";
static int g_port = 5004;
static int g_output = 1, g_input = 1;
static rtpmidi_shm_t *g_shm;
static volatile sig_atomic_t g_quit;
static rtpmidi_usb_asm_t g_asm;

static uint64_t now_100us(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (uint64_t)ts.tv_sec * 10000u + (uint64_t)ts.tv_nsec / 100000u;
}
static uint64_t now_ms(void) { return now_100us() / 10u; }

static uint32_t rand32(void)
{
    uint32_t v = 0;
    int fd = open("/dev/urandom", O_RDONLY);
    if (fd >= 0) {
        if (read(fd, &v, sizeof(v)) != (ssize_t)sizeof(v)) v = 0;
        close(fd);
    }
    if (!v) v = (uint32_t)now_100us() ^ ((uint32_t)getpid() << 16) ^ 0xA5C39E1Du;
    return v;
}

static const char *addr_str(const struct sockaddr_in *a, char *buf, size_t n)
{
    char ip[INET_ADDRSTRLEN] = "?";
    inet_ntop(AF_INET, &a->sin_addr, ip, sizeof(ip));
    snprintf(buf, n, "%s:%d", ip, (int)ntohs(a->sin_port));
    return buf;
}

static void send_to(int fd, const struct sockaddr_in *to, const uint8_t *b, int n)
{
    if (n > 0) sendto(fd, b, (size_t)n, 0, (const struct sockaddr *)to, sizeof(*to));
}

static void peer_set_state(peer_t *p, int state)
{
    p->state = state;
    p->state_ms = now_ms();
}

static void publish_status(void)
{
    uint32_t up = 0;
    for (int i = 0; i < MAX_PEERS; i++)
        if (g_peers[i].state == P_UP) up++;
    __atomic_store_n(&g_shm->peers, up, __ATOMIC_RELAXED);
    __atomic_store_n(&g_shm->out_active, (up && g_output) ? 1u : 0u,
                     __ATOMIC_RELAXED);
}

static void peer_up(peer_t *p)
{
    char a[64];
    peer_set_state(p, P_UP);
    p->last_rx_ms = now_ms();
    p->last_sync_ms = 0;
    p->seq_out = (uint16_t)rand32();
    memset(&p->rx, 0, sizeof(p->rx));
    memset(&p->enc, 0, sizeof(p->enc));
    LOG_INFO(LOG_SRC, "session up: \"%s\" %s (%s)", p->name,
             addr_str(&p->data, a, sizeof(a)),
             p->initiator ? "invited by us" : "invited us");
    publish_status();
}

static void peer_down(peer_t *p, const char *why, int say_bye)
{
    char a[64];
    if (p->state == P_UP) {
        LOG_INFO(LOG_SRC, "session down: \"%s\" %s (%s)", p->name,
                 addr_str(&p->data, a, sizeof(a)), why);
        if (say_bye) {
            uint8_t b[32];
            int n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_BY,
                                              p->token, g_ssrc, NULL);
            send_to(g_ctrl_fd, &p->ctrl, b, n);
        }
    }
    if (p->initiator) {
        peer_set_state(p, P_IDLE);
    } else {
        memset(p, 0, sizeof(*p));
    }
    publish_status();
}

static peer_t *peer_by_ssrc(uint32_t ssrc)
{
    for (int i = 0; i < MAX_PEERS; i++) {
        peer_t *p = &g_peers[i];
        if (p->state > P_INVITE_CTRL && p->ssrc == ssrc) return p;
    }
    return NULL;
}

static peer_t *peer_by_token(uint32_t token, int state)
{
    for (int i = 0; i < MAX_PEERS; i++) {
        peer_t *p = &g_peers[i];
        if (p->initiator && p->state == state && p->token == token) return p;
    }
    return NULL;
}

static peer_t *peer_alloc(void)
{
    for (int i = 0; i < MAX_PEERS; i++)
        if (g_peers[i].state == P_FREE) return &g_peers[i];
    return NULL;
}

/* ---- inbound MIDI ------------------------------------------------------- */

static void in_usb(void *ctx, const uint8_t pkt[4])
{
    (void)ctx;
    rtpmidi_ring_push(&g_shm->in, pkt);
}

static void in_bytes(void *ctx, const uint8_t *bytes, int n)
{
    peer_t *p = (peer_t *)ctx;
    rtpmidi_bytes_to_usb(&p->enc, 2, bytes, n, in_usb, NULL);
}

/* ---- session commands --------------------------------------------------- */

static void reply_session(int fd, const struct sockaddr_in *to, uint16_t cmd,
                          uint32_t token)
{
    uint8_t b[16 + RTPMIDI_NAME_MAX];
    int n = rtpmidi_cmd_build_session(b, sizeof(b), cmd, token, g_ssrc, g_name);
    send_to(fd, to, b, n);
}

static void send_sync(peer_t *p, uint8_t count, const uint64_t ts[3],
                      const struct sockaddr_in *to)
{
    uint8_t b[36];
    int n = rtpmidi_cmd_build_ck(b, sizeof(b), g_ssrc, count, ts);
    send_to(g_data_fd, to, b, n);
    p->last_sync_ms = now_ms();
}

static void handle_cmd(int is_data, const rtpmidi_cmd_t *c,
                       const struct sockaddr_in *from)
{
    int fd = is_data ? g_data_fd : g_ctrl_fd;
    peer_t *p;

    switch (c->cmd) {
    case RTPMIDI_CMD_IN:
        p = peer_by_ssrc(c->ssrc);
        if (p && p->initiator) {
            /* A host we invite has invited us. One session is enough; ours
             * is already under way or up, so decline theirs. */
            reply_session(fd, from, RTPMIDI_CMD_NO, c->token);
            return;
        }
        if (!p) p = peer_alloc();
        if (!p) {
            reply_session(fd, from, RTPMIDI_CMD_NO, c->token);
            return;
        }
        p->ssrc = c->ssrc;
        p->token = c->token;
        p->last_rx_ms = now_ms();
        snprintf(p->name, sizeof(p->name), "%s", c->name);
        if (!is_data) {
            p->ctrl = *from;
            if (p->state != P_UP) peer_set_state(p, P_WAIT_DATA);
            reply_session(fd, from, RTPMIDI_CMD_OK, c->token);
        } else {
            p->data = *from;
            if (p->state == P_FREE) {           /* data before control */
                p->ctrl = *from;
                p->ctrl.sin_port = htons((uint16_t)(ntohs(from->sin_port) - 1));
            }
            reply_session(fd, from, RTPMIDI_CMD_OK, c->token);
            if (p->state != P_UP) peer_up(p);
        }
        return;

    case RTPMIDI_CMD_OK:
        if (!is_data) {
            p = peer_by_token(c->token, P_INVITE_CTRL);
            if (!p) return;
            p->ssrc = c->ssrc;
            snprintf(p->name, sizeof(p->name), "%s", c->name);
            peer_set_state(p, P_INVITE_DATA);
            uint8_t b[16 + RTPMIDI_NAME_MAX];
            int n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_IN,
                                              p->token, g_ssrc, g_name);
            send_to(g_data_fd, &p->data, b, n);
        } else {
            p = peer_by_token(c->token, P_INVITE_DATA);
            if (!p) return;
            peer_up(p);
            uint64_t ts[3] = { now_100us(), 0, 0 };
            send_sync(p, 0, ts, &p->data);
        }
        return;

    case RTPMIDI_CMD_NO:
        p = peer_by_token(c->token, P_INVITE_CTRL);
        if (!p) p = peer_by_token(c->token, P_INVITE_DATA);
        if (p) peer_set_state(p, P_IDLE);
        return;

    case RTPMIDI_CMD_BY:
        p = peer_by_ssrc(c->ssrc);
        if (p) peer_down(p, "peer said goodbye", 0);
        return;

    case RTPMIDI_CMD_CK:
        p = peer_by_ssrc(c->ssrc);
        if (!p || p->state != P_UP) return;
        p->last_rx_ms = now_ms();
        if (c->count == 0) {
            uint64_t ts[3] = { c->ts[0], now_100us(), 0 };
            send_sync(p, 1, ts, from);
        } else if (c->count == 1) {
            uint64_t ts[3] = { c->ts[0], c->ts[1], now_100us() };
            send_sync(p, 2, ts, from);
        }
        return;

    default:                                    /* RS: nothing to do, J=0 */
        return;
    }
}

static void handle_socket(int is_data)
{
    int fd = is_data ? g_data_fd : g_ctrl_fd;
    for (int burst = 0; burst < 64; burst++) {
        uint8_t buf[2048];
        struct sockaddr_in from;
        socklen_t flen = sizeof(from);
        ssize_t n = recvfrom(fd, buf, sizeof(buf), MSG_DONTWAIT,
                             (struct sockaddr *)&from, &flen);
        if (n <= 0) return;

        rtpmidi_cmd_t c;
        if (rtpmidi_cmd_parse(buf, (int)n, &c)) {
            handle_cmd(is_data, &c, &from);
            continue;
        }
        if (!is_data || n < 12) continue;

        /* The SSRC picks the session before the walk, so a segmented SysEx
         * is continued in the state of the peer that opened it. */
        uint32_t ssrc = ((uint32_t)buf[8] << 24) | ((uint32_t)buf[9] << 16) |
                        ((uint32_t)buf[10] << 8) | (uint32_t)buf[11];
        peer_t *p = peer_by_ssrc(ssrc);
        if (!p || p->state != P_UP) continue;
        uint16_t seq = 0;
        if (!rtpmidi_rtp_parse(buf, (int)n, &p->rx, &seq, NULL,
                               g_input ? in_bytes : NULL, p))
            continue;
        p->last_rx_ms = now_ms();
        p->last_seq_in = seq;
        p->rs_due = 1;
    }
}

/* ---- timers ------------------------------------------------------------- */

static void run_timers(void)
{
    uint64_t t = now_ms();
    for (int i = 0; i < MAX_PEERS; i++) {
        peer_t *p = &g_peers[i];
        switch (p->state) {
        case P_IDLE:
            if (t - p->state_ms < INVITE_RETRY_MS && p->token) break;
            p->token = rand32();
            peer_set_state(p, P_INVITE_CTRL);
            {
                uint8_t b[16 + RTPMIDI_NAME_MAX];
                int n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_IN,
                                                  p->token, g_ssrc, g_name);
                send_to(g_ctrl_fd, &p->ctrl, b, n);
            }
            break;
        case P_INVITE_CTRL:
            /* Unanswered: back to idle quickly, so a peer that comes up is
             * found within one retry period. */
            if (t - p->state_ms >= INVITE_RETRY_MS) peer_set_state(p, P_IDLE);
            break;
        case P_INVITE_DATA:
            if (t - p->state_ms >= INVITE_GIVEUP_MS) peer_set_state(p, P_IDLE);
            break;
        case P_WAIT_DATA:
            if (t - p->state_ms >= INVITE_GIVEUP_MS) peer_down(p, "", 0);
            break;
        case P_UP:
            if (t - p->last_rx_ms >= PEER_TIMEOUT_MS) {
                peer_down(p, "timed out", 1);
                break;
            }
            if (p->initiator && t - p->last_sync_ms >= SYNC_PERIOD_MS) {
                uint64_t ts[3] = { now_100us(), 0, 0 };
                send_sync(p, 0, ts, &p->data);
            }
            if (p->rs_due && t - p->last_rs_ms >= RS_PERIOD_MS) {
                uint8_t b[12];
                int n = rtpmidi_cmd_build_rs(b, sizeof(b), g_ssrc, p->last_seq_in);
                send_to(g_ctrl_fd, &p->ctrl, b, n);
                p->rs_due = 0;
                p->last_rs_ms = t;
            }
            break;
        default:
            break;
        }
    }
}

/* ---- outbound MIDI ------------------------------------------------------ */

static void flush_pkt(rtpmidi_pkt_t *pkt)
{
    if (pkt->ncmd) {
        uint32_t ts = (uint32_t)now_100us();
        for (int i = 0; i < MAX_PEERS; i++) {
            peer_t *p = &g_peers[i];
            if (p->state != P_UP) continue;
            int n = rtpmidi_pkt_finish(pkt, p->seq_out++, ts, g_ssrc);
            send_to(g_data_fd, &p->data, pkt->buf, n);
        }
    }
    rtpmidi_pkt_begin(pkt);
}

static void drain_out(void)
{
    static rtpmidi_pkt_t pkt;
    uint8_t usb[4];
    rtpmidi_pkt_begin(&pkt);
    for (uint32_t i = 0; i < RTPMIDI_RING_PACKETS; i++) {
        if (!rtpmidi_ring_peek_at(&g_shm->out, usb, 0)) break;
        rtpmidi_ring_pop(&g_shm->out);
        const uint8_t *msg;
        int len;
        if (!rtpmidi_usb_to_msg(&g_asm, usb, &msg, &len)) continue;

        if (len > RTPMIDI_PKT_MAX - RTPMIDI_PKT_HDR - 1) {
            /* A SysEx too big for one datagram: segments, each alone. */
            uint8_t seg[RTPMIDI_SYSEX_CHUNK + 2];
            int pos = 0;
            flush_pkt(&pkt);
            while (pos < len) {
                int sn = rtpmidi_sysex_segment(msg, len, &pos, seg);
                rtpmidi_pkt_add(&pkt, seg, sn);
                flush_pkt(&pkt);
            }
            continue;
        }
        if (!rtpmidi_pkt_add(&pkt, msg, len)) {
            flush_pkt(&pkt);
            rtpmidi_pkt_add(&pkt, msg, len);
        }
    }
    flush_pkt(&pkt);
}

/* ---- setup -------------------------------------------------------------- */

static int parse_peer(const char *val, struct sockaddr_in *ctrl,
                      struct sockaddr_in *data)
{
    char host[128];
    int port = 5004;
    snprintf(host, sizeof(host), "%s", val);
    char *colon = strrchr(host, ':');
    if (colon) {
        *colon = '\0';
        port = atoi(colon + 1);
    }
    if (port <= 0 || port >= 65535) return 0;
    memset(ctrl, 0, sizeof(*ctrl));
    ctrl->sin_family = AF_INET;
    if (inet_pton(AF_INET, host, &ctrl->sin_addr) != 1) return 0;
    *data = *ctrl;
    ctrl->sin_port = htons((uint16_t)port);
    data->sin_port = htons((uint16_t)(port + 1));
    return 1;
}

static void load_conf(const char *path)
{
    FILE *f = fopen(path, "r");
    if (!f) return;                 /* every key has a default */
    char line[256];
    while (fgets(line, sizeof(line), f)) {
        char *s = line;
        while (*s == ' ' || *s == '\t') s++;
        if (*s == '#' || *s == '\n' || *s == '\r' || !*s) continue;
        char *eq = strchr(s, '=');
        if (!eq) continue;
        *eq = '\0';
        char *val = eq + 1;
        while (*val == ' ' || *val == '\t') val++;
        char *e = val + strlen(val);
        while (e > val && (e[-1] == '\n' || e[-1] == '\r' || e[-1] == ' ' ||
                           e[-1] == '\t')) *--e = '\0';
        e = s + strlen(s);
        while (e > s && (e[-1] == ' ' || e[-1] == '\t')) *--e = '\0';

        if (!strcmp(s, "name")) {
            if (*val) snprintf(g_name, sizeof(g_name), "%s", val);
        } else if (!strcmp(s, "port")) {
            int p = atoi(val);
            if (p > 0 && p < 65535) g_port = p;
        } else if (!strcmp(s, "output")) {
            g_output = atoi(val) != 0;
        } else if (!strcmp(s, "input")) {
            g_input = atoi(val) != 0;
        } else if (!strcmp(s, "peer")) {
            peer_t *p = peer_alloc();
            struct sockaddr_in c, d;
            if (!p) {
                LOG_WARN(LOG_SRC, "peer=%s ignored: at most %d", val, MAX_PEERS);
            } else if (!parse_peer(val, &c, &d)) {
                LOG_WARN(LOG_SRC, "peer=%s ignored: want an IPv4 address[:port]", val);
            } else {
                p->initiator = 1;
                p->ctrl = c;
                p->data = d;
                snprintf(p->name, sizeof(p->name), "%s", val);
                peer_set_state(p, P_IDLE);
            }
        }
    }
    fclose(f);
}

static int open_udp(int port)
{
    int fd = socket(AF_INET, SOCK_DGRAM | SOCK_CLOEXEC, 0);
    if (fd < 0) return -1;
    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
    struct sockaddr_in a;
    memset(&a, 0, sizeof(a));
    a.sin_family = AF_INET;
    a.sin_addr.s_addr = htonl(INADDR_ANY);      /* every interface */
    a.sin_port = htons((uint16_t)port);
    if (bind(fd, (struct sockaddr *)&a, sizeof(a)) != 0) {
        close(fd);
        return -1;
    }
    return fd;
}

static rtpmidi_shm_t *open_shm(const char *name)
{
    int fd = shm_open(name, O_CREAT | O_RDWR, 0666);
    if (fd < 0) return NULL;
    fchmod(fd, 0666);
    /* Same size every time, so this neither shrinks nor zeroes a segment the
     * shim already has mapped. */
    if (ftruncate(fd, (off_t)sizeof(rtpmidi_shm_t)) != 0) {
        close(fd);
        return NULL;
    }
    void *m = mmap(NULL, sizeof(rtpmidi_shm_t), PROT_READ | PROT_WRITE,
                   MAP_SHARED, fd, 0);
    close(fd);
    return m == MAP_FAILED ? NULL : (rtpmidi_shm_t *)m;
}

static void on_signal(int sig) { (void)sig; g_quit = 1; }

int main(int argc, char **argv)
{
    const char *conf = DEFAULT_CONF;
    const char *shm_name = RTPMIDI_SHM_NAME;
    int port_override = 0;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "--conf") && i + 1 < argc) conf = argv[++i];
        else if (!strcmp(argv[i], "--shm") && i + 1 < argc) shm_name = argv[++i];
        else if (!strcmp(argv[i], "--port") && i + 1 < argc) port_override = atoi(argv[++i]);
        else {
            fprintf(stderr, "usage: %s [--conf FILE] [--shm NAME] [--port N]\n", argv[0]);
            return 2;
        }
    }

    unified_log_init();
    load_conf(conf);
    if (port_override > 0) g_port = port_override;

    g_shm = open_shm(shm_name);
    if (!g_shm) {
        LOG_ERROR(LOG_SRC, "cannot map %s: %s", shm_name, strerror(errno));
        return 1;
    }
    g_ctrl_fd = open_udp(g_port);
    g_data_fd = open_udp(g_port + 1);
    if (g_ctrl_fd < 0 || g_data_fd < 0) {
        LOG_ERROR(LOG_SRC, "cannot bind UDP %d/%d: %s", g_port, g_port + 1,
                  strerror(errno));
        return 1;
    }

    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = on_signal;
    sigaction(SIGTERM, &sa, NULL);
    sigaction(SIGINT, &sa, NULL);

    g_ssrc = rand32();
    g_shm->magic = RTPMIDI_SHM_MAGIC;
    g_shm->version = RTPMIDI_SHM_VERSION;
    publish_status();
    /* Whatever a previous instance left queued is not ours to send. */
    rtpmidi_ring_flush(&g_shm->out);

    LOG_INFO(LOG_SRC, "listening on UDP %d/%d as \"%s\" (output=%d input=%d)",
             g_port, g_port + 1, g_name, g_output, g_input);

    while (!g_quit) {
        struct pollfd fds[2] = {
            { .fd = g_ctrl_fd, .events = POLLIN },
            { .fd = g_data_fd, .events = POLLIN },
        };
        /* With a session up the out ring is polled every millisecond: the
         * shim cannot wake us (a wake is a syscall on the SPI callback), and
         * 1 ms is a third of the audio block the MIDI was produced in. Idle,
         * only the sockets and the retry timers need serving. */
        int up = (int)__atomic_load_n(&g_shm->peers, __ATOMIC_RELAXED);
        int rc = poll(fds, 2, up ? 1 : 200);
        if (rc > 0) {
            if (fds[0].revents & POLLIN) handle_socket(0);
            if (fds[1].revents & POLLIN) handle_socket(1);
        }
        run_timers();
        drain_out();
    }

    for (int i = 0; i < MAX_PEERS; i++)
        if (g_peers[i].state == P_UP) peer_down(&g_peers[i], "shutting down", 1);
    __atomic_store_n(&g_shm->out_active, 0u, __ATOMIC_RELAXED);
    __atomic_store_n(&g_shm->peers, 0u, __ATOMIC_RELAXED);
    unified_log_shutdown();
    return 0;
}

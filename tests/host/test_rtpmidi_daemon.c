/*
 * The network MIDI sidecar, end to end, over loopback.
 *
 * Builds nothing itself: argv[1] is a natively built schwung-rtpmidi. The
 * test plays BOTH kinds of peer the daemon must work with --
 *
 *   1. a peer that INVITES it (macOS Audio MIDI Setup, rtpMIDI): the full
 *      AppleMIDI handshake on both ports, a clock sync, then MIDI each way;
 *   2. a peer that only LISTENS and is named in the config as `peer=`: the
 *      daemon must invite it, unprompted, and sync as the initiator.
 *
 * -- and stands in for the shim by pushing to / popping from the same SHM
 * rings (rtpmidi_shm.h) under a private segment name.
 */
#define _GNU_SOURCE
#include <arpa/inet.h>
#include <fcntl.h>
#include <netinet/in.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/socket.h>
#include <sys/wait.h>
#include <unistd.h>

#include "rtpmidi_proto.h"
#include "rtpmidi_shm.h"

static int fails = 0;
#define CHECK(cond, ...) do { if (!(cond)) { fails++; \
    printf("FAIL %s:%d: ", __FILE__, __LINE__); printf(__VA_ARGS__); printf("\n"); } } while (0)

static int udp_bind(int port)
{
    int fd = socket(AF_INET, SOCK_DGRAM, 0);
    struct sockaddr_in a = { .sin_family = AF_INET, .sin_port = htons((uint16_t)port) };
    a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    if (bind(fd, (struct sockaddr *)&a, sizeof(a)) != 0) { perror("bind"); exit(1); }
    return fd;
}
static struct sockaddr_in lo(int port)
{
    struct sockaddr_in a = { .sin_family = AF_INET, .sin_port = htons((uint16_t)port) };
    a.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
    return a;
}
static int recv_wait(int fd, uint8_t *buf, int cap, struct sockaddr_in *from, int ms)
{
    struct pollfd p = { .fd = fd, .events = POLLIN };
    if (poll(&p, 1, ms) <= 0) return 0;
    socklen_t fl = sizeof(*from);
    ssize_t n = recvfrom(fd, buf, (size_t)cap, 0, (struct sockaddr *)from, &fl);
    return n > 0 ? (int)n : 0;
}
static void send_to(int fd, struct sockaddr_in to, const uint8_t *b, int n)
{
    sendto(fd, b, (size_t)n, 0, (struct sockaddr *)&to, sizeof(to));
}
/* Wait for a command of one kind, skipping others (e.g. an RS). */
static int recv_cmd(int fd, uint16_t want, rtpmidi_cmd_t *c, struct sockaddr_in *from, int ms)
{
    uint8_t b[2048];
    for (int tries = 0; tries < 20; tries++) {
        int n = recv_wait(fd, b, sizeof(b), from, ms);
        if (!n) return 0;
        if (rtpmidi_cmd_parse(b, n, c) && c->cmd == want) return 1;
    }
    return 0;
}

static uint8_t g_bytes[4096]; static int g_nbytes;
static void collect(void *ctx, const uint8_t *b, int n) {
    (void)ctx; memcpy(g_bytes + g_nbytes, b, (size_t)n); g_nbytes += n;
}
/* Receive RTP-MIDI until `want` MIDI bytes have arrived. */
static int recv_midi(int fd, int want, int ms)
{
    uint8_t b[2048]; struct sockaddr_in from; rtpmidi_rx_t rx = {0};
    g_nbytes = 0;
    for (int tries = 0; tries < 50 && g_nbytes < want; tries++) {
        int n = recv_wait(fd, b, sizeof(b), &from, ms);
        if (!n) break;
        rtpmidi_rtp_parse(b, n, &rx, NULL, NULL, collect, NULL);
    }
    return g_nbytes;
}
static int wait_in(rtpmidi_shm_t *shm, uint8_t out[4], int ms)
{
    for (int i = 0; i < ms; i++) {
        if (rtpmidi_ring_peek_at(&shm->in, out, 0)) { rtpmidi_ring_pop(&shm->in); return 1; }
        usleep(1000);
    }
    return 0;
}
static int wait_flag(volatile uint32_t *v, uint32_t want, int ms)
{
    for (int i = 0; i < ms; i++) { if (*v == want) return 1; usleep(1000); }
    return 0;
}

int main(int argc, char **argv)
{
    if (argc < 2) { fprintf(stderr, "usage: %s <schwung-rtpmidi>\n", argv[0]); return 2; }
    int base = 20000 + (getpid() % 20000) * 2;     /* daemon: base, base+1 */
    int lport = base + 10;                          /* our listening peer   */
    char shm_name[64], conf[128], port_s[16];
    snprintf(shm_name, sizeof(shm_name), "/schwung-rtpmidi-test-%d", (int)getpid());
    snprintf(conf, sizeof(conf), "/tmp/schwung-rtpmidi-test-%d.conf", (int)getpid());
    snprintf(port_s, sizeof(port_s), "%d", base);

    FILE *f = fopen(conf, "w");
    fprintf(f, "# test\nname = Test Move\npeer=127.0.0.1:%d\n", lport);
    fclose(f);

    /* The listening peer's sockets exist BEFORE the daemon starts. */
    int l_ctrl = udp_bind(lport), l_data = udp_bind(lport + 1);

    pid_t pid = fork();
    if (pid == 0) {
        execl(argv[1], argv[1], "--conf", conf, "--shm", shm_name, "--port", port_s, (char *)0);
        _exit(127);
    }

    rtpmidi_shm_t *shm = NULL;
    for (int i = 0; i < 2000 && !shm; i++) {
        int fd = shm_open(shm_name, O_RDWR, 0);
        if (fd >= 0) {
            void *m = mmap(NULL, sizeof(*shm), PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
            close(fd);
            if (m != MAP_FAILED && ((rtpmidi_shm_t *)m)->magic == RTPMIDI_SHM_MAGIC) shm = m;
            else if (m != MAP_FAILED) munmap(m, sizeof(*shm));
        }
        if (!shm) usleep(1000);
    }
    if (!shm) { printf("FAIL: daemon never published its SHM\n"); kill(pid, SIGKILL); return 1; }

    uint8_t b[2048]; rtpmidi_cmd_t c; struct sockaddr_in from;
    int n;

    /* ---- 2. the daemon invites the configured peer ---- */
    CHECK(recv_cmd(l_ctrl, RTPMIDI_CMD_IN, &c, &from, 3000), "daemon invites peer= (control)");
    CHECK(!strcmp(c.name, "Test Move"), "name from config: \"%s\"", c.name);
    uint32_t d_ssrc = c.ssrc, tok = c.token;
    n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_OK, tok, 0x0BADF00D, "listener");
    send_to(l_ctrl, from, b, n);
    CHECK(recv_cmd(l_data, RTPMIDI_CMD_IN, &c, &from, 2000) && c.token == tok, "then on the data port");
    send_to(l_data, from, b, n);
    CHECK(recv_cmd(l_data, RTPMIDI_CMD_CK, &c, &from, 2000) && c.count == 0, "initiator syncs (CK0)");
    uint64_t ts[3] = { c.ts[0], 1234, 0 };
    n = rtpmidi_cmd_build_ck(b, sizeof(b), 0x0BADF00D, 1, ts);
    send_to(l_data, from, b, n);
    CHECK(recv_cmd(l_data, RTPMIDI_CMD_CK, &c, &from, 2000) && c.count == 2 && c.ts[1] == 1234, "CK2 closes the exchange");
    CHECK(wait_flag(&shm->peers, 1, 2000) && shm->out_active == 1, "one session up, output active");

    /* ---- 1. a peer invites the daemon ---- */
    int i_ctrl = udp_bind(base + 20), i_data = udp_bind(base + 21);
    n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_IN, 0x77, 0xC0FFEE01, "inviter");
    send_to(i_ctrl, lo(base), b, n);
    CHECK(recv_cmd(i_ctrl, RTPMIDI_CMD_OK, &c, &from, 2000) && c.token == 0x77 && c.ssrc == d_ssrc, "invitation accepted (control)");
    send_to(i_data, lo(base + 1), b, n);
    CHECK(recv_cmd(i_data, RTPMIDI_CMD_OK, &c, &from, 2000), "invitation accepted (data)");
    uint64_t ts0[3] = { 42, 0, 0 };
    n = rtpmidi_cmd_build_ck(b, sizeof(b), 0xC0FFEE01, 0, ts0);
    send_to(i_data, lo(base + 1), b, n);
    CHECK(recv_cmd(i_data, RTPMIDI_CMD_CK, &c, &from, 2000) && c.count == 1 && c.ts[0] == 42, "responder answers CK0 with CK1");
    CHECK(wait_flag(&shm->peers, 2, 2000), "two sessions up");

    /* ---- a reply leaves from the address the peer CONTACTED ----
     * The daemon is bound to every interface, so an unbound reply takes its
     * source from the route back -- on a Move reached at its USB address by a
     * host it routes to over Wi-Fi, the Wi-Fi address, which the client does
     * not recognise as the session it invited. 127.0.0.2 is a second local
     * address on any Linux loopback: invited there, the answer must come from
     * there and not from 127.0.0.1. */
    {
        int a_fd = udp_bind(base + 30);
        struct sockaddr_in alt = lo(base);
        alt.sin_addr.s_addr = htonl(0x7F000002);
        n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_IN, 0x99, 0xA17E0001, "alt");
        send_to(a_fd, alt, b, n);
        CHECK(recv_cmd(a_fd, RTPMIDI_CMD_OK, &c, &from, 2000), "invitation to a second local address accepted");
        CHECK(from.sin_addr.s_addr == htonl(0x7F000002), "reply source is the address invited (got %s)",
              inet_ntoa(from.sin_addr));
        n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_BY, 0x99, 0xA17E0001, NULL);
        send_to(a_fd, alt, b, n);
        close(a_fd);
        usleep(50000);
        CHECK(wait_flag(&shm->peers, 2, 2000), "still two sessions up");
    }

    /* ---- shim -> network: both peers get it, SysEx reassembled ---- */
    const uint8_t out_pkts[][4] = {
        {0x29, 0x90, 60, 100}, {0x2F, 0xF8, 0, 0},
        {0x24, 0xF0, 0x7D, 1}, {0x27, 2, 3, 0xF7}, {0x28, 0x80, 60, 0},
    };
    for (unsigned i = 0; i < sizeof(out_pkts) / 4; i++) rtpmidi_ring_push(&shm->out, out_pkts[i]);
    const uint8_t want[] = { 0x90,60,100, 0xF8, 0xF0,0x7D,1,2,3,0xF7, 0x80,60,0 };
    CHECK(recv_midi(i_data, sizeof(want), 2000) == (int)sizeof(want) &&
          !memcmp(g_bytes, want, sizeof(want)), "inviter receives the stream (%d bytes)", g_nbytes);
    CHECK(recv_midi(l_data, sizeof(want), 2000) == (int)sizeof(want) &&
          !memcmp(g_bytes, want, sizeof(want)), "listener receives the stream (%d bytes)", g_nbytes);

    /* ---- network -> shim: arrives as cable-2 USB-MIDI packets ---- */
    rtpmidi_pkt_t pkt; rtpmidi_pkt_begin(&pkt);
    const uint8_t on[] = { 0x91, 64, 99 }, sx[] = { 0xF0, 1, 2, 0xF7 };
    rtpmidi_pkt_add(&pkt, on, 3); rtpmidi_pkt_add(&pkt, sx, 4);
    n = rtpmidi_pkt_finish(&pkt, 1, 0, 0xC0FFEE01);
    send_to(i_data, lo(base + 1), pkt.buf, n);
    uint8_t got[4];
    CHECK(wait_in(shm, got, 2000) && !memcmp(got, (uint8_t[]){0x29, 0x91, 64, 99}, 4), "note on in");
    CHECK(wait_in(shm, got, 2000) && !memcmp(got, (uint8_t[]){0x24, 0xF0, 1, 2}, 4), "sysex head in");
    CHECK(wait_in(shm, got, 2000) && !memcmp(got, (uint8_t[]){0x25, 0xF7, 0, 0}, 4), "sysex tail in");

    /* RTP from a stranger (no session) is not delivered. */
    rtpmidi_pkt_begin(&pkt); rtpmidi_pkt_add(&pkt, on, 3);
    n = rtpmidi_pkt_finish(&pkt, 1, 0, 0xDEAD0001);
    send_to(i_data, lo(base + 1), pkt.buf, n);
    CHECK(!wait_in(shm, got, 150), "unknown SSRC ignored");

    /* ---- goodbye from one peer; the other stays; shutdown says BY ---- */
    n = rtpmidi_cmd_build_session(b, sizeof(b), RTPMIDI_CMD_BY, 0x77, 0xC0FFEE01, NULL);
    send_to(i_ctrl, lo(base), b, n);
    CHECK(wait_flag(&shm->peers, 1, 2000), "BY ends that session only");
    kill(pid, SIGTERM);
    CHECK(recv_cmd(l_ctrl, RTPMIDI_CMD_BY, &c, &from, 2000), "daemon says BY on shutdown");
    int st = 0; waitpid(pid, &st, 0);
    CHECK(WIFEXITED(st) && WEXITSTATUS(st) == 0, "clean exit (status %d)", st);
    CHECK(shm->out_active == 0, "output marked inactive on exit");

    shm_unlink(shm_name); unlink(conf);
    if (fails) { printf("test_rtpmidi_daemon: %d FAILED\n", fails); return 1; }
    printf("test_rtpmidi_daemon: PASS\n");
    return 0;
}

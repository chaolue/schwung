/* shadow_state.c - Shadow slot state persistence
 * Extracted from schwung_shim.c for maintainability. */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <pwd.h>
#include "shadow_state.h"
#include "shadow_constants.h"

/* ============================================================================
 * Host callbacks (set by state_init)
 * ============================================================================ */

static void (*host_log)(const char *msg);
static shadow_chain_slot_t *host_chain_slots;
static int *host_solo_count;

/* Fix file ownership after writing as root */
static void chown_to_ableton(const char *path) {
    struct passwd *pw = getpwnam("ableton");
    if (pw) chown(path, pw->pw_uid, pw->pw_gid);
}

/* ============================================================================
 * Per-slot arrays: one value per chain slot, Move's four first, then the aux
 * slots. Written "[a, b, c, d, ...]" -- exactly the shape the four-value
 * sscanf of builds before the aux slots parses, so a DOWNGRADE still reads
 * the first four and ignores the rest. Read back as however many the file
 * holds: a file from before the aux slots has four, and the aux slots keep
 * their defaults.
 * ============================================================================ */

enum { SLOT_VOLUME, SLOT_CHANNEL, SLOT_FORWARD, SLOT_TRANSPOSE, SLOT_MUTED, SLOT_SOLOED };

static int slot_int_field(int i, int field)
{
    switch (field) {
    case SLOT_CHANNEL:   return host_chain_slots[i].channel;
    case SLOT_FORWARD:   return host_chain_slots[i].forward_channel;
    case SLOT_TRANSPOSE: return host_chain_slots[i].transpose;
    case SLOT_MUTED:     return host_chain_slots[i].muted;
    case SLOT_SOLOED:    return host_chain_slots[i].soloed;
    default:             return 0;
    }
}

static void write_slot_array(FILE *f, const char *key, int field, int last)
{
    fprintf(f, "  \"%s\": [", key);
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) {
        if (i) fputs(", ", f);
        if (field == SLOT_VOLUME) fprintf(f, "%.3f", host_chain_slots[i].volume);
        else fprintf(f, "%d", slot_int_field(i, field));
    }
    fprintf(f, "]%s\n", last ? "" : ",");
}

/* Up to `max` numbers from the "[a, b, ...]" that follows `key`. Returns how
 * many were read; fewer than SHADOW_MOVE_SLOTS is not a whole answer and the
 * callers ignore it, as the four-value sscanf it replaces did. */
static int parse_slot_array(const char *json, const char *key, double *out, int max)
{
    const char *p = strstr(json, key);
    if (!p) return 0;
    p = strchr(p, '[');
    if (!p) return 0;
    p++;
    int n = 0;
    while (n < max) {
        while (*p == ' ' || *p == '\n' || *p == '\r' || *p == '\t') p++;
        char *end;
        double v = strtod(p, &end);
        if (end == p) break;
        out[n++] = v;
        p = end;
        while (*p == ' ' || *p == '\n' || *p == '\r' || *p == '\t') p++;
        if (*p != ',') break;
        p++;
    }
    return n;
}

static void slot_array_text(char *buf, size_t len, const double *v, int n, int is_float)
{
    size_t off = 0;
    buf[0] = '\0';
    for (int i = 0; i < n && off < len; i++) {
        int w = is_float ? snprintf(buf + off, len - off, "%s%.2f", i ? ", " : "", v[i])
                         : snprintf(buf + off, len - off, "%s%d", i ? ", " : "", (int)v[i]);
        if (w < 0) break;
        off += (size_t)w;
    }
}

void state_init(const state_host_t *host)
{
    host_log = host->log;
    host_chain_slots = host->chain_slots;
    host_solo_count = host->solo_count;
}

/* ============================================================================
 * shadow_save_state - Write slot state to shadow_chain_config.json
 * ============================================================================ */

static volatile int g_save_requested;
void shadow_request_save_state(void) { __atomic_store_n(&g_save_requested, 1, __ATOMIC_RELEASE); }
void shadow_save_state_service(void)
{
    if (__atomic_exchange_n(&g_save_requested, 0, __ATOMIC_ACQ_REL)) shadow_save_state();
}

void shadow_save_state(void)
{
    /* Read existing config to preserve fields written by shadow_ui.js */
    FILE *f = fopen(SHADOW_CONFIG_PATH, "r");
    char patches_buf[4096] = "";
    char master_fx[256] = "";
    char master_fx_path[256] = "";
    char master_fx_chain_buf[2048] = "";
    int overlay_knobs_mode = -1;
    int resample_bridge_mode = -1;
    int link_audio_routing_saved = -1;

    if (f) {
        fseek(f, 0, SEEK_END);
        long size = ftell(f);
        fseek(f, 0, SEEK_SET);

        if (size > 0 && size < 65536) {
            char *json = malloc(size + 1);
            if (json) {
                size_t nread = fread(json, 1, size, f);
                json[nread] = '\0';

                /* Extract patches array (preserve as-is) */
                char *patches_start = strstr(json, "\"patches\":");
                if (patches_start) {
                    char *arr_start = strchr(patches_start, '[');
                    if (arr_start) {
                        int depth = 1;
                        char *arr_end = arr_start + 1;
                        while (*arr_end && depth > 0) {
                            if (*arr_end == '[') depth++;
                            else if (*arr_end == ']') depth--;
                            arr_end++;
                        }
                        int len = arr_end - arr_start;
                        if (len < (int)sizeof(patches_buf) - 1) {
                            strncpy(patches_buf, arr_start, len);
                            patches_buf[len] = '\0';
                        }
                    }
                }

                /* Extract master_fx string (legacy single-slot) */
                char *mfx = strstr(json, "\"master_fx\":");
                if (mfx) {
                    mfx = strchr(mfx, ':');
                    if (mfx) {
                        mfx++;
                        while (*mfx == ' ' || *mfx == '"') mfx++;
                        char *end = mfx;
                        while (*end && *end != '"' && *end != ',' && *end != '\n') end++;
                        int len = end - mfx;
                        if (len < (int)sizeof(master_fx) - 1) {
                            strncpy(master_fx, mfx, len);
                            master_fx[len] = '\0';
                        }
                    }
                }

                /* Extract master_fx_path string */
                char *mfxp = strstr(json, "\"master_fx_path\":");
                if (mfxp) {
                    mfxp = strchr(mfxp, ':');
                    if (mfxp) {
                        mfxp++;
                        while (*mfxp == ' ' || *mfxp == '"') mfxp++;
                        char *end = mfxp;
                        while (*end && *end != '"' && *end != ',' && *end != '\n') end++;
                        int len = end - mfxp;
                        if (len < (int)sizeof(master_fx_path) - 1) {
                            strncpy(master_fx_path, mfxp, len);
                            master_fx_path[len] = '\0';
                        }
                    }
                }

                /* Extract master_fx_chain object (written by shadow_ui.js) */
                char *mfc = strstr(json, "\"master_fx_chain\":");
                if (mfc) {
                    char *obj_start = strchr(mfc, '{');
                    if (obj_start) {
                        int depth = 1;
                        char *obj_end = obj_start + 1;
                        while (*obj_end && depth > 0) {
                            if (*obj_end == '{') depth++;
                            else if (*obj_end == '}') depth--;
                            obj_end++;
                        }
                        int len = obj_end - obj_start;
                        if (len < (int)sizeof(master_fx_chain_buf) - 1) {
                            strncpy(master_fx_chain_buf, obj_start, len);
                            master_fx_chain_buf[len] = '\0';
                        }
                    }
                }

                /* Extract overlay_knobs_mode integer */
                char *okm = strstr(json, "\"overlay_knobs_mode\":");
                if (okm) {
                    okm = strchr(okm, ':');
                    if (okm) {
                        okm++;
                        while (*okm == ' ') okm++;
                        overlay_knobs_mode = atoi(okm);
                    }
                }

                /* Extract resample_bridge_mode integer */
                char *rbm = strstr(json, "\"resample_bridge_mode\":");
                if (rbm) {
                    rbm = strchr(rbm, ':');
                    if (rbm) {
                        rbm++;
                        while (*rbm == ' ') rbm++;
                        resample_bridge_mode = atoi(rbm);
                    }
                }

                /* Extract link_audio_routing boolean */
                char *lar = strstr(json, "\"link_audio_routing\":");
                if (lar) {
                    lar = strchr(lar, ':');
                    if (lar) {
                        lar++;
                        while (*lar == ' ') lar++;
                        link_audio_routing_saved = (strncmp(lar, "true", 4) == 0) ? 1 : 0;
                    }
                }

                free(json);
            }
        }
        fclose(f);
    }

    /* Write complete config file */
    f = fopen(SHADOW_CONFIG_PATH, "w");
    if (!f) {
        if (host_log) host_log("shadow_save_state: failed to open for writing");
        return;
    }

    fprintf(f, "{\n");
    if (patches_buf[0]) {
        fprintf(f, "  \"patches\": %s,\n", patches_buf);
    }
    fprintf(f, "  \"master_fx\": \"%s\",\n", master_fx);
    if (master_fx_path[0]) {
        fprintf(f, "  \"master_fx_path\": \"%s\",\n", master_fx_path);
    }
    if (master_fx_chain_buf[0]) {
        fprintf(f, "  \"master_fx_chain\": %s,\n", master_fx_chain_buf);
    }
    if (overlay_knobs_mode >= 0) {
        fprintf(f, "  \"overlay_knobs_mode\": %d,\n", overlay_knobs_mode);
    }
    if (resample_bridge_mode >= 0) {
        fprintf(f, "  \"resample_bridge_mode\": %d,\n", resample_bridge_mode);
    }
    if (link_audio_routing_saved >= 0) {
        fprintf(f, "  \"link_audio_routing\": %s,\n", link_audio_routing_saved ? "true" : "false");
    }
    /* Volume is always the real user-set level; mute/solo are separate flags */
    write_slot_array(f, "slot_volumes", SLOT_VOLUME, 0);
    write_slot_array(f, "slot_channels", SLOT_CHANNEL, 0);
    write_slot_array(f, "slot_forward_channels", SLOT_FORWARD, 0);
    write_slot_array(f, "slot_transpose", SLOT_TRANSPOSE, 0);
    write_slot_array(f, "slot_muted", SLOT_MUTED, 0);
    write_slot_array(f, "slot_soloed", SLOT_SOLOED, 1);
    fprintf(f, "}\n");
    fclose(f);
    chown_to_ableton(SHADOW_CONFIG_PATH);

    char ch[64], fwd[64], vol[96], mu[40], so[40];
    double v[SHADOW_CHAIN_INSTANCES];
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) v[i] = host_chain_slots[i].channel;
    slot_array_text(ch, sizeof(ch), v, SHADOW_CHAIN_INSTANCES, 0);
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) v[i] = host_chain_slots[i].forward_channel;
    slot_array_text(fwd, sizeof(fwd), v, SHADOW_CHAIN_INSTANCES, 0);
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) v[i] = host_chain_slots[i].volume;
    slot_array_text(vol, sizeof(vol), v, SHADOW_CHAIN_INSTANCES, 1);
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) v[i] = host_chain_slots[i].muted;
    slot_array_text(mu, sizeof(mu), v, SHADOW_CHAIN_INSTANCES, 0);
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) v[i] = host_chain_slots[i].soloed;
    slot_array_text(so, sizeof(so), v, SHADOW_CHAIN_INSTANCES, 0);
    char msg[400];
    snprintf(msg, sizeof(msg), "Saved slots: ch=[%s] fwd=[%s] vol=[%s] muted=[%s] soloed=[%s]",
             ch, fwd, vol, mu, so);
    if (host_log) host_log(msg);
}

/* ============================================================================
 * shadow_load_state - Read slot state from shadow_chain_config.json
 * ============================================================================ */

void shadow_load_state(void)
{
    FILE *f = fopen(SHADOW_CONFIG_PATH, "r");
    if (!f) {
        return;
    }

    fseek(f, 0, SEEK_END);
    long size = ftell(f);
    fseek(f, 0, SEEK_SET);

    /* 16 KB: eight slots' patches, six eight-wide arrays and a Master FX
     * chain no longer fit comfortably in the 8 KB this was. A file over the
     * cap is IGNORED whole, so the cap must sit well above any real one. */
    if (size <= 0 || size > 16384) {
        fclose(f);
        return;
    }

    char *json = malloc(size + 1);
    if (!json) {
        fclose(f);
        return;
    }

    size_t nread = fread(json, 1, size, f);
    json[nread] = '\0';
    fclose(f);

    /* The per-slot arrays (see write_slot_array). Each is applied only when
     * it is a whole answer for Move's four; an aux slot the file does not
     * mention keeps its default. */
    double v[SHADOW_CHAIN_INSTANCES];
    char text[160], msg[200];
    int n;

    if ((n = parse_slot_array(json, "\"slot_volumes\":", v, SHADOW_CHAIN_INSTANCES)) >= SHADOW_MOVE_SLOTS) {
        for (int i = 0; i < n; i++) {
            if (v[i] < 0.0) v[i] = 0.0;
            if (v[i] > 4.0) v[i] = 4.0;
            host_chain_slots[i].volume = (float)v[i];
        }
        slot_array_text(text, sizeof(text), v, n, 1);
        snprintf(msg, sizeof(msg), "Loaded slot volumes: [%s]", text);
        if (host_log) host_log(msg);
    }

    /* Receive channel */
    if ((n = parse_slot_array(json, "\"slot_channels\":", v, SHADOW_CHAIN_INSTANCES)) >= SHADOW_MOVE_SLOTS) {
        for (int i = 0; i < n; i++) host_chain_slots[i].channel = (int)v[i];
        slot_array_text(text, sizeof(text), v, n, 0);
        snprintf(msg, sizeof(msg), "Loaded slot channels: [%s]", text);
        if (host_log) host_log(msg);
    }

    if ((n = parse_slot_array(json, "\"slot_forward_channels\":", v, SHADOW_CHAIN_INSTANCES)) >= SHADOW_MOVE_SLOTS) {
        for (int i = 0; i < n; i++) host_chain_slots[i].forward_channel = (int)v[i];
        slot_array_text(text, sizeof(text), v, n, 0);
        snprintf(msg, sizeof(msg), "Loaded slot fwd channels: [%s]", text);
        if (host_log) host_log(msg);
    }

    if ((n = parse_slot_array(json, "\"slot_transpose\":", v, SHADOW_CHAIN_INSTANCES)) >= SHADOW_MOVE_SLOTS) {
        for (int i = 0; i < n; i++) {
            if (v[i] < -12) v[i] = -12;
            if (v[i] > 12) v[i] = 12;
            host_chain_slots[i].transpose = (int)v[i];
        }
        slot_array_text(text, sizeof(text), v, n, 0);
        snprintf(msg, sizeof(msg), "Loaded slot transpose: [%s]", text);
        if (host_log) host_log(msg);
    }

    if ((n = parse_slot_array(json, "\"slot_muted\":", v, SHADOW_CHAIN_INSTANCES)) >= SHADOW_MOVE_SLOTS) {
        for (int i = 0; i < n; i++) host_chain_slots[i].muted = (int)v[i];
        slot_array_text(text, sizeof(text), v, n, 0);
        snprintf(msg, sizeof(msg), "Loaded slot muted: [%s]", text);
        if (host_log) host_log(msg);
    }

    if ((n = parse_slot_array(json, "\"slot_soloed\":", v, SHADOW_CHAIN_INSTANCES)) >= SHADOW_MOVE_SLOTS) {
        for (int i = 0; i < n; i++) host_chain_slots[i].soloed = (int)v[i];
        slot_array_text(text, sizeof(text), v, n, 0);
        snprintf(msg, sizeof(msg), "Loaded slot soloed: [%s]", text);
        if (host_log) host_log(msg);
    }
    /* Counted over EVERY slot, whatever the file said: an aux slot soloed
     * before this load is still soloed. */
    *host_solo_count = 0;
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++)
        if (host_chain_slots[i].soloed) (*host_solo_count)++;

    free(json);

    /* One-time heal for the removed D-Bus mute/solo auto-correct.
     * That code could spuriously mute/solo a slot when Move announced a drum
     * kit/pad name ending in "muted"/"soloed"; the stray state was saved to
     * shadow_chain_config.json and restored every boot, silencing the slot's
     * audio across all projects. Clear any persisted mute/solo exactly once
     * (version-stamped flag) so users already stuck are healed by updating,
     * while deliberate mute/solo set on later boots is not wiped. */
    {
        static const char reset_flag[] =
            "/data/UserData/schwung/mute_solo_reset_v1_done";
        if (access(reset_flag, F_OK) != 0) {
            int had_state = 0;
            for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) {
                if (host_chain_slots[i].muted || host_chain_slots[i].soloed)
                    had_state = 1;
                host_chain_slots[i].muted = 0;
                host_chain_slots[i].soloed = 0;
            }
            *host_solo_count = 0;
            shadow_save_state();  /* persist cleared state so it survives reboot */
            FILE *flag = fopen(reset_flag, "w");
            if (flag) {
                fputs("1\n", flag);
                fclose(flag);
                chown_to_ableton(reset_flag);
            }
            if (host_log)
                host_log(had_state
                    ? "One-time mute/solo reset: cleared persisted slot mute/solo"
                    : "One-time mute/solo reset: none persisted, flag set");
        }
    }
}

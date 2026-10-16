/*
 * The Remote UI's Mixer writes through the web set ring, which lands in
 * shadow_direct_set_param -- a SECOND router beside the shadow_param serve, with
 * its own idea of which keys exist. It routed master_fx: and slot keys and
 * handed everything else to the slot's chain plugin, so a send RETURN written
 * from the browser reached slot 0's chain under a key the chain does not serve
 * and was dropped. Nothing logged it; the knob simply moved nothing.
 *
 * And a level that DID land was still lost at the next boot: send returns are
 * saved by shadow_ui, only when its own sendLevelsDirty is set, and a write it
 * did not make cannot set it. The shim now raises a UI flag for that.
 *
 * Pinned here against the REAL shadow_chain_mgmt.c (included, as
 * test_slot_clip_phase.c does -- file-static state, no seam), driving BOTH
 * writers through the one helper they now share:
 *
 *   - the web path sets `send<N>:return` / `send1:to_send2`, clamps, notifies,
 *     raises SHADOW_UI_FLAG_SEND_LEVELS_DIRTY, and never touches the chain;
 *   - every other `send<N>:` key is kept off the chain too;
 *   - ordinary component keys still reach the chain (the control case);
 *   - the shadow_param serve sets the same levels through the same helper and
 *     does NOT raise the flag: its writer is shadow_ui itself, whose set
 *     restore writes only the fields a set's file holds -- a save triggered
 *     from there would write every level and turn the deliberately ABSENT
 *     ones into stored values carried over from the previous set;
 *   - a web `slot:volume` / `slot:muted` write is mirrored into /schwung-ui,
 *     which is what the Mixer reads back instead of spending param reads.
 */
#include <stdio.h>
#include <string.h>
#include <math.h>

#include "shadow_chain_mgmt.c"

static int failures = 0;
static int checks = 0;
#define CHECK(cond, ...) do { \
    checks++; \
    if (!(cond)) { failures++; printf("FAIL: "); printf(__VA_ARGS__); \
                   printf("\n      (%s:%d: %s)\n", __FILE__, __LINE__, #cond); } \
} while (0)

/* ------------------------------------------------------------------ fakes */
int move_model_get(move_model_t *out) { memset(out, 0, sizeof *out); return 0; }
double shadow_transport_beat_position(void) { return -1.0; }
int move_model_sync_active(void) { return 0; }
void shadow_set_pages_ack_aligned(uint32_t gen) { (void)gen; }
uint32_t shadow_set_pages_published_gen(void) { return 0; }
const clip_state_t *clip_state_current(void) { return NULL; }
int shadow_transport_pulses = 0;
char sampler_current_set_name[128];
char sampler_current_set_uuid[64];
_Atomic int schwung_trace_on = 0;
uint32_t schwung_trace_intern(const char *name) { (void)name; return 0; }
uint64_t schwung_trace_now_ns(void) { return 0; }
void schwung_trace_span_explicit(uint32_t a, uint64_t b, uint64_t c, uint64_t d, uint64_t e)
{ (void)a; (void)b; (void)c; (void)d; (void)e; }
int set_page_current = 0;
int set_page_read_persisted(void) { return 0; }
void shadow_batch_migrate_sets(void) {}
int shadow_load_config_from_dir(const char *dir) { (void)dir; return 0; }
void shadow_save_state(void) {}
void shadow_request_save_state(void) {}
int shadow_chain_midi_inject(const uint8_t *msg, int len) { (void)msg; (void)len; return 0; }
void unified_log(const char *source, int level, const char *fmt, ...)
{ (void)source; (void)level; (void)fmt; }

/* The slot's chain plugin: records what reached it. */
static int chain_sets = 0;
static char chain_last_key[128];
static void fake_chain_set_param(void *inst, const char *key, const char *val)
{
    (void)inst; (void)val;
    chain_sets++;
    snprintf(chain_last_key, sizeof chain_last_key, "%s", key);
}
static plugin_api_v2_t fake_chain_api;

/* A module in a send position: records what reached it. */
static int send_fx_sets = 0;
static char send_fx_last_key[128];
static char send_fx_last_value[64];
static void fake_send_fx_set_param(void *inst, const char *key, const char *val)
{
    (void)inst;
    send_fx_sets++;
    snprintf(send_fx_last_key, sizeof send_fx_last_key, "%s", key);
    snprintf(send_fx_last_value, sizeof send_fx_last_value, "%s", val);
}
static audio_fx_api_v2_t fake_send_fx_api;

/* The web notify ring: records what the browser would be told. */
static int notifies = 0;
static char notify_last_key[128];
static char notify_last_value[64];
static void fake_on_param_changed(uint8_t slot, const char *key, const char *value)
{
    (void)slot;
    notifies++;
    snprintf(notify_last_key, sizeof notify_last_key, "%s", key);
    snprintf(notify_last_value, sizeof notify_last_value, "%s", value);
}

static shadow_control_t ctrl;
static shadow_control_t *ctrl_ptr = &ctrl;
static shadow_param_t param;
static shadow_param_t *param_ptr = &param;
static shadow_ui_state_t ui_state;
static shadow_ui_state_t *ui_state_ptr = &ui_state;

static int flag_raised(void)
{
    return (ctrl.ui_flags_ext &
            (SHADOW_UI_FLAG_SEND_LEVELS_DIRTY >> SHADOW_UI_FLAG_EXT_SHIFT)) != 0;
}

static void reset(void)
{
    chain_sets = 0; chain_last_key[0] = '\0';
    notifies = 0; notify_last_key[0] = '\0'; notify_last_value[0] = '\0';
    ctrl.ui_flags_ext = 0;
}

/* One GET through the serve; answers into param.value. */
static void serve_get(uint8_t slot, const char *key)
{
    static uint32_t gid = 50000;
    memset(&param, 0, sizeof param);
    param.slot = slot;
    snprintf(param.key, sizeof param.key, "%s", key);
    param.request_id = ++gid;
    __atomic_store_n(&param.request_type, 2, __ATOMIC_RELEASE);
    shadow_inprocess_handle_param_request();
}

/* One SET through the shadow_param serve, the way shadow_ui sends it. */
static void serve_set(uint8_t slot, const char *key, const char *value)
{
    static uint32_t id = 100;
    memset(&param, 0, sizeof param);
    param.slot = slot;
    snprintf(param.key, sizeof param.key, "%s", key);
    snprintf(param.value, sizeof param.value, "%s", value);
    param.request_id = ++id;
    __atomic_store_n(&param.request_type, 1, __ATOMIC_RELEASE);
    shadow_inprocess_handle_param_request();
}

int main(void)
{
    chain_mgmt_host_t h;
    memset(&h, 0, sizeof h);
    h.shadow_control_ptr = &ctrl_ptr;
    h.shadow_param_ptr = &param_ptr;
    h.shadow_ui_state_ptr = &ui_state_ptr;
    h.on_param_changed = fake_on_param_changed;
    chain_mgmt_init(&h);

    memset(&fake_chain_api, 0, sizeof fake_chain_api);
    fake_chain_api.set_param = fake_chain_set_param;
    memset(&fake_send_fx_api, 0, sizeof fake_send_fx_api);
    fake_send_fx_api.set_param = fake_send_fx_set_param;
    shadow_plugin_v2 = &fake_chain_api;
    shadow_chain_slots[0].active = 1;
    shadow_chain_slots[0].instance = (void *)&fake_chain_api;

    /* ---- the web path: the two levels ---------------------------------- */
    reset();
    shadow_send_return_level[0] = 0;
    shadow_direct_set_param(0, "send1:return", "90");
    CHECK(shadow_send_return_level[0] == 90, "web send1:return sets Send A's return (got %d)",
          shadow_send_return_level[0]);
    CHECK(chain_sets == 0, "send1:return must NOT reach the slot's chain (got '%s')", chain_last_key);
    CHECK(flag_raised(), "a web-written return raises SEND_LEVELS_DIRTY so shadow_ui saves it");
    CHECK(notifies == 1 && !strcmp(notify_last_key, "send1:return") &&
          !strcmp(notify_last_value, "90"),
          "the browser is told (notifies=%d key='%s')", notifies, notify_last_key);

    reset();
    shadow_direct_set_param(0, "send2:return", "999");
    CHECK(shadow_send_return_level[1] == BUS_MIX_SEND_LEVEL_MAX,
          "a return is clamped to the send-level range (got %d)", shadow_send_return_level[1]);
    shadow_direct_set_param(0, "send2:return", "-5");
    CHECK(shadow_send_return_level[1] == 0, "and from below (got %d)", shadow_send_return_level[1]);

    reset();
    shadow_send_a_to_b = 0;
    shadow_direct_set_param(0, "send1:to_send2", "40");
    CHECK(shadow_send_a_to_b == 40, "web send1:to_send2 sets the A->B feed (got %d)", shadow_send_a_to_b);
    CHECK(chain_sets == 0 && flag_raised(), "A->B: off the chain, flag raised");

    /* Only A feeds B: there is no send2:to_send2, and it must not be invented
     * or leak anywhere. */
    reset();
    shadow_direct_set_param(0, "send2:to_send2", "77");
    CHECK(shadow_send_a_to_b == 40, "send2:to_send2 names nothing (A->B still %d)", shadow_send_a_to_b);
    CHECK(chain_sets == 0 && notifies == 0 && !flag_raised(),
          "send2:to_send2: no chain write, no notify, no flag");

    /* Every other send-bus key stays off the slot's chain too. */
    reset();
    shadow_direct_set_param(0, "send1:fx1:cutoff", "0.5");
    shadow_direct_set_param(0, "send2:fx:move", "1>2");
    CHECK(chain_sets == 0, "send-bus position / shape keys never reach the slot's chain (got '%s')",
          chain_last_key);

    /* A LOADED send position's parameter is set, as a Master FX one is: the
     * Remote UI's Send A/B tabs and the Mixer's returns strip write these. */
    shadow_send_fx_slots[0][1].api = &fake_send_fx_api;
    shadow_send_fx_slots[0][1].instance = (void *)&fake_send_fx_api;
    reset(); send_fx_sets = 0;
    shadow_direct_set_param(0, "send1:fx2:mix", "0.7");
    CHECK(send_fx_sets == 1 && !strcmp(send_fx_last_key, "mix") && !strcmp(send_fx_last_value, "0.7"),
          "send1:fx2:mix reaches Send A position 2 as 'mix' (sets=%d key='%s')",
          send_fx_sets, send_fx_last_key);
    CHECK(chain_sets == 0, "and never the slot's chain");
    CHECK(notifies == 1 && !strcmp(notify_last_key, "send1:fx2:mix"),
          "the browser is told (notifies=%d key='%s')", notifies, notify_last_key);
    CHECK(!flag_raised(), "a position parameter raises no LEVELS flag (the autosave reads it back)");

    reset(); send_fx_sets = 0;
    shadow_direct_set_param(0, "send1:fx2:bypassed", "1");
    CHECK(shadow_send_fx_slots[0][1].bypassed == 1 && send_fx_sets == 0,
          "bypassed is host-side, never handed to the module");
    shadow_send_fx_slots[0][1].bypassed = 0;

    /* What is LOADED is not this ring's to change. */
    reset(); send_fx_sets = 0;
    shadow_direct_set_param(0, "send1:fx2:module", "");
    shadow_direct_set_param(0, "send1:fx2:name", "x");
    CHECK(send_fx_sets == 0 && notifies == 0 && shadow_send_fx_slots[0][1].instance,
          "module / name writes are left to the shadow_param path (sets=%d)", send_fx_sets);
    shadow_send_fx_slots[0][1].api = NULL;
    shadow_send_fx_slots[0][1].instance = NULL;

    /* The control: an ordinary component key still goes to the chain. */
    reset();
    shadow_direct_set_param(0, "fx1:mix", "0.25");
    CHECK(chain_sets == 1 && !strcmp(chain_last_key, "fx1:mix"),
          "a component key still reaches the chain (sets=%d key='%s')", chain_sets, chain_last_key);
    CHECK(!flag_raised(), "and raises no send-level flag");

    /* ---- the serve: the SAME helper ------------------------------------ */
    reset();
    serve_set(0, "send2:return", "64");
    CHECK(shadow_send_return_level[1] == 64, "serve send2:return sets the level (got %d)",
          shadow_send_return_level[1]);
    CHECK(!flag_raised(), "the serve does NOT raise the flag (shadow_ui owns its own dirty state)");
    CHECK(chain_sets == 0, "the serve keeps it off the chain");
    CHECK(param.response_ready == 1 && param.error == 0, "and answers the request");

    reset();
    serve_set(0, "send1:to_send2", "12");
    CHECK(shadow_send_a_to_b == 12, "serve send1:to_send2 sets the feed (got %d)", shadow_send_a_to_b);

    /* A GET is still a read: it answers the level and writes nothing. */
    reset();
    memset(&param, 0, sizeof param);
    param.slot = 0;
    snprintf(param.key, sizeof param.key, "send2:return");
    param.request_id = 7777;
    __atomic_store_n(&param.request_type, 2, __ATOMIC_RELEASE);
    shadow_inprocess_handle_param_request();
    CHECK(param.response_ready == 1 && !strcmp(param.value, "64"),
          "a GET of send2:return answers 64 (got '%s')", param.value);
    CHECK(!flag_raised(), "a GET raises no flag");

    /* ---- what the Mixer reads back: /schwung-ui ------------------------ */
    reset();
    shadow_direct_set_param(2, "slot:volume", "0.5");
    CHECK(ui_state.slot_volumes[2] == 50, "a web slot:volume is mirrored into /schwung-ui (got %u)",
          ui_state.slot_volumes[2]);
    shadow_direct_set_param(2, "slot:muted", "1");
    CHECK(ui_state.slot_muted[2] == 1, "and slot:muted (got %u)", ui_state.slot_muted[2]);
    CHECK(notifies == 2, "both are notified (got %d)", notifies);

    /* ---- a return's MUTE: both routers, and read back ---------------- */
    reset();
    shadow_send_return_level[0] = 100;
    shadow_direct_set_param(0, "send1:return_muted", "1");
    CHECK(shadow_send_return_muted[0] == 1 && shadow_send_return_level[0] == 100,
          "web send1:return_muted mutes Send A and leaves its level (muted=%d level=%d)",
          shadow_send_return_muted[0], shadow_send_return_level[0]);
    CHECK(flag_raised() && notifies == 1 && chain_sets == 0,
          "flagged for saving, told to the browser, kept off the chain");
    serve_get(0, "send1:return_muted");
    CHECK(param.error == 0 && !strcmp(param.value, "1"), "and reads back 1 (got '%s')", param.value);
    reset();
    serve_set(0, "send2:return_muted", "1");
    CHECK(shadow_send_return_muted[1] == 1 && !flag_raised(),
          "the serve sets Send B's mute too, without the web flag");
    serve_set(0, "send2:return_muted", "0");
    CHECK(shadow_send_return_muted[1] == 0, "and clears it");

    /* ---- the MASTER BUS's own level / mute / chain bypass -------------- */
    static audio_fx_api_v2_t fake_mfx_api;
    memset(&fake_mfx_api, 0, sizeof fake_mfx_api);
    fake_mfx_api.set_param = fake_send_fx_set_param;
    shadow_master_fx_slots[0].api = &fake_mfx_api;
    shadow_master_fx_slots[0].instance = (void *)&fake_mfx_api;

    reset(); send_fx_sets = 0;
    shadow_direct_set_param(0, "master_fx:level", "0.5");
    CHECK(fabsf(shadow_master_bus_level - 0.5f) < 1e-6f, "web master_fx:level sets the bus level (got %f)",
          (double)shadow_master_bus_level);
    CHECK(send_fx_sets == 0 && chain_sets == 0,
          "and never reaches Master FX position 1 as its module's parameter (sets=%d)", send_fx_sets);
    CHECK(flag_raised() && notifies == 1 && !strcmp(notify_last_key, "master_fx:level"),
          "flagged for saving and told to the browser");
    shadow_direct_set_param(0, "master_fx:level", "9");
    CHECK(shadow_master_bus_level == MASTER_BUS_LEVEL_MAX, "clamped to +6 dB (got %f)",
          (double)shadow_master_bus_level);
    shadow_direct_set_param(0, "master_fx:level", "nan");
    CHECK(shadow_master_bus_level == 0.0f, "a NaN is silence, not a NaN gain");

    reset(); send_fx_sets = 0;
    serve_set(0, "master_fx:muted", "1");
    serve_set(0, "master_fx:chain_bypassed", "1");
    serve_set(0, "master_fx:level", "1.25");
    CHECK(shadow_master_bus_muted == 1 && shadow_master_fx_chain_bypassed == 1 &&
          fabsf(shadow_master_bus_level - 1.25f) < 1e-6f && send_fx_sets == 0,
          "the serve sets all three, and none reaches the module (sets=%d)", send_fx_sets);
    serve_get(0, "master_fx:level");
    CHECK(!strcmp(param.value, "1.2500"), "master_fx:level reads back (got '%s')", param.value);
    serve_get(0, "master_fx:muted");
    CHECK(!strcmp(param.value, "1"), "master_fx:muted reads back (got '%s')", param.value);
    serve_get(0, "master_fx:chain_bypassed");
    CHECK(!strcmp(param.value, "1"), "master_fx:chain_bypassed reads back (got '%s')", param.value);
    shadow_master_fx_slots[0].api = NULL;
    shadow_master_fx_slots[0].instance = NULL;

    /* ---- the boot restore reads them from send_levels.json ------------- */
    {
        char dir[] = "/tmp/test_web_send_levelsXXXXXX";
        if (mkdtemp(dir)) {
            char path[300];
            snprintf(path, sizeof path, "%s/send_levels.json", dir);
            FILE *f = fopen(path, "w");
            fputs("{\n  \"send1_return\": 64,\n  \"send2_return_muted\": 1,\n"
                  "  \"master_level\": 0.8,\n  \"master_muted\": 0,\n  \"master_fx_bypassed\": 0\n}\n", f);
            fclose(f);
            shadow_send_return_muted[0] = 1;   /* absent in the file: kept */
            shadow_send_return_muted[1] = 0;
            shadow_master_bus_muted = 1;
            shadow_master_fx_chain_bypassed = 1;
            shadow_send_levels_restore(dir);
            CHECK(shadow_send_return_level[0] == 64 && shadow_send_return_muted[1] == 1 &&
                  shadow_send_return_muted[0] == 1, "returns and their mutes restored");
            CHECK(fabsf(shadow_master_bus_level - 0.8f) < 1e-6f && shadow_master_bus_muted == 0 &&
                  shadow_master_fx_chain_bypassed == 0, "the master bus restored (level %f)",
                  (double)shadow_master_bus_level);
            unlink(path);
            rmdir(dir);
        }
    }

    printf("test_web_send_levels: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
}

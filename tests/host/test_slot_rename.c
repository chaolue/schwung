/*
 * A slot's NAME, and renames asked for by the Remote UI -- the shim's half.
 *
 * The shim's copy of a slot's name (patch_name) is what /schwung-ui publishes,
 * and it used to change only when a patch LOADED: a preset saved on Move under
 * a new name kept its old name in the browser until the next boot. shadow_ui
 * now publishes it with `slot:name`.
 *
 * A rename rewrites the preset file, which only shadow_ui can do, so the
 * browser's request is HELD here per slot (`slot:rename_request`
 * "<seq>:<name>") with SHADOW_UI_FLAG_SLOT_RENAME raised, and shadow_ui's
 * answer (`slot:rename_status` "<seq>:...") clears exactly the request it
 * names -- never a newer one that arrived meanwhile.
 *
 * Against the REAL shadow_chain_mgmt.c, included as test_web_send_levels.c
 * does, through both routers (web set ring and shadow_param serve).
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

static int chain_sets = 0;
static char chain_last_key[128];
static void fake_chain_set_param(void *inst, const char *key, const char *val)
{
    (void)inst; (void)val;
    chain_sets++;
    snprintf(chain_last_key, sizeof chain_last_key, "%s", key);
}
static plugin_api_v2_t fake_chain_api;

static shadow_control_t ctrl;
static shadow_control_t *ctrl_ptr = &ctrl;
static shadow_param_t param;
static shadow_param_t *param_ptr = &param;
static shadow_ui_state_t ui_state;
static shadow_ui_state_t *ui_state_ptr = &ui_state;

static int rename_flag(void)
{
    return (ctrl.ui_flags_ext &
            (SHADOW_UI_FLAG_SLOT_RENAME >> SHADOW_UI_FLAG_EXT_SHIFT)) != 0;
}

/* One request through the shadow_param serve, as shadow_ui makes it. */
static void serve(uint8_t req_type, uint8_t slot, const char *key, const char *value)
{
    static uint32_t id = 500;
    memset(&param, 0, sizeof param);
    param.slot = slot;
    snprintf(param.key, sizeof param.key, "%s", key);
    if (value) snprintf(param.value, sizeof param.value, "%s", value);
    param.request_id = ++id;
    __atomic_store_n(&param.request_type, req_type, __ATOMIC_RELEASE);
    shadow_inprocess_handle_param_request();
}

static const char *get(uint8_t slot, const char *key)
{
    serve(2, slot, key, NULL);
    return param.value;
}

int main(void)
{
    chain_mgmt_host_t h;
    memset(&h, 0, sizeof h);
    h.shadow_control_ptr = &ctrl_ptr;
    h.shadow_param_ptr = &param_ptr;
    h.shadow_ui_state_ptr = &ui_state_ptr;
    chain_mgmt_init(&h);

    memset(&fake_chain_api, 0, sizeof fake_chain_api);
    fake_chain_api.set_param = fake_chain_set_param;
    shadow_plugin_v2 = &fake_chain_api;
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) {
        shadow_chain_slots[i].active = 1;
        shadow_chain_slots[i].instance = (void *)&fake_chain_api;
    }

    /* ---- slot:name: the shim's copy, and what /schwung-ui publishes ---- */
    serve(1, 2, "slot:name", "Choir Pad");
    CHECK(!strcmp(shadow_chain_slots[2].patch_name, "Choir Pad"), "slot:name sets patch_name (got '%s')",
          shadow_chain_slots[2].patch_name);
    CHECK(!strcmp(shadow_ui_state_name(&ui_state, 2), "Choir Pad"), "and /schwung-ui's name (got '%s')",
          shadow_ui_state_name(&ui_state, 2));
    CHECK(!strcmp(get(2, "slot:name"), "Choir Pad"), "a GET answers it");
    CHECK(chain_sets == 0, "slot:name never reaches the chain (got '%s')", chain_last_key);

    serve(1, 6, "slot:name", "Aux Lead");
    CHECK(!strcmp(shadow_ui_state_name(&ui_state, 6), "Aux Lead"),
          "an aux slot's name lands in the aux block (got '%s')", shadow_ui_state_name(&ui_state, 6));

    /* Quotes, backslashes and control bytes cannot ride in a preset file or
     * the hand-rolled JSON readers: dropped, not escaped. */
    serve(1, 1, "slot:name", "Say \"hi\"\\\tnow");
    CHECK(!strcmp(shadow_chain_slots[1].patch_name, "Say hinow"), "unsafe bytes dropped (got '%s')",
          shadow_chain_slots[1].patch_name);
    char longname[200];
    memset(longname, 'x', sizeof longname - 1);
    longname[sizeof longname - 1] = '\0';
    serve(1, 1, "slot:name", longname);
    CHECK(strlen(shadow_chain_slots[1].patch_name) == sizeof(shadow_chain_slots[1].patch_name) - 1,
          "an over-long name is cut to the buffer, terminated (len %zu)",
          strlen(shadow_chain_slots[1].patch_name));

    /* ---- the rename mailbox, written by the WEB path ------------------- */
    ctrl.ui_flags_ext = 0;
    chain_sets = 0;
    shadow_direct_set_param(3, "slot:rename_request", "41:Strings");
    CHECK(rename_flag(), "a web rename request raises SHADOW_UI_FLAG_SLOT_RENAME");
    CHECK(chain_sets == 0, "and never reaches the chain");
    CHECK(!strcmp(get(3, "slot:rename_request"), "41:Strings"), "shadow_ui reads it back (got '%s')", param.value);
    CHECK(!strcmp(get(2, "slot:rename_request"), ""), "another slot has none (got '%s')", param.value);

    /* A malformed request is dropped, flag and all. */
    ctrl.ui_flags_ext = 0;
    shadow_direct_set_param(4, "slot:rename_request", "Strings");
    CHECK(!rename_flag() && !strcmp(get(4, "slot:rename_request"), ""), "no '<seq>:' -> dropped");

    /* The answer clears ONLY the request it names. */
    shadow_direct_set_param(3, "slot:rename_request", "42:Strings 2");   /* a newer one arrived */
    serve(1, 3, "slot:rename_status", "41:ok");
    CHECK(!strcmp(get(3, "slot:rename_request"), "42:Strings 2"),
          "an answer to 41 leaves the newer 42 pending (got '%s')", param.value);
    serve(1, 3, "slot:rename_status", "42:err:A preset named \"Strings 2\" already exists.");
    CHECK(!strcmp(get(3, "slot:rename_request"), ""), "the answer to 42 clears it (got '%s')", param.value);
    CHECK(chain_sets == 0, "status writes never reach the chain (got '%s')", chain_last_key);

    printf("test_slot_rename: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
}

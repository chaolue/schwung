/*
 * test_aux_slots.c -- the AUX slots (chain slots 4-7, no Move track behind
 * them): what they cost the formats that existed before them.
 *
 *   1. shadow_ui_state_t: the v2 fields did not move, the aux block is
 *      APPENDED, every slot reads back through the accessors, and a reader
 *      facing a v2 writer trusts four slots whatever slot_count says.
 *   2. The shim's state file (shadow_state.c): eight values per array, in
 *      the "[a, b, c, d, ...]" shape a four-value sscanf still parses -- so a
 *      DOWNGRADE reads Move's four -- and a file from before the aux slots
 *      loads Move's four and leaves the aux slots alone.
 *   3. A set's chain config (shadow_set_pages.c): a set saved before the aux
 *      slots resets them to their defaults instead of inheriting the previous
 *      set's -- above all its SOLO, which would silence every track -- and the
 *      solo count agrees with the slots afterwards.
 *
 * shadow_state.c is compiled INTO this file so its config path can be pointed
 * at a temp file; shadow_set_pages.c is linked as is.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <stdint.h>

#include "shadow_state.h"
static char aux_state_path[512];
#undef SHADOW_CONFIG_PATH
#define SHADOW_CONFIG_PATH aux_state_path
#include "../../src/host/shadow_state.c"

#include "shadow_constants.h"
#include "shadow_set_pages.h"

/* shadow_set_pages.c's model-sync calls, which nothing here reaches. */
int move_model_sync_active(void) { return 0; }
uint32_t move_model_sync_gen(void) { return 0; }
int move_model_sync_settled(void) { return 0; }
int move_model_sync_misaligned(void) { return 0; }

static int fails = 0;
#define CHECK(cond, ...) do { if (!(cond)) { fails++; \
    fprintf(stderr, "FAIL %s:%d: ", __FILE__, __LINE__); \
    fprintf(stderr, __VA_ARGS__); fputc('\n', stderr); } } while (0)

static shadow_chain_slot_t slots[SHADOW_CHAIN_INSTANCES];
static int solo_count_state;
static volatile int solo_count_pages;
static int ui_refreshes;

static void quiet_log(const char *msg) { (void)msg; }
static int parse_channel(int ch) { return ch == 0 ? -1 : (ch >= 1 && ch <= 16) ? ch - 1 : ch; }
static void ui_refresh(void) { ui_refreshes++; }
static int no_command(const char *const argv[]) { (void)argv; return 0; }   /* chown */

static void write_file(const char *path, const char *text) {
    FILE *f = fopen(path, "w");
    if (!f) { perror(path); exit(2); }
    fputs(text, f);
    fclose(f);
}

static char *read_file(const char *path) {
    FILE *f = fopen(path, "r");
    if (!f) return NULL;
    static char buf[16384];
    size_t n = fread(buf, 1, sizeof(buf) - 1, f);
    buf[n] = '\0';
    fclose(f);
    return buf;
}

static void test_ui_state_layout(void) {
    CHECK(SHADOW_UI_SLOTS == SHADOW_CHAIN_INSTANCES, "the UI slot count is the chain slot count");
    CHECK(SHADOW_MOVE_SLOTS == 4, "Move has four tracks");
    CHECK(sizeof(shadow_ui_state_t) <= SHADOW_UI_BUFFER_SIZE, "the struct fits its segment");

    static shadow_ui_state_t st;
    memset(&st, 0, sizeof st);
    st.version = SHADOW_UI_STATE_VERSION;
    st.slot_count = SHADOW_UI_SLOTS;
    for (int i = 0; i < SHADOW_UI_SLOTS; i++) {
        *shadow_ui_state_channel(&st, i) = (uint8_t)(i + 1);
        *shadow_ui_state_volume(&st, i) = (uint16_t)(100 + i);
        *shadow_ui_state_forward(&st, i) = (int8_t)(i - 2);
        *shadow_ui_state_muted(&st, i) = (uint8_t)(i & 1);
        *shadow_ui_state_soloed(&st, i) = (uint8_t)(i == 6);
        snprintf(shadow_ui_state_name(&st, i), SHADOW_UI_NAME_LEN, "slot-%d", i);
    }
    for (int i = 0; i < SHADOW_UI_SLOTS; i++) {
        char want[16];
        snprintf(want, sizeof want, "slot-%d", i);
        CHECK(*shadow_ui_state_channel(&st, i) == i + 1 &&
              *shadow_ui_state_volume(&st, i) == 100 + i &&
              *shadow_ui_state_forward(&st, i) == i - 2 &&
              *shadow_ui_state_muted(&st, i) == (i & 1) &&
              *shadow_ui_state_soloed(&st, i) == (i == 6) &&
              strcmp(shadow_ui_state_name(&st, i), want) == 0,
              "slot %d does not read back through the accessors", i);
    }
    /* The v2 arrays hold exactly Move's four -- what an old shadow_ui reads. */
    CHECK(st.slot_channels[3] == 4 && strcmp(st.slot_names[3], "slot-3") == 0 &&
          st.aux_channels[0] == 5 && strcmp(st.aux_names[3], "slot-7") == 0,
          "slots 0-3 must live in the v2 arrays and 4-7 in the aux block");

    CHECK(shadow_ui_state_slot_count(&st) == SHADOW_UI_SLOTS, "a v3 writer is trusted for all slots");
    st.version = 2;
    CHECK(shadow_ui_state_slot_count(&st) == SHADOW_UI_STATE_BASE_SLOTS,
          "a v2 writer has no aux block, whatever slot_count says");
    st.version = SHADOW_UI_STATE_VERSION;
    st.slot_count = 4;
    CHECK(shadow_ui_state_slot_count(&st) == 4, "a writer publishing four is read as four");
    st.slot_count = 0;
    CHECK(shadow_ui_state_slot_count(&st) == SHADOW_UI_SLOTS, "an unset count falls back to the cap");
}

static void reset_slots(void) {
    memset(slots, 0, sizeof slots);
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) {
        slots[i].channel = i;
        slots[i].volume = 1.0f;
        slots[i].forward_channel = -1;
    }
}

static void test_state_file(void) {
    state_host_t sh = { quiet_log, slots, &solo_count_state };
    state_init(&sh);
    reset_slots();
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) {
        slots[i].volume = 0.25f + 0.125f * i;
        slots[i].channel = 15 - i;
        slots[i].forward_channel = i - 2;
        slots[i].transpose = i - 4;
    }
    write_file(aux_state_path, "{}\n");
    shadow_save_state();
    const char *text = read_file(aux_state_path);
    CHECK(text != NULL, "the state file was not written");
    if (!text) return;

    /* Eight values, in the shape a build from before the aux slots parses. */
    const char *vol = strstr(text, "\"slot_volumes\": [");
    CHECK(vol != NULL, "slot_volumes is missing: %s", text);
    if (vol) {
        float v[4];
        int n = sscanf(strchr(vol, '['), "[%f, %f, %f, %f]", &v[0], &v[1], &v[2], &v[3]);
        CHECK(n == 4 && v[0] == 0.25f && v[3] == 0.625f,
              "a four-value parse (a downgrade) must still read Move's four, got %d", n);
        int commas = 0;
        for (const char *p = vol; *p && *p != ']'; p++) commas += (*p == ',');
        CHECK(commas == SHADOW_CHAIN_INSTANCES - 1, "slot_volumes holds %d values", commas + 1);
    }

    /* Round trip: every slot comes back. */
    reset_slots();
    shadow_load_state();
    for (int i = 0; i < SHADOW_CHAIN_INSTANCES; i++) {
        CHECK(slots[i].volume == 0.25f + 0.125f * i && slots[i].channel == 15 - i &&
              slots[i].forward_channel == i - 2 && slots[i].transpose == i - 4,
              "slot %d did not round-trip (vol %.3f ch %d fwd %d tr %d)", i,
              slots[i].volume, slots[i].channel, slots[i].forward_channel, slots[i].transpose);
    }

    /* A file from before the aux slots: Move's four load, the aux slots keep
     * what they had. */
    write_file(aux_state_path,
        "{\n"
        "  \"slot_volumes\": [0.500, 0.600, 0.700, 0.800],\n"
        "  \"slot_channels\": [9, 10, 11, 12],\n"
        "  \"slot_forward_channels\": [-1, -1, -2, 3],\n"
        "  \"slot_transpose\": [1, 2, 3, 4],\n"
        "  \"slot_muted\": [0, 0, 0, 0],\n"
        "  \"slot_soloed\": [0, 0, 0, 0]\n"
        "}\n");
    reset_slots();
    for (int i = SHADOW_MOVE_SLOTS; i < SHADOW_CHAIN_INSTANCES; i++) {
        slots[i].volume = 0.3f;
        slots[i].channel = 20 + i;
        slots[i].transpose = -7;
    }
    shadow_load_state();
    CHECK(slots[0].volume == 0.5f && slots[3].volume == 0.8f && slots[3].channel == 12 &&
          slots[3].forward_channel == 3 && slots[3].transpose == 4,
          "a four-slot file must still load Move's four");
    for (int i = SHADOW_MOVE_SLOTS; i < SHADOW_CHAIN_INSTANCES; i++) {
        CHECK(slots[i].volume == 0.3f && slots[i].channel == 20 + i && slots[i].transpose == -7,
              "aux slot %d was touched by a four-slot file", i);
    }

    /* Fewer than Move's four is not a whole answer: nothing is applied. */
    write_file(aux_state_path, "{\n  \"slot_volumes\": [0.100, 0.200]\n}\n");
    reset_slots();
    shadow_load_state();
    CHECK(slots[0].volume == 1.0f, "a two-value array must be ignored, as the old parse did");
}

static void test_set_config(const char *dir) {
    set_pages_host_t ph;
    memset(&ph, 0, sizeof ph);
    ph.log = quiet_log;
    ph.ui_state_refresh = ui_refresh;
    ph.run_command = no_command;
    ph.chain_parse_channel = parse_channel;
    ph.chain_slots = slots;
    ph.solo_count = &solo_count_pages;
    set_pages_init(&ph);

    char path[600];
    snprintf(path, sizeof path, "%s/" SHADOW_CHAIN_CONFIG_FILENAME, dir);

    /* The previous set left aux slot 5 soloed, quiet and on channel 10. */
    reset_slots();
    slots[5].soloed = 1;
    slots[5].volume = 0.2f;
    slots[5].channel = 9;
    slots[6].muted = 1;
    snprintf(slots[5].patch_name, sizeof slots[5].patch_name, "Old Set Pad");
    solo_count_pages = 1;

    /* This set was saved before the aux slots existed: four entries. */
    write_file(path,
        "{\n  \"slots\": [\n"
        "    {\"name\": \"A\", \"channel\": 1, \"volume\": 0.900, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0},\n"
        "    {\"name\": \"B\", \"channel\": 2, \"volume\": 0.800, \"forward_channel\": -1, \"muted\": 1, \"soloed\": 0},\n"
        "    {\"name\": \"C\", \"channel\": 3, \"volume\": 0.700, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0},\n"
        "    {\"name\": \"D\", \"channel\": 4, \"volume\": 0.600, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0}\n"
        "  ]\n}\n");
    CHECK(shadow_load_config_from_dir(dir) == 1, "the four-slot config did not load");
    CHECK(slots[1].muted == 1 && slots[3].volume == 0.6f && strcmp(slots[0].patch_name, "A") == 0,
          "Move's four must load from a four-slot config");
    for (int i = SHADOW_MOVE_SLOTS; i < SHADOW_CHAIN_INSTANCES; i++) {
        CHECK(slots[i].soloed == 0 && slots[i].muted == 0 && slots[i].volume == 1.0f &&
              slots[i].channel == i && slots[i].forward_channel == -1 &&
              slots[i].patch_name[0] == '\0',
              "aux slot %d kept the previous set's state (solo %d mute %d vol %.2f ch %d)", i,
              slots[i].soloed, slots[i].muted, slots[i].volume, slots[i].channel);
    }
    CHECK(solo_count_pages == 0, "the solo count says %d with nothing soloed", solo_count_pages);

    /* A set saved with all eight: the aux slots load like any other. */
    write_file(path,
        "{\n  \"slots\": [\n"
        "    {\"name\": \"A\", \"channel\": 1, \"volume\": 1.000, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0},\n"
        "    {\"name\": \"B\", \"channel\": 2, \"volume\": 1.000, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0},\n"
        "    {\"name\": \"C\", \"channel\": 3, \"volume\": 1.000, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0},\n"
        "    {\"name\": \"D\", \"channel\": 4, \"volume\": 1.000, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0},\n"
        "    {\"name\": \"E\", \"channel\": 5, \"volume\": 1.000, \"forward_channel\": -1, \"muted\": 0, \"soloed\": 0},\n"
        "    {\"name\": \"F\", \"channel\": 9, \"volume\": 0.500, \"forward_channel\": 2, \"muted\": 0, \"soloed\": 1},\n"
        "    {\"name\": \"G\", \"channel\": 7, \"volume\": 1.000, \"forward_channel\": -1, \"muted\": 1, \"soloed\": 0},\n"
        "    {\"name\": \"H\", \"channel\": 0, \"volume\": 1.000, \"forward_channel\": -2, \"muted\": 0, \"soloed\": 0}\n"
        "  ]\n}\n");
    reset_slots();
    CHECK(shadow_load_config_from_dir(dir) == 1, "the eight-slot config did not load");
    CHECK(slots[5].soloed == 1 && slots[5].volume == 0.5f && slots[5].channel == 8 &&
          slots[5].forward_channel == 2 && strcmp(slots[5].patch_name, "F") == 0,
          "an aux slot's saved state must load");
    CHECK(slots[6].muted == 1 && slots[7].channel == -1 && slots[7].forward_channel == -2,
          "aux slots 6 and 7 did not load");
    CHECK(solo_count_pages == 1, "the solo count says %d with one aux slot soloed", solo_count_pages);

    /* And the saver writes all eight. */
    shadow_save_config_to_dir(dir);
    const char *text = read_file(path);
    int names = 0;
    for (const char *p = text; p && (p = strstr(p, "\"name\"")); p++) names++;
    CHECK(names == SHADOW_CHAIN_INSTANCES, "the set config holds %d slots", names);
}

int main(void) {
    char dir[] = "/tmp/schwung_test_aux_slots_XXXXXX";
    if (!mkdtemp(dir)) { perror("mkdtemp"); return 2; }
    snprintf(aux_state_path, sizeof aux_state_path, "%s/shadow_chain_config.json", dir);

    test_ui_state_layout();
    test_state_file();
    test_set_config(dir);

    char cmd[600];
    snprintf(cmd, sizeof cmd, "rm -rf '%s'", dir);
    if (system(cmd) != 0) { /* a stray temp dir is not a test failure */ }

    if (fails) {
        fprintf(stderr, "test_aux_slots: %d failure(s)\n", fails);
        return 1;
    }
    printf("PASS test_aux_slots\n");
    return 0;
}

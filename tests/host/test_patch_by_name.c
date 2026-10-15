/*
 * The patch library must be written BY NAME.
 *
 * `update_patch` / `delete_patch` took an INDEX into the chain instance's own
 * patch list, and an instance rescans that list only when IT saves. Each slot
 * is its own instance over ONE shared folder, so once another slot had added a
 * preset, an index computed from a fresh listing named a different file in the
 * stale list -- measured on a device: two Remote UI renames rewrote ns.json and
 * linein_freeverb.json instead of the presets asked for.
 *
 * This drives the REAL chain_patch.c (included; its load-path dependencies are
 * stubbed, none of them reached here) against a temporary folder with two
 * instances, reproducing the stale list, and pins what by-name does instead:
 * rescan, then the ONE preset with exactly that name, or nothing.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <sys/stat.h>

#include "chain_patch.c"

/* ---- stubs: the load path, logging -- never reached by scan/save/update ---- */
void chain_log(const char *m) { (void)m; }
void v2_chain_log(chain_instance_t *i, const char *m) { (void)i; (void)m; }
void parse_debug_log(const char *m) { (void)m; }
int chain_bus_apply_patch(chain_instance_t *i, const patch_info_t *p) { (void)i; (void)p; return 0; }
void chain_mod_clear_source(void *i, const char *s) { (void)i; (void)s; }
float dsp_value_to_float(const char *v, chain_param_info_t *p, float d) { (void)v; (void)p; return d; }
chain_param_info_t *find_param_by_key(chain_instance_t *i, const char *t, const char *k) { (void)i; (void)t; (void)k; return NULL; }
void knob_emit_cc_out_all(chain_instance_t *i) { (void)i; }
int v2_load_audio_fx(chain_instance_t *i, const char *n) { (void)i; (void)n; return 0; }
int v2_load_midi_fx(chain_instance_t *i, const char *n) { (void)i; (void)n; return 0; }
int v2_load_synth(chain_instance_t *i, const char *n) { (void)i; (void)n; return 0; }
void v2_synth_panic(chain_instance_t *i) { (void)i; }
void v2_unload_all_audio_fx(chain_instance_t *i) { (void)i; }
void v2_unload_all_midi_fx(chain_instance_t *i) { (void)i; }
void v2_unload_synth(chain_instance_t *i) { (void)i; }

static int failures = 0, checks = 0;
#define CHECK(c, ...) do { checks++; if (!(c)) { failures++; printf("FAIL: "); printf(__VA_ARGS__); \
    printf("\n      (%s:%d: %s)\n", __FILE__, __LINE__, #c); } } while (0)

static char dir[256], patches[300];

static void put(const char *file, const char *name)
{
    char p[512];
    snprintf(p, sizeof p, "%s/%s", patches, file);
    FILE *f = fopen(p, "w");
    fprintf(f, "{\n  \"name\": \"%s\",\n  \"version\": 1,\n  \"chain\": {\"synth\": {\"module\": \"x\"}}\n}\n", name);
    fclose(f);
}

static void name_in(const char *file, char *out, size_t n)
{
    char p[512];
    snprintf(p, sizeof p, "%s/%s", patches, file);
    out[0] = '\0';
    FILE *f = fopen(p, "r");
    if (!f) return;
    char buf[4096];
    size_t r = fread(buf, 1, sizeof buf - 1, f);
    buf[r] = '\0';
    fclose(f);
    json_get_string(buf, "name", out, (int)n);
}

static chain_instance_t *instance(void)
{
    chain_instance_t *inst = calloc(1, sizeof *inst);
    snprintf(inst->module_dir, sizeof inst->module_dir, "%s/modules/chain", dir);
    v2_scan_patches(inst);
    return inst;
}

int main(void)
{
    snprintf(dir, sizeof dir, "%s/patch_by_name.XXXXXX", getenv("TMPDIR") ? getenv("TMPDIR") : "/tmp");
    if (!mkdtemp(dir)) { perror("mkdtemp"); return 1; }
    char mod[300];
    snprintf(mod, sizeof mod, "%s/modules", dir); mkdir(mod, 0755);
    snprintf(mod, sizeof mod, "%s/modules/chain", dir); mkdir(mod, 0755);
    snprintf(patches, sizeof patches, "%s/patches", dir); mkdir(patches, 0755);

    put("ch.json", "CH");
    put("msp__td.json", "MSP + TD");
    put("ns.json", "NS");
    put("sf.json", "SF");

    /* Two slots, both scanned now. */
    chain_instance_t *a = instance(), *b = instance();
    CHECK(a->patch_count == 4 && b->patch_count == 4, "both see 4 presets (%d, %d)", a->patch_count, b->patch_count);

    /* Slot A adds a preset that sorts FIRST. B does not rescan. */
    v2_save_patch(a, "{\"custom_name\": \"Aardvark\", \"synth\": {\"module\": \"x\"}}");
    v2_scan_patches(a);
    CHECK(a->patch_count == 5 && b->patch_count == 4, "only A's list moved (%d, %d)", a->patch_count, b->patch_count);

    /* The bug, reproduced: a fresh listing puts "MSP + TD" at index 2
     * (Aardvark, CH, MSP + TD, ...); in B's stale list index 2 is NS. */
    CHECK(strcmp(a->patches[2].name, "MSP + TD") == 0, "fresh index 2 is MSP + TD (%s)", a->patches[2].name);
    CHECK(strcmp(b->patches[2].name, "NS") == 0,
          "B's STALE index 2 is NS (%s) -- an index-addressed update overwrites the wrong preset",
          b->patches[2].name);

    /* By name: rescan, then the one preset with that exact name. */
    v2_scan_patches(b);
    int i = v2_find_unique_patch(b, "MSP + TD");
    CHECK(i >= 0 && strcmp(b->patches[i].path + strlen(b->patches[i].path) - 12, "msp__td.json") == 0,
          "by name finds msp__td.json (%s)", i >= 0 ? b->patches[i].path : "none");
    CHECK(v2_update_patch(b, i, "{\"custom_name\": \"Pads\", \"synth\": {\"module\": \"x\"}}") == 0, "update succeeds");
    char got[64];
    name_in("msp__td.json", got, sizeof got);
    CHECK(strcmp(got, "Pads") == 0, "msp__td.json is now Pads (%s)", got);
    name_in("ns.json", got, sizeof got);
    CHECK(strcmp(got, "NS") == 0, "ns.json is untouched (%s)", got);

    /* Nothing, or more than one, is NEVER picked. */
    v2_scan_patches(b);
    CHECK(v2_find_unique_patch(b, "MSP + TD") == -1, "a name no longer present finds nothing");
    CHECK(v2_find_unique_patch(b, "ns") == -1, "names are exact (no case folding)");
    put("ns_copy.json", "NS");
    v2_scan_patches(b);
    CHECK(v2_find_unique_patch(b, "NS") == -1, "an AMBIGUOUS name picks nothing");

    char cmd[600];
    snprintf(cmd, sizeof cmd, "rm -rf '%s'", dir);
    if (system(cmd) != 0) {}
    free(a); free(b);
    printf("test_patch_by_name: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
}

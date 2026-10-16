/*
 * "knob_map" -- Knobs 1-8's mappings and positions in one read
 * (src/modules/chain/dsp/knob_map_json.h). The Remote UI's Slot tabs poll it to
 * follow hardware knob turns, which reach the module directly and are on no
 * notify ring.
 */
#include <stdio.h>
#include <string.h>
#include "knob_map_json.h"

static int failures = 0, checks = 0;
#define CHECK(c, ...) do { checks++; if (!(c)) { failures++; printf("FAIL: "); printf(__VA_ARGS__); printf("\n"); } } while (0)

int main(void)
{
    char buf[512];
    int knob[] = { 1, 3, 8 };
    const char *target[] = { "synth", "fx2", "midi_fx1" };
    const char *param[] = { "cutoff", "wet", "rate" };
    float value[] = { 0.42f, 7.0f, -999999.0f };

    int n = knob_map_format(buf, sizeof buf, 3, knob, target, param, value);
    CHECK(n == (int)strlen(buf), "returns its length");
    CHECK(strcmp(buf, "[{\"knob\":1,\"target\":\"synth\",\"param\":\"cutoff\",\"value\":0.42},"
                      "{\"knob\":3,\"target\":\"fx2\",\"param\":\"wet\",\"value\":7},"
                      "{\"knob\":8,\"target\":\"midi_fx1\",\"param\":\"rate\",\"value\":null}]") == 0,
          "shape: %s", buf);

    n = knob_map_format(buf, sizeof buf, 0, NULL, NULL, NULL, NULL);
    CHECK(n == 2 && strcmp(buf, "[]") == 0, "no mappings: %s", buf);

    /* A name a JSON string cannot hold verbatim is left out, not escaped --
     * and the list stays valid around it. */
    const char *bad[] = { "synth", "fx\"1", "fx2" };
    const char *bp[] = { "a", "b", "c" };
    float bv[] = { 1, 2, 3 };
    int bk[] = { 1, 2, 3 };
    knob_map_format(buf, sizeof buf, 3, bk, bad, bp, bv);
    CHECK(strcmp(buf, "[{\"knob\":1,\"target\":\"synth\",\"param\":\"a\",\"value\":1},"
                      "{\"knob\":3,\"target\":\"fx2\",\"param\":\"c\",\"value\":3}]") == 0,
          "unsafe name skipped, commas right: %s", buf);

    char tiny[30];
    n = knob_map_format(tiny, sizeof tiny, 3, knob, target, param, value);
    CHECK(n == -1 && tiny[0] == '\0', "too small is refused, never truncated (n=%d)", n);

    printf("test_knob_map_json: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
}

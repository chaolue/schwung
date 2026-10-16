/*
 * "chain_modules" -- every position's module in one read
 * (src/modules/chain/dsp/chain_modules_json.h). The Remote UI polls this in
 * place of up to 17 per-position reads, so it must be exact: each list exactly
 * its section's length, in position order, escaped, and REFUSED rather than
 * truncated -- a short list parses fine and reads as "those positions are
 * empty".
 */
#include <stdio.h>
#include <string.h>
#include "chain_modules_json.h"

static int failures = 0, checks = 0;
#define CHECK(c, ...) do { checks++; if (!(c)) { failures++; printf("FAIL: "); printf(__VA_ARGS__); printf("\n"); } } while (0)

int main(void)
{
    char buf[512];
    const char *midi[] = { "arp" };
    const char *fx[] = { "freeverb", "", "tape-echo2" };

    int n = chain_modules_format(buf, sizeof buf, "obxd", midi, 1, fx, 3);
    CHECK(n == (int)strlen(buf), "returns its length (%d vs %zu)", n, strlen(buf));
    CHECK(strcmp(buf, "{\"synth\":\"obxd\",\"midi_fx\":[\"arp\"],\"fx\":[\"freeverb\",\"\",\"tape-echo2\"]}") == 0,
          "shape: %s", buf);

    n = chain_modules_format(buf, sizeof buf, "", NULL, 0, NULL, 0);
    CHECK(strcmp(buf, "{\"synth\":\"\",\"midi_fx\":[],\"fx\":[]}") == 0, "an empty chain: %s", buf);

    const char *odd[] = { "a\"b\\c\nd" };
    chain_modules_format(buf, sizeof buf, "x", NULL, 0, odd, 1);
    CHECK(strstr(buf, "\"a\\\"b\\\\cd\"") != NULL, "quotes and backslashes escaped, control bytes dropped: %s", buf);

    /* Too small: refused, and the buffer says nothing. */
    char tiny[24];
    n = chain_modules_format(tiny, sizeof tiny, "obxd", midi, 1, fx, 3);
    CHECK(n == -1 && tiny[0] == '\0', "a buffer too small is refused, never truncated (n=%d '%s')", n, tiny);

    /* Exactly enough fits; one byte less does not. */
    int need = chain_modules_format(buf, sizeof buf, "obxd", midi, 1, fx, 3);
    char exact[128];
    CHECK(chain_modules_format(exact, need + 1, "obxd", midi, 1, fx, 3) == need, "fits in length+1");
    CHECK(chain_modules_format(exact, need, "obxd", midi, 1, fx, 3) == -1, "refused at length");

    printf("test_chain_modules_json: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
}

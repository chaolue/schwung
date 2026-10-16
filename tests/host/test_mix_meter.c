/*
 * The Remote UI Mixer's peak meters (src/host/mix_meter.h): the block peak,
 * the 20 dB/s fall, and the floor that lets a silent strip settle at 0.
 */
#include <math.h>
#include <stdio.h>
#include "mix_meter.h"

static int failures = 0, checks = 0;
#define CHECK(c, ...) do { checks++; if (!(c)) { failures++; printf("FAIL: "); printf(__VA_ARGS__); printf("\n"); } } while (0)

int main(void)
{
    /* The constant is the stated fall at 128-frame blocks of 44.1 kHz. */
    const double want = pow(10.0, -MIX_METER_FALL_DB_PER_S * (128.0 / 44100.0) / 20.0);
    CHECK(fabs(want - MIX_METER_DECAY) < 1e-6, "MIX_METER_DECAY %f, want %f", (double)MIX_METER_DECAY, want);

    int16_t blk[256];
    for (int i = 0; i < 128; i++) { blk[2 * i] = 0; blk[2 * i + 1] = 0; }
    blk[40] = -16384;   /* left, half scale, negative */
    blk[91] = 8192;     /* right, quarter scale */
    float pk[2] = { 0, 0 };
    mix_meter_peak(blk, 128, 1.0f, 1.0f, pk);
    CHECK(fabsf(pk[0] - 0.5f) < 1e-6f && fabsf(pk[1] - 0.25f) < 1e-6f,
          "peaks per channel, sign ignored (got %f %f)", (double)pk[0], (double)pk[1]);

    /* Gains scale each side (a fader at half, panned hard right). */
    float g[2] = { 0, 0 };
    mix_meter_peak(blk, 128, 0.0f, 0.5f, g);
    CHECK(g[0] == 0.0f && fabsf(g[1] - 0.125f) < 1e-6f, "gains apply per side (got %f %f)",
          (double)g[0], (double)g[1]);

    /* Accumulates: a second, quieter tap in the same block keeps the louder. */
    mix_meter_peak(blk, 128, 0.1f, 0.1f, pk);
    CHECK(fabsf(pk[0] - 0.5f) < 1e-6f, "a second tap never lowers the block peak");
    int16_t full[256];
    for (int i = 0; i < 256; i++) full[i] = (int16_t)-32768;
    float f[2] = { 0, 0 };
    mix_meter_peak(full, 128, 1.0f, 1.0f, f);
    CHECK(f[0] == 1.0f, "full scale reads 1.0, not past it (got %f)", (double)f[0]);
    mix_meter_peak(NULL, 128, 1.0f, 1.0f, f);   /* no buffer: nothing */

    /* The fall: a peak is held, and has dropped 20 dB after one second. */
    float h = mix_meter_fall(0.0f, 1.0f);
    CHECK(h == 1.0f, "a new peak is taken at once");
    const int per_s = (int)lround(44100.0 / 128.0);
    for (int i = 0; i < per_s; i++) h = mix_meter_fall(h, 0.0f);
    CHECK(fabs(20.0 * log10(h) + 20.0) < 0.2, "-20 dB after a second (got %.2f dB)", 20.0 * log10(h));
    /* A sample taken 50 ms after a peak still sees it, within 1 dB. */
    float s = mix_meter_fall(0.0f, 0.8f);
    for (int i = 0; i < 17; i++) s = mix_meter_fall(s, 0.0f);
    CHECK(20.0 * log10(0.8 / s) < 1.0, "a 20 Hz reader cannot miss a peak (%.2f dB lost)", 20.0 * log10(0.8 / s));
    /* A quieter block does not pull it down faster than the fall. */
    float q = mix_meter_fall(1.0f, 0.1f);
    CHECK(fabsf(q - MIX_METER_DECAY) < 1e-6f, "a quieter block only falls (got %f)", (double)q);
    /* And silence settles to an exact zero. */
    for (int i = 0; i < 20 * per_s; i++) h = mix_meter_fall(h, 0.0f);
    CHECK(h == 0.0f, "silence settles at exactly 0 (got %g)", (double)h);

    printf("test_mix_meter: %d checks, %d failures\n", checks, failures);
    return failures ? 1 : 0;
}

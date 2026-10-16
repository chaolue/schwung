/*
 * mix_meter.h — the Remote UI Mixer's peak meters, as plain arithmetic.
 *
 * Header-only and free of shim state so tests/host can run it
 * (tests/host/test_mix_meter.c). The shim taps a block at each strip's mix
 * point, keeps the block's PEAK, and publishes a HELD value that falls at
 * MIX_METER_FALL_DB_PER_S. The manager samples that at 20 Hz; because the
 * held value only falls slowly, a peak between two samples is still there at
 * the next one -- the reader never needs to reset anything, so there is no
 * handshake with the audio thread at all.
 *
 * RT-safe: no allocation, no locks, no I/O; a loop of compares per block.
 * Values are linear, 1.0 = full scale (int16 32768).
 */
#ifndef MIX_METER_H
#define MIX_METER_H

#include <stdint.h>

#define MIX_METER_FALL_DB_PER_S 20.0f
/* Per block: 10^(-20 dB/s * 128 / 44100 s / 20) -- 20 dB/s at the shim's
 * 128-frame blocks. tests/host/test_mix_meter.c recomputes it. */
#define MIX_METER_DECAY 0.993339f
/* Below this the held value is simply zero (about -100 dBFS), so a silent
 * strip settles to an exact 0 rather than denormals. */
#define MIX_METER_FLOOR 1e-5f

/* Raise pk[0] / pk[1] to this interleaved stereo block's peak, each channel
 * scaled by its gain (the strip's fader and pan as the mix applies them).
 * Accumulates: a strip tapped twice in one block keeps the larger. */
static inline void mix_meter_peak(const int16_t *buf, int frames, float gl, float gr, float pk[2])
{
    if (!buf) return;
    int ml = 0, mr = 0;
    for (int f = 0; f < frames; f++) {
        int l = buf[2 * f], r = buf[2 * f + 1];
        if (l < 0) l = -l;
        if (r < 0) r = -r;
        if (l > ml) ml = l;
        if (r > mr) mr = r;
    }
    if (gl < 0.0f) gl = -gl;
    if (gr < 0.0f) gr = -gr;
    const float l = (float)ml * gl * (1.0f / 32768.0f);
    const float r = (float)mr * gr * (1.0f / 32768.0f);
    if (l > pk[0]) pk[0] = l;
    if (r > pk[1]) pk[1] = r;
}

/* One block of fall, then the block's own peak if that is higher. */
static inline float mix_meter_fall(float held, float block_peak)
{
    held *= MIX_METER_DECAY;
    if (block_peak > held) held = block_peak;
    if (held < MIX_METER_FLOOR) held = 0.0f;
    return held;
}

#endif /* MIX_METER_H */

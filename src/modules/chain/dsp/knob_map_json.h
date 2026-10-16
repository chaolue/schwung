/*
 * knob_map_json.h -- the "knob_map" answer: a slot's knob mappings (Knobs 1-8)
 * and where each one sits, in ONE read,
 *
 *   [{"knob":1,"target":"synth","param":"cutoff","value":0.42}, ...]
 *
 * Mapped knobs only, in knob order. "value" is the mapping's own accumulator --
 * what the next turn of the hardware knob moves from -- or null before it has
 * one. The Remote UI's Slot tabs poll this while a tab is open: a hardware knob
 * turn reaches the module directly and is on no notify ring, so this is the
 * only way the browser can follow it.
 *
 * Header-only and dependency-free so tests/host can run it.
 */
#ifndef KNOB_MAP_JSON_H
#define KNOB_MAP_JSON_H

#include <stdio.h>
#include <string.h>

/* The answer's length, or -1 -- refused rather than truncated. */
static inline int knob_map_format(char *buf, int cap, int count, const int *knob,
                                  const char *const *target, const char *const *param,
                                  const float *value)
{
    if (!buf || cap <= 0) return -1;
    int len = 0;
#define KM_PUT(...) do { int n_ = snprintf(buf + len, (size_t)(cap - len), __VA_ARGS__); \
                         if (n_ < 0 || n_ >= cap - len) { buf[0] = '\0'; return -1; } len += n_; } while (0)
    KM_PUT("[");
    for (int i = 0; i < count; i++) {
        const char *t = target[i] ? target[i] : "", *p = param[i] ? param[i] : "";
        /* Mapping names come from module metadata: anything a JSON string
         * cannot hold verbatim is refused rather than escaped -- a key with a
         * quote in it is not a key the Remote UI could address anyway. */
        if (strpbrk(t, "\"\\") || strpbrk(p, "\"\\")) continue;
        KM_PUT("%s{\"knob\":%d,\"target\":\"%s\",\"param\":\"%s\",\"value\":", len > 1 ? "," : "",
               knob[i], t, p);
        if (value[i] > -999998.0f && value[i] == value[i]) KM_PUT("%g}", (double)value[i]);
        else KM_PUT("null}");
    }
    KM_PUT("]");
#undef KM_PUT
    return len;
}

#endif /* KNOB_MAP_JSON_H */

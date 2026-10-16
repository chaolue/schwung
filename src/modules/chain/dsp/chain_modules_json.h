/*
 * chain_modules_json.h -- the "chain_modules" answer: every position's module
 * in ONE read,
 *
 *   {"synth":"obxd","midi_fx":["arp"],"fx":["freeverb","tape-echo2"]}
 *
 * Each list is exactly that section's length, in position order. The Remote UI
 * polls this instead of up to 17 per-position reads (synth, fx1..8,
 * midi_fx1..8), each a ~2.8 ms round trip on the channel shadow_ui draws with.
 *
 * Header-only and dependency-free so tests/host can run it.
 */
#ifndef CHAIN_MODULES_JSON_H
#define CHAIN_MODULES_JSON_H

/* Append str. quote=1: a JSON string VALUE -- quoted, escaped, control bytes
 * dropped. quote=0: the document's own structure, copied verbatim (escaping
 * it would turn `{"synth":` into `{\"synth\":`). Returns 0 when it does not
 * fit. */
static inline int chain_modules_put(char *buf, int cap, int *len, const char *str, int quote)
{
    if (quote) { if (*len + 1 >= cap) return 0; buf[(*len)++] = '"'; }
    for (const char *p = str ? str : ""; *p; p++) {
        unsigned char c = (unsigned char)*p;
        if (quote && c < 0x20) continue;
        if (quote && (c == '"' || c == '\\')) { if (*len + 2 >= cap) return 0; buf[(*len)++] = '\\'; }
        if (*len + 1 >= cap) return 0;
        buf[(*len)++] = (char)c;
    }
    if (quote) { if (*len + 1 >= cap) return 0; buf[(*len)++] = '"'; }
    buf[*len] = '\0';
    return 1;
}

/* The answer's length, or -1 -- REFUSED rather than truncated: a short list
 * parses fine and reads as "those positions are empty". */
static inline int chain_modules_format(char *buf, int cap, const char *synth,
                                       const char *const *midi, int nm,
                                       const char *const *fx, int nf)
{
    int len = 0;
    if (!buf || cap <= 0) return -1;
    int ok = chain_modules_put(buf, cap, &len, "{\"synth\":", 0) &&
             chain_modules_put(buf, cap, &len, synth, 1) &&
             chain_modules_put(buf, cap, &len, ",\"midi_fx\":[", 0);
    for (int i = 0; ok && i < nm; i++)
        ok = (i == 0 || chain_modules_put(buf, cap, &len, ",", 0)) &&
             chain_modules_put(buf, cap, &len, midi[i], 1);
    ok = ok && chain_modules_put(buf, cap, &len, "],\"fx\":[", 0);
    for (int i = 0; ok && i < nf; i++)
        ok = (i == 0 || chain_modules_put(buf, cap, &len, ",", 0)) &&
             chain_modules_put(buf, cap, &len, fx[i], 1);
    ok = ok && chain_modules_put(buf, cap, &len, "]}", 0);
    if (!ok) { buf[0] = '\0'; return -1; }
    return len;
}

#endif /* CHAIN_MODULES_JSON_H */

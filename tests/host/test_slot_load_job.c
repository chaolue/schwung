/*
 * slot_load_job.h — which slot writes are handed to the loader thread.
 *
 * The list is the whole safety margin of the feature in one direction: a key
 * that is NOT a load but matches here parks the slot, which cuts its audio for
 * a frame on every write — and knob writes arrive on every detent. A load
 * that does NOT match here runs on the SPI callback and the hiccup is back.
 * Both directions are pinned.
 */
#include <stdio.h>
#include "slot_load_job.h"

static int failures;
#define CHECK(cond, msg) do { \
    if (!(cond)) { printf("FAIL: %s\n", msg); failures++; } \
} while (0)

int main(void) {
    /* ---- every write that loads or unloads a module ------------------- */
    CHECK(slot_load_key_is_async("synth:module"), "synth:module is a load");
    CHECK(slot_load_key_is_async("load_file"), "load_file is a load");
    CHECK(slot_load_key_is_async("load_patch"), "load_patch is a load");
    CHECK(slot_load_key_is_async("patch"), "patch is a load");
    CHECK(slot_load_key_is_async("clear"), "clear unloads every module");
    CHECK(slot_load_key_is_async("fx1:module"), "fx1:module is a load");
    CHECK(slot_load_key_is_async("fx8:module"), "fx8:module is a load");
    CHECK(slot_load_key_is_async("fx12:module"), "a two-digit position is a load");
    CHECK(slot_load_key_is_async("midi_fx1:module"), "midi_fx1:module is a load");
    CHECK(slot_load_key_is_async("midi_fx8:module"), "midi_fx8:module is a load");

    /* ---- writes that must never park a slot --------------------------- */
    CHECK(!slot_load_key_is_async(NULL), "NULL is not a load");
    CHECK(!slot_load_key_is_async(""), "empty is not a load");
    CHECK(!slot_load_key_is_async("synth:cutoff"), "a knob write is not a load");
    CHECK(!slot_load_key_is_async("synth:state"), "state is not a module load");
    CHECK(!slot_load_key_is_async("fx1:mix"), "an FX knob is not a load");
    CHECK(!slot_load_key_is_async("fx1:module_x"), "prefix of :module is not a load");
    CHECK(!slot_load_key_is_async("synth:modules"), "synth:modules is not a load");
    CHECK(!slot_load_key_is_async("fx:module"), "no position digit is not a load");
    CHECK(!slot_load_key_is_async("fx0:module"), "position 0 does not exist");
    CHECK(!slot_load_key_is_async("fx01:module"), "a leading zero is not a position");
    CHECK(!slot_load_key_is_async("fx:insert"), "a shape verb permutes, it does not load");
    CHECK(!slot_load_key_is_async("bus1:fx1:module"),
          "a bus insert loads on chain_bus.c's own worker already");
    CHECK(!slot_load_key_is_async("mod:tick"), "the per-frame mod tick is not a load");
    CHECK(!slot_load_key_is_async("load_file_x"), "load_file is matched whole");
    CHECK(!slot_load_key_is_async("clear_clip"), "clear is matched whole");

    /* The patch LIBRARY writes: off the callback (a rescan parses every
     * preset, ~14 ms measured), but never parked -- they load nothing. */
    CHECK(slot_load_key_is_patch_io("save_patch"), "save_patch writes the library");
    CHECK(slot_load_key_is_patch_io("update_patch"), "update_patch writes the library");
    CHECK(slot_load_key_is_patch_io("delete_patch"), "delete_patch writes the library");
    CHECK(slot_load_key_is_patch_io("update_patch_by_name"), "update_patch_by_name writes the library");
    CHECK(slot_load_key_is_patch_io("delete_patch_by_name"), "delete_patch_by_name writes the library");
    CHECK(!slot_load_key_is_async("save_patch"), "a library write is not a LOAD (it must not park the slot)");
    CHECK(!slot_load_key_is_patch_io("load_patch"), "load_patch is a load, not a library write");
    CHECK(!slot_load_key_is_patch_io("save_patch_x"), "save_patch is matched whole");
    CHECK(!slot_load_key_is_patch_io("synth:save_patch"), "a component key is never a library write");

    if (failures) { printf("%d failure(s)\n", failures); return 1; }
    printf("test_slot_load_job: all passed\n");
    return 0;
}

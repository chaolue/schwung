/* ================================================================
   Remote UI - Mixer tab

   The four slots' levels, mutes, solos, pans and sends, and the two send
   returns, on one page. Every control writes the SAME key the device's Slot
   Settings and the E16/EC4 Mixer write, so this page, a hardware surface and
   Move's own track knob all move one value.

   The server (remote_ui_mixer.go) sends:
     mixer_state   the whole mixer. A key that is ABSENT is UNKNOWN: its
                   control is disabled and reads "—". A read that did not
                   answer must never become a picture.
     mixer_update  only what changed.
   and takes set_mixer_param { slot, key, value } for an allowlisted key.

   While a hand is on a control, the device's echo of it is ignored; when the
   hand leaves, the control snaps to whatever the device last said. The
   device is the truth -- a write can be refused (an armed scene takes it as a
   lock) and the page must show that rather than what was asked for.
   ================================================================ */

(function (root) {
    "use strict";

    var SLOTS = 8;            // SHADOW_CHAIN_INSTANCES: Move's four, then the aux four
    var MOVE_SLOTS = 4;       // SHADOW_MOVE_SLOTS: the ones paired with a Move track
    var VOLUME_MAX = 4;       // slot:volume clamp in the shim: +12 dB
    var SEND_MAX = 127;       // BUS_MIX_SEND_LEVEL_MAX; a send is level/127, linear
    var FADER_STEPS = 1000;
    var DB_FLOOR = -48;       // the bottom of the dB-linear part of the fader
    var KNEE = 0.1;           // below this position the fader fades linearly to silence
    var HOLD_MS = 400;        // ignore the device's echo this long after the hand leaves
    var SEND_INTERVAL_MS = 33; // at most ~30 writes a second per control

    // ------------------------------------------------------------------
    // Pure helpers (exported for tests/host/test_remote_ui_mixer.sh)
    // ------------------------------------------------------------------

    function gainToDb(g) { return 20 * Math.log(g) / Math.LN10; }
    function dbToGain(db) { return Math.pow(10, db / 20); }

    // A fader taper over [0, maxGain]: dB-linear from DB_FLOOR to the top,
    // with the bottom KNEE of travel fading linearly to silence so 0 is
    // reachable. Unity sits where a mixer puts it, about four fifths up.
    function makeTaper(maxGain) {
        var ceil = gainToDb(maxGain);
        var floorGain = dbToGain(DB_FLOOR);
        return {
            toGain: function (p) {
                if (!(p > 0)) return 0;
                if (p >= 1) return maxGain;
                if (p < KNEE) return floorGain * (p / KNEE);
                return dbToGain(DB_FLOOR + (p - KNEE) / (1 - KNEE) * (ceil - DB_FLOOR));
            },
            toPos: function (g) {
                if (!(g > 0)) return 0;
                if (g >= maxGain) return 1;
                if (g < floorGain) return KNEE * g / floorGain;
                return KNEE + (gainToDb(g) - DB_FLOOR) / (ceil - DB_FLOOR) * (1 - KNEE);
            }
        };
    }

    var volumeTaper = makeTaper(VOLUME_MAX); // slot faders, to +12 dB
    var returnTaper = makeTaper(1);          // return faders, to 0 dB (127)

    // "−6.0 dB", "+0.0 dB" is written "0.0 dB", silence is "−∞ dB".
    function formatDb(g) {
        if (!(g > 0)) return "−∞ dB";
        var r = Math.round(gainToDb(g) * 10) / 10;
        if (r === 0) return "0.0 dB";
        return (r > 0 ? "+" : "−") + Math.abs(r).toFixed(1) + " dB";
    }

    // The device's own pan text (Slot Settings): "C", "L 50", "R 50".
    function formatPan(p) {
        if (!isFinite(p) || Math.abs(p) < 0.01) return "C";
        return (p < 0 ? "L " : "R ") + Math.round(Math.abs(p) * 100);
    }

    // ------------------------------------------------------------------
    // Control kinds: how a key's wire value maps to an input and back
    // ------------------------------------------------------------------

    var KINDS = {
        volume: {  // slot:volume, linear 0..4, on a tapered vertical fader
            min: 0, max: FADER_STEPS, step: 1, vertical: true, unity: Math.round(volumeTaper.toPos(1) * FADER_STEPS),
            toInput: function (v) { return Math.round(volumeTaper.toPos(parseFloat(v)) * FADER_STEPS); },
            toWire: function (n) { return volumeTaper.toGain(n / FADER_STEPS).toFixed(4); },
            text: function (v) { return formatDb(parseFloat(v)); }
        },
        ret: {     // send<N>:return, 0..127, on a tapered vertical fader
            min: 0, max: FADER_STEPS, step: 1, vertical: true, unity: FADER_STEPS,
            toInput: function (v) { return Math.round(returnTaper.toPos(parseInt(v, 10) / SEND_MAX) * FADER_STEPS); },
            toWire: function (n) { return String(Math.round(returnTaper.toGain(n / FADER_STEPS) * SEND_MAX)); },
            text: function (v) { return formatDb(parseInt(v, 10) / SEND_MAX); }
        },
        send: {    // buses:main_send<N>, send1:to_send2: 0..127, linear, horizontal
            min: 0, max: SEND_MAX, step: 1, vertical: false, unity: 0,
            toInput: function (v) { return parseInt(v, 10); },
            toWire: function (n) { return String(n); },
            text: function (v) { return formatDb(parseInt(v, 10) / SEND_MAX); }
        },
        pan: {     // slot:pan, -1..1, horizontal, double-click centres
            min: -100, max: 100, step: 1, vertical: false, unity: 0,
            toInput: function (v) { return Math.round(parseFloat(v) * 100); },
            toWire: function (n) { return (n / 100).toFixed(2); },
            text: function (v) { return formatPan(parseFloat(v)); }
        }
    };

    // ------------------------------------------------------------------
    // The tab
    // ------------------------------------------------------------------

    function create(opts) {
        var sendMsg = opts.send;

        // Last thing the device said, keyed "<slot>|<key>" or "g|<key>".
        var model = {};
        var live = false;
        var synced = false;      // a mixer_state has arrived since subscribing
        var rootEl = null;
        var controls = {};       // id -> control (sliders)
        var toggles = {};        // id -> { button, label }
        var names = [];          // per-slot name element
        var noticeEl = null;

        function id(slot, key) { return (slot === null ? "g" : String(slot)) + "|" + key; }

        function attached() { return rootEl && document.body.contains(rootEl); }

        // --- inbound -------------------------------------------------------

        function merge(msg) {
            var changed = [];
            var s, k;
            if (msg.slots) {
                for (s in msg.slots) {
                    for (k in msg.slots[s]) {
                        model[s + "|" + k] = msg.slots[s][k];
                        changed.push(s + "|" + k);
                    }
                }
            }
            if (msg.global) {
                for (k in msg.global) {
                    model["g|" + k] = msg.global[k];
                    changed.push("g|" + k);
                }
            }
            return changed;
        }

        function handle(msg) {
            if (msg.type === "mixer_state") {
                model = {};          // absent means unknown
                synced = true;
                if (typeof msg.live === "boolean") live = msg.live;
                merge(msg);
                refreshAll();
                return;
            }
            if (msg.type !== "mixer_update") return;
            if (typeof msg.live === "boolean") live = msg.live;
            var changed = merge(msg);
            if (typeof msg.live === "boolean") { refreshAll(); return; }
            for (var i = 0; i < changed.length; i++) refreshOne(changed[i]);
        }

        // Called when the socket drops or the tab is left: nothing shown is
        // known any more until the next mixer_state.
        function reset() {
            model = {};
            synced = false;
            live = false;
            refreshAll();
        }

        // --- what the model says about one id -------------------------------

        // volume / mute / solo / name come from the slot-state segment: when
        // that is not live, they are unknown whatever the model last held.
        function fromSegment(key) {
            return key === "slot:volume" || key === "slot:muted" || key === "slot:soloed" || key === "name";
        }

        function known(cid) {
            if (!synced || !(cid in model)) return false;
            var key = cid.slice(cid.indexOf("|") + 1);
            return !(fromSegment(key) && !live);
        }

        // --- outbound ------------------------------------------------------

        function write(ctl, wire) {
            var msg = { type: "set_mixer_param", key: ctl.key, value: wire };
            if (ctl.slot !== null) msg.slot = ctl.slot;
            sendMsg(msg);
        }

        // Throttled per control: the first value goes at once, then at most one
        // per SEND_INTERVAL_MS, and the LAST value always goes.
        function queueWrite(ctl, wire) {
            ctl.pending = wire;
            if (ctl.timer) return;
            var flush = function () {
                if (ctl.pending === null) { ctl.timer = null; return; }
                write(ctl, ctl.pending);
                ctl.pending = null;
                ctl.timer = setTimeout(flush, SEND_INTERVAL_MS);
            };
            flush();
        }

        // --- DOM -----------------------------------------------------------

        function held(ctl) {
            return ctl.touching || Date.now() < ctl.holdUntil;
        }

        function paintSlider(ctl) {
            var cid = ctl.id;
            var ok = known(cid);
            ctl.input.disabled = !ok;
            if (!ok) {
                ctl.out.textContent = "—";
                ctl.input.setAttribute("aria-valuetext", "unknown");
                return;
            }
            var v = model[cid];
            ctl.input.value = String(ctl.kind.toInput(v));
            var text = ctl.kind.text(v);
            ctl.out.textContent = text;
            ctl.input.setAttribute("aria-valuetext", text);
        }

        function paintToggle(t) {
            var ok = known(t.id);
            t.button.disabled = !ok;
            var on = ok && model[t.id] === "1";
            t.button.setAttribute("aria-pressed", on ? "true" : "false");
            t.button.classList.toggle("active", on);
        }

        function paintStrip(slot) {
            var nameEl = names[slot];
            if (nameEl) {
                var nid = id(slot, "name");
                nameEl.textContent = known(nid) ? (model[nid] || "Empty") : "—";
            }
            var strip = rootEl && rootEl.querySelector('[data-mixer-slot="' + slot + '"]');
            if (strip) strip.classList.toggle("muted", known(id(slot, "slot:muted")) && model[id(slot, "slot:muted")] === "1");
        }

        function refreshOne(cid) {
            if (!attached()) return;
            var ctl = controls[cid];
            if (ctl && !held(ctl)) paintSlider(ctl);
            if (toggles[cid]) paintToggle(toggles[cid]);
            var bar = cid.indexOf("|");
            var slot = parseInt(cid.slice(0, bar), 10);
            if (slot >= 0 && slot < SLOTS) paintStrip(slot);
        }

        function refreshAll() {
            if (!attached()) return;
            for (var c in controls) if (!held(controls[c])) paintSlider(controls[c]);
            for (var t in toggles) paintToggle(toggles[t]);
            for (var s = 0; s < SLOTS; s++) paintStrip(s);
            if (noticeEl) {
                var msg = "";
                if (!synced) msg = "Loading mixer…";
                else if (!live) msg = "Waiting for Move — slot levels are not available yet.";
                noticeEl.textContent = msg;
                noticeEl.hidden = !msg;
            }
        }

        function el(tag, cls, text) {
            var e = document.createElement(tag);
            if (cls) e.className = cls;
            if (text !== undefined) e.textContent = text;
            return e;
        }

        var uid = 0;

        // A control's value as the hand sets it: shown at once, written
        // (throttled), and -- for a value the device only reports by echo --
        // kept as the model until the device says otherwise.
        function setFromHand(ctl, n) {
            var kind = ctl.kind;
            ctl.input.value = String(n);
            var wire = kind.toWire(n);
            var text = kind.text(wire);
            ctl.out.textContent = text;
            ctl.input.setAttribute("aria-valuetext", text);
            ctl.holdUntil = Date.now() + HOLD_MS;
            armSettle(ctl);
            // Pan, sends and returns are confirmed by the notify ring, which
            // can drop an echo when it overflows; silence there is not a
            // refusal, so the written value stands until the device reports
            // another. Level, mute and solo come from the slot-state segment,
            // polled 20x a second, which DOES report a refusal (an armed
            // scene takes a level as a lock) -- so those are never assumed.
            if (!fromSegment(ctl.key)) model[ctl.id] = wire;
            queueWrite(ctl, wire);
        }

        // When the hold lapses, show what the device ended up with.
        function armSettle(ctl) {
            clearTimeout(ctl.settle);
            ctl.settle = setTimeout(function () { if (!held(ctl)) paintSlider(ctl); }, HOLD_MS + 20);
        }

        function slider(slot, key, kindName, label, ariaLabel) {
            var kind = KINDS[kindName];
            var ctl = {
                id: id(slot, key), slot: slot, key: key, kind: kind,
                touching: false, holdUntil: 0, pending: null, timer: null, settle: null
            };
            var wrap = el("div", "mixer-control " + (kind.vertical ? "mixer-fader" : "mixer-knob") + " mixer-" + kindName);
            var inputId = "mixer-ctl-" + (++uid);
            var lab = el("label", "mixer-label", label);
            lab.setAttribute("for", inputId);

            // The input is the control for the keyboard and for assistive
            // technology. It does not take the POINTER: a native range jumps
            // to wherever it is touched, which on a live fader is an audible
            // step -- a stray tap on a return's track dropped it to -26 dB.
            // The track around it takes the pointer instead and moves the
            // value by how far the hand MOVES, so a tap changes nothing.
            var track = el("div", "mixer-track");
            var input = document.createElement("input");
            input.type = "range";
            input.id = inputId;
            input.min = String(kind.min);
            input.max = String(kind.max);
            input.step = String(kind.step);
            input.disabled = true;
            input.setAttribute("aria-label", ariaLabel);
            if (kind.vertical) input.setAttribute("aria-orientation", "vertical");
            track.appendChild(input);
            var out = el("output", "mixer-value", "\u2014");
            out.setAttribute("for", inputId);
            out.setAttribute("aria-hidden", "true"); // aria-valuetext carries it

            ctl.input = input;
            ctl.out = out;

            var drag = null;
            var lastTap = 0;

            var release = function () {
                drag = null;
                if (!ctl.touching) return;
                ctl.touching = false;
                ctl.holdUntil = Date.now() + HOLD_MS;
                armSettle(ctl);
            };

            track.addEventListener("pointerdown", function (e) {
                if (input.disabled || (e.button !== undefined && e.button !== 0)) return;
                e.preventDefault();
                if (track.setPointerCapture) track.setPointerCapture(e.pointerId);
                try { input.focus({ preventScroll: true }); } catch (err) { input.focus(); }
                var r = track.getBoundingClientRect();
                drag = {
                    id: e.pointerId,
                    start: kind.vertical ? e.clientY : e.clientX,
                    from: parseInt(input.value, 10),
                    len: Math.max(1, kind.vertical ? r.height : r.width),
                    moved: false
                };
                ctl.touching = true;
            });
            track.addEventListener("pointermove", function (e) {
                if (!drag || e.pointerId !== drag.id) return;
                var d = kind.vertical ? drag.start - e.clientY : e.clientX - drag.start;
                if (Math.abs(d) > 3) drag.moved = true;
                if (!drag.moved) return;
                var n = Math.round(drag.from + d / drag.len * (kind.max - kind.min));
                n = Math.max(kind.min, Math.min(kind.max, n));
                if (n !== parseInt(input.value, 10)) setFromHand(ctl, n);
            });
            var up = function (e) {
                if (!drag || e.pointerId !== drag.id) return;
                var tapped = !drag.moved;
                release();
                if (!tapped) { lastTap = 0; return; }
                // Two taps without movement: a fader to 0 dB, a pan to centre,
                // a send to off. Never through a jump -- see above.
                var now = Date.now();
                if (now - lastTap < 350) {
                    lastTap = 0;
                    setFromHand(ctl, kind.unity);
                } else {
                    lastTap = now;
                }
            };
            track.addEventListener("pointerup", up);
            track.addEventListener("pointercancel", function (e) {
                if (drag && e.pointerId === drag.id) release();
            });
            input.addEventListener("blur", release);
            // Keyboard (and assistive technology) still drive the input itself.
            input.addEventListener("input", function () {
                setFromHand(ctl, parseInt(input.value, 10));
            });

            wrap.appendChild(lab);
            wrap.appendChild(track);
            wrap.appendChild(out);
            controls[ctl.id] = ctl;
            return wrap;
        }

        function toggle(slot, key, text, ariaLabel, cls) {
            var t = { id: id(slot, key), slot: slot, key: key };
            var b = el("button", "mixer-toggle " + cls, text);
            b.type = "button";
            b.disabled = true;
            b.setAttribute("aria-label", ariaLabel);
            b.setAttribute("aria-pressed", "false");
            b.addEventListener("click", function () {
                if (!known(t.id)) return;
                // Not optimistic: the slot-state segment confirms within a poll.
                write(t, model[t.id] === "1" ? "0" : "1");
            });
            t.button = b;
            toggles[t.id] = t;
            return b;
        }

        function slotStrip(slot) {
            var n = slot + 1;
            var aux = slot >= MOVE_SLOTS;
            var strip = el("section", "mixer-strip" + (aux ? " mixer-aux" : ""));
            strip.setAttribute("data-mixer-slot", String(slot));
            if (aux) strip.title = "Aux slot (no Move track)";
            var titleId = "mixer-slot-title-" + n;
            strip.setAttribute("aria-labelledby", titleId);

            var head = el("header", "mixer-strip-head");
            var h = el("h3", "mixer-strip-title", "Slot " + n);
            h.id = titleId;
            var name = el("div", "mixer-strip-name", "—");
            names[slot] = name;
            head.appendChild(h);
            head.appendChild(name);
            strip.appendChild(head);

            strip.appendChild(slider(slot, "buses:main_send1", "send", "Send A", "Slot " + n + " send A"));
            strip.appendChild(slider(slot, "buses:main_send2", "send", "Send B", "Slot " + n + " send B"));
            strip.appendChild(slider(slot, "slot:pan", "pan", "Pan", "Slot " + n + " pan"));
            strip.appendChild(slider(slot, "slot:volume", "volume", "Level", "Slot " + n + " level"));

            var btns = el("div", "mixer-buttons");
            btns.appendChild(toggle(slot, "slot:muted", "M", "Mute slot " + n, "mixer-mute"));
            btns.appendChild(toggle(slot, "slot:soloed", "S", "Solo slot " + n, "mixer-solo"));
            strip.appendChild(btns);
            return strip;
        }

        function returnsStrip() {
            var strip = el("section", "mixer-strip mixer-returns");
            strip.setAttribute("aria-labelledby", "mixer-returns-title");
            var head = el("header", "mixer-strip-head");
            var h = el("h3", "mixer-strip-title", "Returns");
            h.id = "mixer-returns-title";
            head.appendChild(h);
            head.appendChild(el("div", "mixer-strip-name", "Send buses"));
            strip.appendChild(head);

            strip.appendChild(slider(null, "send1:to_send2", "send", "A → B", "Send A into send B"));
            var faders = el("div", "mixer-return-faders");
            faders.appendChild(slider(null, "send1:return", "ret", "Return A", "Return A level"));
            faders.appendChild(slider(null, "send2:return", "ret", "Return B", "Return B level"));
            strip.appendChild(faders);
            return strip;
        }

        function render(container) {
            controls = {};
            toggles = {};
            names = [];
            rootEl = el("div", "mixer");
            rootEl.setAttribute("role", "group");
            rootEl.setAttribute("aria-label", "Mixer");

            noticeEl = el("p", "mixer-notice text-muted", "");
            noticeEl.setAttribute("role", "status");
            container.appendChild(noticeEl);

            for (var s = 0; s < SLOTS; s++) rootEl.appendChild(slotStrip(s));
            rootEl.appendChild(returnsStrip());
            container.appendChild(rootEl);

            var hint = el("p", "mixer-hint text-muted",
                "Same controls as Slot Settings on the device. Drag to change; double-tap a fader for 0 dB, a pan for centre, a send for off.");
            container.appendChild(hint);
            refreshAll();
        }

        return { render: render, handle: handle, reset: reset };
    }

    var api = {
        create: create,
        makeTaper: makeTaper,
        formatDb: formatDb,
        formatPan: formatPan,
        KINDS: KINDS,
        VOLUME_MAX: VOLUME_MAX,
        SEND_MAX: SEND_MAX
    };
    root.SchwungMixer = api;
    if (typeof module === "object" && module.exports) module.exports = api;
})(typeof window !== "undefined" ? window : globalThis);

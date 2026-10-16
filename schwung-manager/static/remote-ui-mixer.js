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
    var MASTER_MAX = 2;       // master_fx:level, MASTER_BUS_LEVEL_MAX: +6 dB
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
    var masterTaper = makeTaper(MASTER_MAX); // the master bus fader, to +6 dB
    // The meters: ONE scale for every strip, full height = full scale
    // (0 dBFS), so the same level reads the same height everywhere.
    var meterTaper = makeTaper(1);
    var METER_AMBER_DB = -12;  // green below, amber to -3 dB, red above
    var METER_RED_DB = -3;
    var METER_CLIP = 0.999;    // -0.01 dBFS: a full-scale sample

    // A snapshot's name from its index (scene_doc.mjs): 0-15 are A1-A16,
    // 16-31 B1-B16.
    function snapshotName(i) {
        return i < 16 ? "A" + (i + 1) : "B" + (i - 15);
    }

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
        master: {  // master_fx:level, linear 0..2, on a tapered vertical fader
            min: 0, max: FADER_STEPS, step: 1, vertical: true, unity: Math.round(masterTaper.toPos(1) * FADER_STEPS),
            toInput: function (v) { return Math.round(masterTaper.toPos(parseFloat(v)) * FADER_STEPS); },
            toWire: function (n) { return masterTaper.toGain(n / FADER_STEPS).toFixed(4); },
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

    // "fx3" -> "Audio FX 3", "midi_fx2" -> "MIDI FX 2", "synth" -> "Synth".
    function COMPONENT_NAMES(comp) {
        if (comp === "synth") return "Synth";
        if (comp.indexOf("midi_fx") === 0) return "MIDI FX " + comp.slice(7);
        if (comp.indexOf("fx") === 0) return "Audio FX " + comp.slice(2);
        return comp;
    }

    // An effect knob's kind, from the range the module declares. The input is
    // integer steps across [min, max] (at most FADER_STEPS of them); the wire
    // carries the module's own units. A 0..1 range or a "%" unit reads as a
    // percentage, as the device shows it.
    function fxKind(knob) {
        var span = knob.max - knob.min;
        var steps = Math.max(1, Math.min(FADER_STEPS, Math.round(span / (knob.step > 0 ? knob.step : span / 100))));
        var decimals = knob.int ? 0 : Math.max(0, Math.min(4, Math.ceil(-Math.log(span / steps) / Math.LN10)));
        // A 0..1 range is a FRACTION, shown as 0-100% -- even when the module
        // also says "%" (Freeverb's wet is 0..1 with unit "%", and read as a
        // percentage that topped out at "1%"). Only a range already in
        // percent (0..100 with "%") is shown as it stands.
        var fraction = knob.min >= 0 && knob.max <= 1;
        var percent = fraction || knob.unit === "%";
        return {
            name: "fx", min: 0, max: steps, step: 1, vertical: false, unity: null,
            toInput: function (v) {
                var f = parseFloat(v);
                if (!isFinite(f)) return 0;
                return Math.round((Math.max(knob.min, Math.min(knob.max, f)) - knob.min) / span * steps);
            },
            toWire: function (n) {
                var f = knob.min + n / steps * span;
                return knob.int ? String(Math.round(f)) : f.toFixed(decimals);
            },
            text: function (v) {
                var f = parseFloat(v);
                if (!isFinite(f)) return "\u2014";
                if (percent) {
                    var pct = fraction ? (f - knob.min) / span * 100 : f;
                    return Math.round(pct) + "%";
                }
                return (knob.int ? String(Math.round(f)) : f.toFixed(Math.min(decimals, 2))) +
                       (knob.unit ? " " + knob.unit : "");
            }
        };
    }

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
        var selects = {};        // id -> choice control (dropdowns)
        var toggles = {};        // id -> { button, label }
        var names = [];          // per-slot name element
        // A STRIP is a slot ("0".."7") or an FX bus ("send1", "send2",
        // "master_fx"); a bus's knob keys name the bus, so their values are
        // global ("g|send1:fx1:mix") and they are written with no slot.
        var fxKnobs = {};        // strip -> the server's knob list; absent = not read
        var fxBoxes = {};        // strip -> container the effect knobs are drawn in
        var fxEmpty = {};        // strip -> what to show when it has none (buses only)
        var meters = {};         // "0".."7", "ret0", "ret1", "master" -> meter
        var armedEl = null;      // the scene-armed banner
        var moveVolEl = null;    // "Move volume -6.0 dB" on the Master strip
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
            if (msg.type === "mixer_meters") {
                paintMeters(msg);
                return;
            }
            if (msg.type === "mixer_state") {
                model = {};          // absent means unknown
                synced = true;
                if (typeof msg.live === "boolean") live = msg.live;
                fxKnobs = msg.fx || {};
                for (var fs in fxBoxes) renderFx(fs);
                merge(msg);
                refreshAll();
                return;
            }
            if (msg.type !== "mixer_update") return;
            if (typeof msg.live === "boolean") live = msg.live;
            // Knobs before values: a value is only drawn on a knob that exists.
            if (msg.fx) {
                for (var sk in msg.fx) {
                    fxKnobs[sk] = msg.fx[sk];
                    renderFx(sk);
                }
            }
            var changed = merge(msg);
            if (typeof msg.live === "boolean") { refreshAll(); return; }
            for (var i = 0; i < changed.length; i++) refreshOne(changed[i]);
        }

        // Called when the socket drops or the tab is left: nothing shown is
        // known any more until the next mixer_state.
        function reset() {
            model = {};
            synced = false;
            fxKnobs = {};
            for (var rs in fxBoxes) renderFx(rs);
            live = false;
            paintMeters(null);
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

        // The returns' and the master's muted look, as a slot strip has.
        function paintBusMutes() {
            if (!rootEl) return;
            var cols = rootEl.querySelectorAll("[data-mute-key]");
            for (var i = 0; i < cols.length; i++) {
                var mid = id(null, cols[i].getAttribute("data-mute-key"));
                cols[i].classList.toggle("muted", known(mid) && model[mid] === "1");
            }
        }

        function refreshOne(cid) {
            if (!attached()) return;
            if (cid.indexOf("g|") === 0) paintBusMutes();
            if (cid === "g|scene:edit" || cid === "g|move:volume") paintStatus();
            var ctl = controls[cid];
            if (ctl && !held(ctl)) paintSlider(ctl);
            if (selects[cid]) paintChoice(selects[cid]);
            if (toggles[cid]) paintToggle(toggles[cid]);
            var bar = cid.indexOf("|");
            var slot = parseInt(cid.slice(0, bar), 10);
            if (slot >= 0 && slot < SLOTS) paintStrip(slot);
        }

        // --- meters ----------------------------------------------------------

        // A meter beside a fader: two bars (L, R) and a clip light that stays
        // lit until clicked. Visual only -- the clip light is the one part a
        // keyboard or screen reader needs, so it is the one control.
        function meter(stripKey, label) {
            var wrap = el("div", "mixer-meter");
            var clip = el("button", "mixer-clip", "");
            clip.type = "button";
            clip.setAttribute("aria-label", "Clear clip, " + label);
            clip.title = "Clipped -- click to clear";
            clip.hidden = true;
            var bars = el("div", "mixer-meter-bars");
            bars.setAttribute("aria-hidden", "true");
            var amber = Math.round(meterTaper.toPos(dbToGain(METER_AMBER_DB)) * 1000) / 10;
            var red = Math.round(meterTaper.toPos(dbToGain(METER_RED_DB)) * 1000) / 10;
            var masks = [];
            for (var c = 0; c < 2; c++) {
                var bar = el("div", "mixer-meter-bar");
                bar.style.background = "linear-gradient(to top, var(--meter-green) 0%, var(--meter-green) " + amber +
                    "%, var(--meter-amber) " + amber + "%, var(--meter-amber) " + red + "%, var(--meter-red) " + red + "%)";
                var mask = el("div", "mixer-meter-mask");
                bar.appendChild(mask);
                bars.appendChild(bar);
                masks.push(mask);
            }
            var m = { masks: masks, clip: clip };
            // Inside the fader's track: never the start of a fader drag.
            clip.addEventListener("pointerdown", function (e) { e.stopPropagation(); });
            clip.addEventListener("click", function (e) { e.stopPropagation(); clip.hidden = true; });
            wrap.appendChild(clip);
            wrap.appendChild(bars);
            meters[stripKey] = m;
            return wrap;
        }

        // Put a meter beside a fader (inside its track, so it is exactly the
        // fader's height).
        function withMeter(faderWrap, stripKey, label) {
            var track = faderWrap.querySelector(".mixer-track");
            if (track) track.appendChild(meter(stripKey, label));
            return faderWrap;
        }

        function paintMeter(m, pair) {
            for (var c = 0; c < 2; c++) {
                var v = pair ? pair[c] : 0;
                var pos = (v > 0) ? meterTaper.toPos(Math.min(1, v)) : 0;
                m.masks[c].style.height = (100 - pos * 100).toFixed(1) + "%";
                // Full scale: an int16 peak tops out at 32767/32768.
                if (v >= METER_CLIP) m.clip.hidden = false;
            }
        }

        function paintMeters(msg) {
            if (!attached()) return;
            for (var k in meters) {
                var pair = null;
                if (msg) {
                    if (k === "master") pair = msg.master;
                    else if (k.indexOf("ret") === 0) pair = (msg.returns || [])[parseInt(k.slice(3), 10)];
                    else pair = (msg.slots || [])[parseInt(k, 10)];
                }
                paintMeter(meters[k], pair);
            }
        }

        // The scene-armed banner and Move's volume -- both read-only, both
        // from the same block as the meters.
        function paintStatus() {
            if (armedEl) {
                var e = parseInt(model["g|scene:edit"], 10);
                var on = synced && isFinite(e) && e >= 0;
                armedEl.hidden = !on;
                if (on) {
                    armedEl.textContent = "Snapshot " + snapshotName(e) +
                        " is armed: level, pan and send changes are saved to the snapshot, not played live.";
                }
            }
            if (moveVolEl) {
                var mv = known("g|move:volume") ? parseFloat(model["g|move:volume"]) : NaN;
                moveVolEl.textContent = isFinite(mv) ? "Move " + formatDb(mv) : "";
            }
        }

        function refreshAll() {
            if (!attached()) return;
            paintStatus();
            for (var c in controls) if (!held(controls[c])) paintSlider(controls[c]);
            for (var sc in selects) paintChoice(selects[sc]);
            for (var t in toggles) paintToggle(toggles[t]);
            for (var s = 0; s < SLOTS; s++) paintStrip(s);
            paintBusMutes();
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
            var kind = (typeof kindName === "string") ? KINDS[kindName] : kindName;
            if (typeof kindName !== "string") kindName = kind.name;
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
                if (now - lastTap < 350 && kind.unity !== null) {
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

        // The FX buses' strips, and how a bus's knobs are named.
        var BUS_NAMES = { send1: "Send A", send2: "Send B", master_fx: "Master FX" };

        // One knob per pin the server has for this strip -- an effect's mix
        // by default, anything else pinned from a Slot or bus tab. Rebuilt
        // only when the strip's knob LIST changes; values update in place like
        // any other control.
        function renderFx(strip) {
            strip = String(strip);
            var box = fxBoxes[strip];
            if (!box) return;
            var busName = BUS_NAMES[strip] || "";
            // A bus's controls are global (slot null); a slot's are its own.
            var slot = busName ? null : parseInt(strip, 10);
            for (var cid in controls) {
                if (controls[cid].fx && controls[cid].strip === strip) {
                    clearTimeout(controls[cid].timer);
                    clearTimeout(controls[cid].settle);
                    delete controls[cid];
                }
            }
            for (var sid in selects) {
                if (selects[sid].strip === strip) delete selects[sid];
            }
            box.textContent = "";
            var list = fxKnobs[strip] || [];
            box.hidden = list.length === 0;
            if (fxEmpty[strip]) fxEmpty[strip].hidden = list.length > 0 || !(strip in fxKnobs);
            if (!list.length) return;
            var lastGroup = null;
            var where = busName || ("Slot " + (slot + 1));
            for (var i = 0; i < list.length; i++) {
                var k = list[i];
                // The server decides: an effect's mix reads as one of the
                // strip's Effects, anything else pinned reads as Pinned.
                var group = k.effect ? "Effects" : "Pinned";
                if (group !== lastGroup) {
                    // On the returns strip two buses share a column pair, so
                    // each says whose effects these are.
                    var title = (busName && strip !== "master_fx") ? busName + " " + group.toLowerCase() : group;
                    box.appendChild(el("div", "mixer-fx-title", title));
                    lastGroup = group;
                }
                var pos = COMPONENT_NAMES(k.comp);
                var label = (k.comp === "synth" ? "" : k.module + " ") + k.name.toLowerCase();
                var aria = where + " " + pos + " " + k.module + " " + k.name;
                var wrap = (k.options && k.options.length) ? choice(slot, k, label, aria)
                                                           : slider(slot, k.key, fxKind(k), label, aria);
                var ctl = controls[id(slot, k.key)];
                if (ctl) { ctl.fx = true; ctl.strip = strip; }
                if (selects[id(slot, k.key)]) selects[id(slot, k.key)].strip = strip;
                var lab = wrap.querySelector(".mixer-label");
                lab.title = pos + " \u00b7 " + k.module + " \u00b7 " + k.name +
                    (k.guessed ? " (the Mixer picked this parameter by its name)" : "");
                if (k.guessed) wrap.classList.add("mixer-guessed");
                if (k.pinned) {
                    wrap.classList.add("mixer-pinned");
                    var un = el("button", "mixer-unpin", "\u00d7");
                    un.type = "button";
                    un.setAttribute("aria-label", "Unpin " + k.name + " from " + (busName || "slot " + (slot + 1)));
                    un.title = "Unpin";
                    (function (key) {
                        un.addEventListener("click", function () {
                            // A bus key names its own strip; a slot's needs the slot.
                            var m = { type: "pin_mixer", key: key, value: "0" };
                            if (slot !== null) m.slot = slot;
                            sendMsg(m);
                        });
                    })(k.key);
                    wrap.insertBefore(un, wrap.firstChild);
                }
                box.appendChild(wrap);
                if (attached()) {
                    if (controls[id(slot, k.key)]) paintSlider(controls[id(slot, k.key)]);
                    if (selects[id(slot, k.key)]) paintChoice(selects[id(slot, k.key)]);
                }
            }
        }

        // A CHOICE parameter (an enum) on a strip: a native select, so the
        // keyboard and a screen reader get it for free. Its value is the
        // option's index; nothing is assumed -- the device's echo sets it.
        function choice(slot, k, label, aria) {
            var c = { id: id(slot, k.key), slot: slot, key: k.key, options: k.options };
            var wrap = el("div", "mixer-control mixer-choice");
            var selId = "mixer-ctl-" + (++uid);
            var lab = el("label", "mixer-label", label);
            lab.setAttribute("for", selId);
            var sel = document.createElement("select");
            sel.id = selId;
            sel.className = "mixer-select";
            sel.setAttribute("aria-label", aria);
            sel.disabled = true;
            for (var i = 0; i < k.options.length; i++) {
                var o = document.createElement("option");
                o.value = String(i);
                o.textContent = k.options[i];
                sel.appendChild(o);
            }
            sel.addEventListener("change", function () {
                write(c, sel.value);
            });
            c.select = sel;
            wrap.appendChild(lab);
            wrap.appendChild(sel);
            selects[c.id] = c;
            return wrap;
        }

        function paintChoice(c) {
            var ok = known(c.id);
            c.select.disabled = !ok;
            if (ok && document.activeElement !== c.select) c.select.value = String(parseInt(model[c.id], 10));
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
            var fxBox = el("div", "mixer-fx");
            fxBox.setAttribute("role", "group");
            fxBox.setAttribute("aria-label", "Slot " + n + " effects");
            fxBoxes[String(slot)] = fxBox;
            strip.appendChild(fxBox);
            strip.appendChild(withMeter(slider(slot, "slot:volume", "volume", "Level", "Slot " + n + " level"),
                                        String(slot), "slot " + n));

            var btns = el("div", "mixer-buttons");
            btns.appendChild(toggle(slot, "slot:muted", "M", "Mute slot " + n, "mixer-mute"));
            btns.appendChild(toggle(slot, "slot:soloed", "S", "Solo slot " + n, "mixer-solo"));
            strip.appendChild(btns);
            return strip;
        }

        // A bus's effect box. Bus knobs are written with no slot.
        function busFxBox(bus, ariaLabel) {
            var box = el("div", "mixer-fx");
            box.setAttribute("role", "group");
            box.setAttribute("aria-label", ariaLabel);
            box.hidden = true;
            fxBoxes[bus] = box;
            return box;
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
            // Each send's effects, across the strip's full width (two half
            // columns cut every name short), then the two return faders.
            strip.appendChild(busFxBox("send1", "Send A effects"));
            strip.appendChild(busFxBox("send2", "Send B effects"));
            // Each return's fader, with its mute under it.
            var faders = el("div", "mixer-return-faders");
            // One call per control, keys written out: test_remote_ui_mixer.sh
            // checks them against the server's list.
            var col = function (muteKey, fader, mute) {
                var c = el("div", "mixer-return-col");
                c.setAttribute("data-mute-key", muteKey);
                c.appendChild(fader);
                var btns = el("div", "mixer-buttons");
                btns.appendChild(mute);
                c.appendChild(btns);
                return c;
            };
            faders.appendChild(col("send1:return_muted",
                withMeter(slider(null, "send1:return", "ret", "Return A", "Return A level"), "ret0", "return A"),
                toggle(null, "send1:return_muted", "M", "Mute return A", "mixer-mute")));
            faders.appendChild(col("send2:return_muted",
                withMeter(slider(null, "send2:return", "ret", "Return B", "Return B level"), "ret1", "return B"),
                toggle(null, "send2:return_muted", "M", "Mute return B", "mixer-mute")));
            strip.appendChild(faders);
            return strip;
        }

        // Master: the master bus's effect mixes and anything pinned from the
        // Master FX tab, then its level -- a software level after Master FX and
        // before Move's volume knob, which stays the final output -- its mute,
        // and FX, which bypasses the whole Master FX chain.
        function masterStrip() {
            var strip = el("section", "mixer-strip mixer-master");
            strip.setAttribute("data-mute-key", "master_fx:muted");
            strip.setAttribute("aria-labelledby", "mixer-master-title");
            var head = el("header", "mixer-strip-head");
            var h = el("h3", "mixer-strip-title", "Master");
            h.id = "mixer-master-title";
            head.appendChild(h);
            head.appendChild(el("div", "mixer-strip-name", "Master FX"));
            // Move's own master volume, after this bus: shown, never set. In
            // the header, so the strip's fader stays in line with the others.
            moveVolEl = el("div", "mixer-move-volume text-muted", "");
            moveVolEl.title = "Move's own volume knob, after the master bus. Schwung can show it but not set it.";
            head.appendChild(moveVolEl);
            strip.appendChild(head);
            strip.appendChild(busFxBox("master_fx", "Master FX effects"));
            var empty = el("p", "mixer-empty text-muted",
                "No effects pinned. A Master FX effect's mix appears when it is loaded; pin more from the Master FX tab.");
            empty.hidden = true;
            fxEmpty.master_fx = empty;
            strip.appendChild(empty);
            strip.appendChild(withMeter(slider(null, "master_fx:level", "master", "Level", "Master level"),
                                        "master", "the master bus"));
            var btns = el("div", "mixer-buttons");
            btns.appendChild(toggle(null, "master_fx:muted", "M", "Mute the master bus", "mixer-mute"));
            var byp = toggle(null, "master_fx:chain_bypassed", "FX", "Bypass all Master FX", "mixer-bypass");
            byp.title = "Bypass all Master FX";
            btns.appendChild(byp);
            strip.appendChild(btns);
            return strip;
        }

        function render(container) {
            controls = {};
            selects = {};
            toggles = {};
            names = [];
            fxBoxes = {};
            fxEmpty = {};
            meters = {};
            rootEl = el("div", "mixer");
            rootEl.setAttribute("role", "group");
            rootEl.setAttribute("aria-label", "Mixer");

            noticeEl = el("p", "mixer-notice text-muted", "");
            noticeEl.setAttribute("role", "status");
            container.appendChild(noticeEl);
            armedEl = el("p", "mixer-armed", "");
            armedEl.setAttribute("role", "status");
            armedEl.hidden = true;
            container.appendChild(armedEl);

            for (var s = 0; s < SLOTS; s++) rootEl.appendChild(slotStrip(s));
            rootEl.appendChild(returnsStrip());
            rootEl.appendChild(masterStrip());
            for (var fs2 in fxBoxes) renderFx(fs2);
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
        fxKind: fxKind,
        snapshotName: snapshotName,
        meterTaper: meterTaper,
        VOLUME_MAX: VOLUME_MAX,
        SEND_MAX: SEND_MAX
    };
    root.SchwungMixer = api;
    if (typeof module === "object" && module.exports) module.exports = api;
})(typeof window !== "undefined" ? window : globalThis);

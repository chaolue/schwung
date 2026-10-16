/* ================================================================
   Remote UI - WebSocket client for shadow slot parameter control
   ================================================================ */

(function () {
    "use strict";

    // EVERY position a chain slot has: the synth, eight audio FX (MAX_AUDIO_FX)
    // and eight MIDI FX (MAX_MIDI_FX). This was the first four, so a module in
    // fx3 or midi_fx2 loaded, ran and had no controls here at all. The server
    // only ever reports the ones that are loaded, and only those are drawn.
    var COMPONENT_KEYS = ["synth",
        "fx1", "fx2", "fx3", "fx4", "fx5", "fx6", "fx7", "fx8",
        "midi_fx1", "midi_fx2", "midi_fx3", "midi_fx4",
        "midi_fx5", "midi_fx6", "midi_fx7", "midi_fx8"];
    // The order components are DRAWN in, which is signal flow and not the order
    // they are enumerated in. Both render paths read this one list; when they
    // each carried their own, the custom-synth path drew the MIDI FX last.
    var COMPONENT_ORDER = ["midi_fx1", "midi_fx2", "midi_fx3", "midi_fx4",
        "midi_fx5", "midi_fx6", "midi_fx7", "midi_fx8", "synth",
        "fx1", "fx2", "fx3", "fx4", "fx5", "fx6", "fx7", "fx8"];
    var COMPONENT_LABELS = { synth: "Synth" };
    for (var cl = 1; cl <= 8; cl++) {
        COMPONENT_LABELS["fx" + cl] = "Audio FX " + cl;
        COMPONENT_LABELS["midi_fx" + cl] = "MIDI FX " + cl;
    }

    // The three FX BUSES: Master FX and the two sends. One shape to the shim
    // -- eight positions (MASTER_FX_SLOTS), keys "<bus>:fx<N>:<param>" on slot
    // 0 -- so one tab renderer with three prefixes. `tab` is the activeSlot
    // value (Master FX's predates the sends), `id` the shim's prefix.
    var FX_BUSES = [
        { tab: "master", id: "master_fx", label: "Master FX", hash: "master-fx" },
        { tab: "send1", id: "send1", label: "Send A", hash: "send-a" },
        { tab: "send2", id: "send2", label: "Send B", hash: "send-b" }
    ];

    function makeComponent() {
        return { hierarchy: null, chainParams: null, params: {}, navStack: ["root"], module: "" };
    }

    function makeComponents(keys) {
        var out = {};
        for (var i = 0; i < keys.length; i++) out[keys[i]] = makeComponent();
        return out;
    }

    function makeSlot() {
        return {
            components: makeComponents(COMPONENT_KEYS),
            // Track which component sections the user has folded. A key is
            // absent until the header is clicked -- see sectionCollapsed(),
            // which derives the default from whether the component ships a
            // panel. Storing `true` here for every non-synth component hid a
            // MIDI FX's whole web_ui.html behind a one-line disclosure
            // triangle, below a 3000px synth panel, with nothing to say a
            // panel was there at all.
            collapsed: {},
            // Custom web UI URLs, keyed by component ("synth", "fx1", …).
            // A component absent from this map uses the auto-generated UI.
            customUI: {},
            // When true, ignore customUI and show the auto-generated parameter
            // UI even though a module web_ui.html is available. Session-only.
            useDefaultUI: false,
            // Knobs 1-8's mappings, from slot_knobs: [{knob, comp, param, key}].
            // null until the server has answered.
            knobMap: null,
            // Keys pinned to the Mixer ("synth:cutoff"), from mixer_pins.
            pins: []
        };
    }

    // Per-slot cached state.
    // Every chain slot: Move's four tracks, then the four aux slots.
    var SLOT_COUNT = 8;
    var slots = [];
    for (var si = 0; si < SLOT_COUNT; si++) slots.push(makeSlot());

    // Per-bus state, keyed by tab.
    function makeBus(def) {
        var keys = [], labels = {};
        for (var n = 1; n <= 8; n++) {
            keys.push(def.id + ":fx" + n);
            labels[def.id + ":fx" + n] = def.label + (def.tab === "master" ? " " : " FX ") + n;
        }
        return {
            def: def,
            keys: keys,
            labels: labels,
            components: makeComponents(keys),
            collapsed: {},
            // Keyed by component, exactly as a slot's is: two positions on one
            // bus can each ship a panel and must not collide.
            customUI: {},
            // The same Custom/Default escape hatch a slot has, so a custom panel
            // can never make a bus parameter unreachable.
            useDefaultUI: false,
            // Keys pinned to the Mixer ("send1:fx1:mix"), from mixer_pins.
            pins: []
        };
    }
    var buses = {};
    for (var bi = 0; bi < FX_BUSES.length; bi++) buses[FX_BUSES[bi].tab] = makeBus(FX_BUSES[bi]);

    function isBusTab(v) { return typeof v === "string" && Object.prototype.hasOwnProperty.call(buses, v); }
    function busById(id) {
        for (var i = 0; i < FX_BUSES.length; i++) if (FX_BUSES[i].id === id) return buses[FX_BUSES[i].tab];
        return null;
    }
    /** The bus whose position a key or component names ("send1:fx2:mix", "master_fx:fx1"). */
    function busOfKey(key) {
        for (var i = 0; i < FX_BUSES.length; i++) {
            if (key.indexOf(FX_BUSES[i].id + ":fx") === 0) return buses[FX_BUSES[i].tab];
        }
        // Every other master_fx: key belongs to its tab too, as it always has.
        if (key.indexOf("master_fx:") === 0) return buses.master;
        return null;
    }

    // Active overtake-tool state. A tool is not a chain slot; its
    // params live under the "overtake_dsp:" prefix and its UI shows in the Tool
    // tab. id == "" means no tool with a remote UI is loaded.
    // known=false until the first tool_info answer arrives, so the Tool tab can
    // show "checking…" instead of a premature (and alarming) "No tool loaded".
    var tool = { id: "", customUI: null, params: {}, known: false };

    // The Mixer tab (static/remote-ui-mixer.js): every slot's level, pan and
    // sends plus the send returns on one page. Its own model; it only needs
    // the socket.
    var mixer = window.SchwungMixer ? window.SchwungMixer.create({ send: function (m) { send(m); } }) : null;

    // Tabs that are not a chain slot index.
    function isNamedTab(v) { return isBusTab(v) || v === "tool" || v === "mixer"; }

    // Read initial slot from URL hash (#slot1 .. #slot8, #master-fx, #send-a,
    // #send-b, #tool, #mixer)
    var initialSlot = 0;
    (function() {
        var h = location.hash.replace("#", "");
        for (var i = 0; i < FX_BUSES.length; i++) if (h === FX_BUSES[i].hash) initialSlot = FX_BUSES[i].tab;
        if (h === "tool") initialSlot = "tool";
        else if (h === "mixer") initialSlot = "mixer";
        else if (/^slot[1-8]$/.test(h)) initialSlot = parseInt(h.charAt(4), 10) - 1;
    })();
    var activeSlot = initialSlot;
    var ws = null;
    var reconnectTimer = null;
    var reconnectDelay = 1000; // Start at 1s, exponential backoff
    var reconnectAttempts = 0;
    var RECONNECT_DELAY_MIN = 1000;
    var RECONNECT_DELAY_MAX = 10000;
    var isConnected = false;
    var waitingForData = false; // True after subscribe, before first data arrives

    // Custom module web UI state. A slot can show more than one at once (a
    // synth panel plus a panel for each audio FX), so frames are tracked per
    // component rather than as a single active iframe.
    //   customUIFrames[component] = { iframe, subscribed, overlay, timer }
    var customUIFrames = {};

    // Knob drag state.
    var dragging = null; // { component, key, startY, startValue, min, max, step, type, slot }
    var sendThrottleTimer = null;
    var SEND_INTERVAL = 16; // ~60Hz

    // DOM references.
    var statusEl = document.getElementById("remote-ui-status");
    var slotTitleEl = document.getElementById("slot-title");
    var slotHeaderControlsEl = document.getElementById("slot-header-controls");
    var slotNameEl = document.getElementById("slot-name");
    var slotContentEl = document.getElementById("slot-content");
    var debugEl = document.getElementById("slot-debug");
    var tabButtons = document.querySelectorAll(".remote-ui-tab");

    // ------------------------------------------------------------------
    // WebSocket connection
    // ------------------------------------------------------------------

    function connect() {
        if (ws && (ws.readyState === WebSocket.CONNECTING || ws.readyState === WebSocket.OPEN)) {
            return;
        }
        var proto = location.protocol === "https:" ? "wss:" : "ws:";
        ws = new WebSocket(proto + "//" + location.host + "/ws/remote-ui");

        ws.onopen = function () {
            reconnectDelay = RECONNECT_DELAY_MIN;
            reconnectAttempts = 0;
            isConnected = true;
            setStatus(true);
            waitingForData = true;
            renderSlot(); // Show loading state
            subscribe(activeSlot);
        };

        ws.onmessage = function (evt) {
            var msg;
            try {
                msg = JSON.parse(evt.data);
            } catch (e) {
                return;
            }
            if (waitingForData) {
                waitingForData = false;
            }
            dispatch(msg);
        };

        ws.onclose = function () {
            isConnected = false;
            if (mixer) mixer.reset();
            setStatus(false);
            scheduleReconnect();
        };

        ws.onerror = function () {};
    }

    function scheduleReconnect() {
        if (reconnectTimer) return;
        reconnectAttempts++;
        reconnectTimer = setTimeout(function () {
            reconnectTimer = null;
            connect();
        }, reconnectDelay);
        // Update status with attempt count
        setStatus(false);
        // Exponential backoff: double delay, cap at max
        reconnectDelay = Math.min(reconnectDelay * 2, RECONNECT_DELAY_MAX);
    }

    function send(obj) {
        if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify(obj));
        }
    }

    function subscribe(slot) {
        if (isBusTab(slot)) {
            send({ type: "subscribe_bus", key: buses[slot].def.id });
        } else if (slot === "mixer") {
            send({ type: "subscribe_mixer" });
        } else if (slot === "tool") {
            send({ type: "subscribe_tool" });
        } else {
            send({ type: "subscribe", slot: slot });
        }
    }

    function unsubscribe(slot) {
        if (isBusTab(slot)) {
            send({ type: "unsubscribe_bus", key: buses[slot].def.id });
        } else if (slot === "mixer") {
            send({ type: "unsubscribe_mixer" });
            if (mixer) mixer.reset();
        } else if (slot === "tool") {
            send({ type: "unsubscribe_tool" });
        } else {
            send({ type: "unsubscribe", slot: slot });
        }
    }

    // ------------------------------------------------------------------
    // Message dispatch
    // ------------------------------------------------------------------

    function dispatch(msg) {
        switch (msg.type) {
            case "master_fx_info":
                handleBusInfo(msg);
                return;
            case "tool_info":
                handleToolInfo(msg);
                return;
            case "mixer_state":
            case "mixer_update":
                if (mixer) mixer.handle(msg);
                return;
            case "rename_result":
                handleRenameResult(msg);
                return;
            case "error":
                if (String(msg.message || "").indexOf("pin_mixer:") === 0) {
                    announcePin(String(msg.message).slice("pin_mixer:".length).trim());
                }
                return;
            default:
                break;
        }

        var slot = msg.slot;

        // Tool custom UI (overtake tool): not a chain slot.
        var comp = msg.component || "";
        if (comp === "tool") {
            if (msg.type === "custom_ui") handleToolCustomUI(msg);
            return;
        }

        // param_update carrying overtake_dsp: keys belongs to the Tool tab.
        if (msg.type === "param_update" && msg.params) {
            var hasOvertake = false, hasNonOvertake = false;
            for (var ok in msg.params) {
                if (ok.indexOf("overtake_dsp:") === 0) hasOvertake = true;
                else hasNonOvertake = true;
            }
            if (hasOvertake) {
                handleToolParamUpdate(msg);
                if (!hasNonOvertake) return;
            }
        }

        // An FX bus position's message (slot 0, "<bus>:fxN" component).
        var compBus = comp ? busOfKey(comp) : null;
        if (compBus) {
            switch (msg.type) {
                case "hierarchy":
                    handleBusHierarchy(compBus, comp, msg);
                    break;
                case "chain_params":
                    handleBusChainParams(compBus, comp, msg);
                    break;
                case "custom_ui":
                    handleBusCustomUI(compBus, comp, msg);
                    break;
            }
            return;
        }

        // A param_update carrying FX bus keys.
        if (msg.type === "param_update" && msg.params) {
            var hasBus = false;
            var hasSlot = false;
            for (var k in msg.params) {
                if (busOfKey(k)) hasBus = true;
                else hasSlot = true;
            }
            if (hasBus) {
                handleBusParamUpdate(msg);
            }
            if (!hasSlot) return;
        }

        // An FX bus's Mixer pins.
        if (msg.type === "mixer_pins" && msg.bus) {
            var pinBus = busById(msg.bus);
            if (pinBus) {
                pinBus.pins = msg.keys || [];
                if (buses[activeSlot] === pinBus) refreshPinButtons();
            }
            return;
        }

        if (slot < 0 || slot >= SLOT_COUNT) return;

        switch (msg.type) {
            case "slot_info":
                handleSlotInfo(slot, msg);
                break;

            case "custom_ui":
                handleCustomUI(slot, msg);
                break;

            case "hierarchy":
                handleHierarchy(slot, msg);
                break;

            case "chain_params":
                handleChainParamsMsg(slot, msg);
                break;

            case "param_update":
                handleParamUpdate(slot, msg);
                break;

            case "slot_knobs":
                slots[slot].knobMap = msg.knobs || [];
                if (slot === activeSlot) renderSlot();
                break;

            case "mixer_pins":
                slots[slot].pins = msg.keys || [];
                if (slot === activeSlot) refreshPinButtons();
                break;

            default:
                break;
        }
    }

    function handleSlotInfo(slot, msg) {
        var s = slots[slot];
        // A component's custom UI belongs to the module that supplied it, so
        // drop it when that particular component's module changes.
        // `modules` names every loaded position; an older server sent only
        // these four fields.
        var incoming = msg.modules || {
            synth: msg.synth, fx1: msg.fx1, fx2: msg.fx2, midi_fx1: msg.midi_fx1
        };
        for (var ci = 0; ci < COMPONENT_KEYS.length; ci++) {
            var ck = COMPONENT_KEYS[ci];
            var mod = incoming[ck] || "";
            if (s.components[ck].module !== mod) {
                s.components[ck].module = mod;
                delete s.customUI[ck];
            }
        }
        if (slot === activeSlot) renderSlot();
    }

    function handleCustomUI(slot, msg) {
        if (slot < 0 || slot >= SLOT_COUNT) return;
        var s = slots[slot];
        var comp = msg.component || "synth";
        var url = msg.url || "";
        if (s.customUI[comp] === url) return;   // avoid reloading a live iframe
        s.customUI[comp] = url;
        if (slot === activeSlot) renderSlot();
    }

    // ------------------------------------------------------------------
    // Overtake tool (Tool tab)
    // ------------------------------------------------------------------

    function handleToolInfo(msg) {
        tool.known = true;
        tool.id = msg.id || "";
        if (!tool.id) {
            // Tool unloaded — drop its custom UI + cached params.
            tool.customUI = null;
            tool.params = {};
        }
        if (activeSlot === "tool") renderSlot();
    }

    function handleToolCustomUI(msg) {
        tool.customUI = { url: msg.url || "" };
        if (activeSlot === "tool") renderSlot();
    }

    function handleToolParamUpdate(msg) {
        for (var k in msg.params) {
            if (Object.prototype.hasOwnProperty.call(msg.params, k)) {
                tool.params[k] = msg.params[k];
            }
        }
        // Forward live updates to the tool iframe if it's the active view.
        if (activeSlot === "tool") {
            postToIframe({ type: "paramUpdate", params: msg.params });
        }
    }

    /** Preserve the open submenu (navStack) across a hierarchy re-broadcast when
     * the structure is unchanged. The poll loop re-sends ui_hierarchy whenever
     * its JSON changes — and that JSON embeds live param values, so an edit
     * (from the web, from Move, or from an LFO) makes it differ and triggers a
     * rebroadcast every couple seconds. Resetting navStack unconditionally
     * collapses the user's open submenu (and can render blank when a stale level
     * is referenced). Keep the existing navStack if every level it points to
     * still exists in the new hierarchy; otherwise fall back to root. */
    function reconcileNavStack(c, newData) {
        var levels = newData && newData.levels;
        if (!levels || !Array.isArray(c.navStack) || c.navStack.length === 0) {
            return ["root"];
        }
        for (var i = 0; i < c.navStack.length; i++) {
            if (!levels[c.navStack[i]]) return ["root"];
        }
        return c.navStack;
    }

    /** Build a fingerprint of the navigation STRUCTURE only — level names plus
     * each level's param/child identifiers, knob mappings, and list/select
     * param keys. Excludes labels and any embedded values. Used to decide
     * whether a hierarchy re-broadcast actually changed the structure or is
     * just value churn (some modules bake live values into ui_hierarchy, which
     * makes the poll loop re-send it every couple seconds). When the structure
     * is unchanged we skip the full re-render — live values still arrive via the
     * param-notify path (updateParamValues), so nothing is lost but the flicker. */
    function hierarchyStructureKey(data) {
        if (!data || !data.levels) return "";
        var levelNames = Object.keys(data.levels).sort();
        var parts = [];
        for (var i = 0; i < levelNames.length; i++) {
            var ln = levelNames[i];
            var lvl = data.levels[ln] || {};
            var ids = [];
            if (Array.isArray(lvl.params)) {
                for (var j = 0; j < lvl.params.length; j++) {
                    var p = lvl.params[j];
                    if (typeof p === "string") ids.push(p);
                    else if (p && p.key) ids.push("k:" + p.key);
                    else if (p && p.level) ids.push("L:" + p.level);
                }
            }
            if (Array.isArray(lvl.knobs)) ids.push("kb:" + lvl.knobs.join(","));
            var selFields = ["list_param", "count_param", "items_param", "select_param"];
            for (var s = 0; s < selFields.length; s++) {
                if (lvl[selFields[s]]) ids.push(selFields[s] + ":" + lvl[selFields[s]]);
            }
            parts.push(ln + "{" + ids.join("|") + "}");
        }
        return parts.join(";");
    }

    function handleHierarchy(slot, msg) {
        var comp = msg.component || "synth";
        var c = slots[slot].components[comp];
        if (!c) return;
        var sameStructure = c.hierarchy &&
            hierarchyStructureKey(c.hierarchy) === hierarchyStructureKey(msg.data);
        c.navStack = reconcileNavStack(c, msg.data);
        c.hierarchy = msg.data;
        /* Value-only churn (structure unchanged): skip the re-render to avoid
         * periodic flicker; values update via the notify path. */
        if (sameStructure) return;
        if (slot === activeSlot) renderSlot();
    }

    function handleChainParamsMsg(slot, msg) {
        var comp = msg.component || "synth";
        var c = slots[slot].components[comp];
        if (!c) return;
        c.chainParams = msg.data;
        if (slot === activeSlot) renderSlot();
    }

    function handleParamUpdate(slot, msg) {
        if (!msg.params) return;
        var s = slots[slot];
        var compsWithNonPresetKeys = {};
        for (var prefixedKey in msg.params) {
            // Route to correct component based on prefix.
            var parts = splitPrefix(prefixedKey);
            var comp = s.components[parts.comp];
            if (comp) {
                comp.params[prefixedKey] = msg.params[prefixedKey];
            }
            // Track which components received param values beyond preset metadata,
            // so we can clear the preset-change loading state when fresh values arrive.
            if (parts.key !== "preset" && parts.key !== "preset_count" && parts.key !== "preset_name") {
                compsWithNonPresetKeys[parts.comp] = true;
            }
        }
        // Clear preset-loading markers for any component that just received real values.
        for (var ck in compsWithNonPresetKeys) {
            if (s.components[ck] && s.components[ck].pendingPresetChange) {
                s.components[ck].pendingPresetChange = false;
                if (slot === activeSlot) {
                    var sec = slotContentEl.querySelector('.component-section[data-component="' + ck + '"]');
                    if (sec) sec.classList.remove('loading-preset');
                }
            }
        }
        if (slot === activeSlot && "slot:name" in msg.params) renderSlotName();
        if (slot === activeSlot) {
            updateParamValues(slot, msg.params);
        }
        // Forward to custom UI iframe if subscribed.
        if (slot === activeSlot) {
            postToIframe({ type: "paramUpdate", params: msg.params });
        }
    }

    // ------------------------------------------------------------------
    // FX bus message handlers (Master FX, Send A, Send B)
    // ------------------------------------------------------------------

    function handleBusInfo(msg) {
        var bus = busById(msg.bus || "master_fx");
        if (!bus) return;
        // `modules` is keyed "fx1".."fx8"; an older server sent fx1..fx4.
        var given = msg.modules || { fx1: msg.fx1, fx2: msg.fx2, fx3: msg.fx3, fx4: msg.fx4 };
        for (var i = 0; i < bus.keys.length; i++) {
            var ck = bus.keys[i];
            var mod = given[ck.slice(bus.def.id.length + 1)] || "";
            if (bus.components[ck].module !== mod) {
                bus.components[ck].module = mod;
                // The panel belonged to the module that just left. Dropping it
                // here is what makes a REMOVED module lose its iframe: the
                // server only sends custom_ui for a module that has one, so
                // nothing would otherwise clear it and the old panel would
                // keep addressing a position holding something else.
                delete bus.customUI[ck];
            }
        }
        if (buses[activeSlot] === bus) renderSlot();
    }

    function handleBusCustomUI(bus, comp, msg) {
        var url = msg.url || "";
        if (bus.customUI[comp] === url) return;  // avoid reloading a live iframe
        bus.customUI[comp] = url;
        if (buses[activeSlot] === bus) renderSlot();
    }

    function handleBusHierarchy(bus, comp, msg) {
        var c = bus.components[comp];
        if (!c) return;
        var sameStructure = c.hierarchy &&
            hierarchyStructureKey(c.hierarchy) === hierarchyStructureKey(msg.data);
        c.navStack = reconcileNavStack(c, msg.data);
        c.hierarchy = msg.data;
        /* Value-only churn (structure unchanged): skip the re-render to avoid
         * periodic flicker; values update via the notify path. */
        if (sameStructure) return;
        if (buses[activeSlot] === bus) renderSlot();
    }

    function handleBusChainParams(bus, comp, msg) {
        var c = bus.components[comp];
        if (!c) return;
        c.chainParams = msg.data;
        if (buses[activeSlot] === bus) renderSlot();
    }

    function handleBusParamUpdate(msg) {
        if (!msg.params) return;
        for (var fullKey in msg.params) {
            // fullKey is like "master_fx:fx1:cutoff" or "send1:fx2:mix"
            var parts = splitBusKey(fullKey);
            if (parts) {
                var comp = parts.bus.components[parts.comp];
                if (comp) {
                    comp.params[fullKey] = msg.params[fullKey];
                }
            }
        }
        var active = buses[activeSlot];
        if (active) {
            updateBusParamValues(msg.params);
            // ...and to any custom panels. Grouped by position rather than
            // broadcast: fx2's panel has no business seeing fx1's values, and
            // two positions each showing a panel is explicitly supported.
            var byComp = {};
            for (var fk in msg.params) {
                var pr = splitBusKey(fk);
                if (!pr || pr.bus !== active) continue;
                (byComp[pr.comp] || (byComp[pr.comp] = {}))[fk] = msg.params[fk];
            }
            for (var ck in byComp) {
                postToIframe({ type: "paramUpdate", params: byComp[ck] }, ck);
            }
        }
    }

    /** Split "send1:fx1:cutoff" -> { bus, comp: "send1:fx1", key: "cutoff" }. */
    function splitBusKey(fullKey) {
        var bus = busOfKey(fullKey);
        if (!bus) return null;
        for (var i = 0; i < bus.keys.length; i++) {
            var p = bus.keys[i] + ":";
            if (fullKey.indexOf(p) === 0) {
                return { bus: bus, comp: bus.keys[i], key: fullKey.substring(p.length) };
            }
        }
        return null;
    }

    /** Split "fx1:cutoff" -> { comp: "fx1", key: "cutoff" }. Default comp is "synth". */
    function splitPrefix(prefixedKey) {
        for (var i = 0; i < COMPONENT_KEYS.length; i++) {
            var p = COMPONENT_KEYS[i] + ":";
            if (prefixedKey.indexOf(p) === 0) {
                return { comp: COMPONENT_KEYS[i], key: prefixedKey.substring(p.length) };
            }
        }
        return { comp: "synth", key: prefixedKey };
    }

    // ------------------------------------------------------------------
    // Slot switching
    // ------------------------------------------------------------------

    function switchSlot(n) {
        if (n === activeSlot) return;
        unsubscribe(activeSlot);
        activeSlot = n;
        waitingForData = true;
        // Persist tab in URL hash
        var hash = isBusTab(n) ? buses[n].def.hash : isNamedTab(n) ? n : "slot" + (n + 1);
        history.replaceState(null, "", "#" + hash);

        tabButtons.forEach(function (btn) {
            var slotAttr = btn.getAttribute("data-slot");
            var match = isNamedTab(slotAttr) ? (slotAttr === n) : (parseInt(slotAttr, 10) === n);
            if (match) {
                btn.classList.add("active");
                btn.setAttribute("aria-selected", "true");
            } else {
                btn.classList.remove("active");
                btn.setAttribute("aria-selected", "false");
            }
        });

        subscribe(n);
        renderSlot();
    }

    window.switchSlot = switchSlot;

    // Set initial tab button active state from URL hash
    tabButtons.forEach(function (btn) {
        var slotAttr = btn.getAttribute("data-slot");
        var match = isNamedTab(slotAttr) ? (slotAttr === activeSlot) : (parseInt(slotAttr, 10) === activeSlot);
        if (match) {
            btn.classList.add("active");
            btn.setAttribute("aria-selected", "true");
        } else {
            btn.classList.remove("active");
            btn.setAttribute("aria-selected", "false");
        }
    });

    // ------------------------------------------------------------------
    // Helpers
    // ------------------------------------------------------------------

    function escapeHtml(str) {
        var div = document.createElement("div");
        div.appendChild(document.createTextNode(str));
        return div.innerHTML;
    }

    /** Look up chain_params metadata for a param key within a component. */
    function findParamMeta(compState, key) {
        var cp = compState.chainParams;
        if (!cp) return null;
        for (var i = 0; i < cp.length; i++) {
            if (cp[i].key === key) return cp[i];
        }
        return null;
    }

    /** Get param value from component state (with prefix). */
    function getCompParamValue(compState, compPrefix, key) {
        var v = compState.params[compPrefix + ":" + key];
        if (v !== undefined) return v;
        return compState.params[key];
    }

    /** Get the current hierarchy level object for a component. */
    function getCompCurrentLevel(compState) {
        if (!compState.hierarchy || !compState.hierarchy.levels) return null;
        var stack = compState.navStack;
        var levelName = stack[stack.length - 1];
        return compState.hierarchy.levels[levelName] || null;
    }

    /** Resolve children: if a level has "children", auto-navigate. */
    function resolveCompLevel(compState) {
        if (!compState.hierarchy || !compState.hierarchy.levels) return;
        var maxDepth = 10;
        while (maxDepth-- > 0) {
            var levelName = compState.navStack[compState.navStack.length - 1];
            var level = compState.hierarchy.levels[levelName];
            if (!level || !level.children) break;
            var child = level.children;
            if (compState.hierarchy.levels[child]) {
                compState.navStack.push(child);
            } else {
                break;
            }
        }
    }

    function clampValue(val, meta) {
        if (meta.min !== undefined && val < meta.min) val = meta.min;
        if (meta.max !== undefined && val > meta.max) val = meta.max;
        return val;
    }

    /**
     * Format a param value for display — matches device overlay logic
     * (formatParamForOverlay + applyDisplayFormat in shadow_ui.js).
     */
    function formatValue(val, meta) {
        if (val === undefined || val === null) return "-";

        // Enum: show option label or option name
        if (meta && (meta.type === "enum" || meta.type === "bool")) {
            if (meta.option_labels) {
                var label = meta.option_labels[String(val)];
                if (label !== undefined) return String(label);
            }
            if (meta.options) {
                var idx = parseInt(val, 10);
                if (idx >= 0 && idx < meta.options.length) return meta.options[idx];
            }
            return String(val);
        }

        // Int: round to integer, append unit
        if (meta && meta.type === "int") {
            var rounded = Math.round(parseFloat(val));
            if (isNaN(rounded)) return String(val);
            if (meta.unit) return rounded + (meta.unit === "%" ? "%" : " " + meta.unit);
            return String(rounded);
        }

        // Float: use display_format if provided
        var num = parseFloat(val);
        if (isNaN(num)) return String(val);

        if (meta && meta.display_format) {
            var formatted = applyDisplayFormat(meta.display_format, num, meta);
            if (formatted !== null) return formatted;
        }

        // Default float: 0-1 or 0-N (N<=4) range → percentage
        var min = (meta && typeof meta.min === "number") ? meta.min : 0;
        var max = (meta && typeof meta.max === "number") ? meta.max : 1;
        if (min === 0 && max >= 1 && max <= 4) {
            return Math.round(num * 100) + "%";
        }

        // Fallback: 2 decimal places + unit
        var result = num.toFixed(2);
        if (meta && meta.unit) return result + (meta.unit === "%" ? "%" : " " + meta.unit);
        return result;
    }

    /** Apply printf-style display_format — matches device applyDisplayFormat(). */
    function applyDisplayFormat(fmt, num, meta) {
        if (!fmt) return null;
        var match = fmt.match(/^%?\.?(\d+)(f|%)$/);
        if (!match) return null;
        var decimals = parseInt(match[1], 10);

        var displayVal = num;
        // Scale 0-1 to 0-100 for "%" unit (matching C-side behavior)
        if (meta && meta.unit === "%" && typeof meta.max === "number" && meta.max <= 1) {
            displayVal = num * 100.0;
        }
        if (match[2] === "%") {
            return (num * 100).toFixed(decimals) + "%";
        }
        var formatted = displayVal.toFixed(decimals);
        if (meta && meta.unit) return formatted + (meta.unit === "%" ? "%" : " " + meta.unit);
        return formatted;
    }

    // ------------------------------------------------------------------
    // SVG Knob
    // ------------------------------------------------------------------

    var KNOB_SIZE = 60;
    var KNOB_RADIUS = 24;
    var ARC_START_DEG = 225;
    var ARC_END_DEG = 135;
    var ARC_SWEEP_DEG = 270;

    function degToRad(d) { return d * Math.PI / 180; }

    function polarToXY(cx, cy, r, deg) {
        var rad = degToRad(deg - 90);
        return { x: cx + r * Math.cos(rad), y: cy + r * Math.sin(rad) };
    }

    function describeArc(cx, cy, r, startDeg, sweepDeg) {
        var endDeg = startDeg + sweepDeg;
        var start = polarToXY(cx, cy, r, startDeg);
        var end = polarToXY(cx, cy, r, endDeg);
        var largeArc = sweepDeg > 180 ? 1 : 0;
        return "M " + start.x + " " + start.y +
               " A " + r + " " + r + " 0 " + largeArc + " 1 " + end.x + " " + end.y;
    }

    function valueToNorm(val, meta) {
        var min = (meta && meta.min !== undefined) ? meta.min : 0;
        var max = (meta && meta.max !== undefined) ? meta.max : 1;
        if (max === min) return 0;
        var t = (val - min) / (max - min);
        return Math.max(0, Math.min(1, t));
    }

    function normToAngle(t) {
        return ARC_START_DEG + t * ARC_SWEEP_DEG;
    }

    function createKnobSVG(key, meta, value) {
        var cx = KNOB_SIZE / 2;
        var cy = KNOB_SIZE / 2;
        var numVal = parseFloat(value);
        if (isNaN(numVal)) numVal = meta ? (meta.min || 0) : 0;

        var t = valueToNorm(numVal, meta);
        var indicatorAngle = normToAngle(t);
        var indicator = polarToXY(cx, cy, KNOB_RADIUS - 4, indicatorAngle);

        var bgPath = describeArc(cx, cy, KNOB_RADIUS, ARC_START_DEG, ARC_SWEEP_DEG);

        var valSweep = t * ARC_SWEEP_DEG;
        var valPath = "";
        if (valSweep > 1) {
            valPath = describeArc(cx, cy, KNOB_RADIUS, ARC_START_DEG, valSweep);
        }

        var svg = '<svg class="knob-svg" data-raw-value="' + numVal + '" width="' + KNOB_SIZE + '" height="' + KNOB_SIZE + '" viewBox="0 0 ' + KNOB_SIZE + ' ' + KNOB_SIZE + '">';
        svg += '<path d="' + bgPath + '" fill="none" stroke="#444" stroke-width="4" stroke-linecap="round"/>';
        if (valPath) {
            svg += '<path class="knob-value-arc" d="' + valPath + '" fill="none" stroke="#e8a84c" stroke-width="4" stroke-linecap="round"/>';
        }
        svg += '<circle cx="' + cx + '" cy="' + cy + '" r="3" fill="#888"/>';
        svg += '<line class="knob-indicator" x1="' + cx + '" y1="' + cy + '" x2="' + indicator.x + '" y2="' + indicator.y + '" stroke="#e8a84c" stroke-width="2" stroke-linecap="round"/>';
        svg += '</svg>';
        return svg;
    }

    function renderKnob(compPrefix, compState, key, meta, value) {
        var container = document.createElement("div");
        container.className = "knob-container";
        container.setAttribute("data-param-key", compPrefix + ":" + key);

        var label = (meta && meta.name) ? meta.name : key;
        var displayVal = formatValue(value !== undefined ? value : (meta ? (meta.min || 0) : 0), meta);

        container.innerHTML =
            createKnobSVG(key, meta, value !== undefined ? value : (meta ? (meta.min || 0) : 0)) +
            '<div class="knob-label">' + escapeHtml(label) + '</div>' +
            '<div class="knob-value" data-knob-value="' + escapeHtml(compPrefix + ":" + key) + '">' + escapeHtml(displayVal) + '</div>';

        // Drag handling
        function onPointerDown(e) {
            e.preventDefault();
            var numVal = parseFloat(getCompParamValue(compState, compPrefix, key));
            if (isNaN(numVal)) numVal = meta ? (meta.min || 0) : 0;
            var clientY = e.touches ? e.touches[0].clientY : e.clientY;
            var dragMin = 0, dragMax = 1, dragStep = 0.01;
            if (meta) {
                dragMin = meta.min !== undefined ? meta.min : 0;
                if (meta.type === "enum" && meta.options) {
                    dragMax = meta.options.length - 1;
                    dragStep = 1;
                } else {
                    dragMax = meta.max !== undefined ? meta.max : 1;
                    dragStep = meta.step || (meta.type === "int" ? 1 : 0.01);
                }
            }
            dragging = {
                component: compPrefix,
                key: key,
                startY: clientY,
                startValue: numVal,
                min: dragMin,
                max: dragMax,
                step: dragStep,
                type: meta ? meta.type : "float",
                slot: activeSlot
            };
            document.addEventListener("mousemove", onPointerMove);
            document.addEventListener("mouseup", onPointerUp);
            document.addEventListener("touchmove", onPointerMove, { passive: false });
            document.addEventListener("touchend", onPointerUp);
        }

        container.addEventListener("mousedown", onPointerDown);
        container.addEventListener("touchstart", onPointerDown, { passive: false });
        var pinBtn = makePinButton(compPrefix, key, meta);
        if (pinBtn) container.appendChild(pinBtn);

        return container;
    }

    function onPointerMove(e) {
        if (!dragging) return;
        e.preventDefault();
        var clientY = e.touches ? e.touches[0].clientY : e.clientY;
        var dy = dragging.startY - clientY;
        var range = dragging.max - dragging.min;
        var sensitivity = range / 150;
        var newVal = dragging.startValue + dy * sensitivity;

        if (dragging.step > 0) {
            newVal = Math.round(newVal / dragging.step) * dragging.step;
        }
        newVal = clampValue(newVal, dragging);
        if (dragging.type === "int" || dragging.type === "enum") newVal = Math.round(newVal);

        var prefixedKey = dragging.component + ":" + dragging.key;
        var comp = getComponentForDrag(dragging.slot, dragging.component);
        if (comp) comp.params[prefixedKey] = String(newVal);

        updateKnobVisual(prefixedKey, newVal);

        if (!sendThrottleTimer) {
            sendThrottleTimer = setTimeout(function () {
                sendThrottleTimer = null;
            }, SEND_INTERVAL);
            sendParamValue(dragging.slot, dragging.component, dragging.key, newVal);
        }
    }

    function onPointerUp(e) {
        if (!dragging) return;
        var prefixedKey = dragging.component + ":" + dragging.key;
        var comp = getComponentForDrag(dragging.slot, dragging.component);
        var finalVal = comp ? comp.params[prefixedKey] : "0";
        sendParamValue(dragging.slot, dragging.component, dragging.key, parseFloat(finalVal));
        dragging = null;
        document.removeEventListener("mousemove", onPointerMove);
        document.removeEventListener("mouseup", onPointerUp);
        document.removeEventListener("touchmove", onPointerMove);
        document.removeEventListener("touchend", onPointerUp);
    }

    /** Helper to get component state for a given slot and component key. */
    function getComponentForDrag(slot, compKey) {
        if (isBusTab(slot)) {
            return buses[slot].components[compKey] || null;
        }
        return slots[slot] ? slots[slot].components[compKey] : null;
    }

    /** Update SVG knob in-place by modifying attributes directly (no DOM rebuild). */
    function updateKnobSVGInPlace(svgEl, value, meta) {
        var cx = KNOB_SIZE / 2;
        var cy = KNOB_SIZE / 2;
        var numVal = parseFloat(value);
        if (isNaN(numVal)) numVal = meta ? (meta.min || 0) : 0;

        // Store raw value so animations can read it back without parsing display text
        svgEl.setAttribute("data-raw-value", numVal);

        var t = valueToNorm(numVal, meta);
        var indicatorAngle = normToAngle(t);
        var indicator = polarToXY(cx, cy, KNOB_RADIUS - 4, indicatorAngle);

        // Update value arc
        var valArc = svgEl.querySelector(".knob-value-arc");
        var valSweep = t * ARC_SWEEP_DEG;
        if (valSweep > 1) {
            var valPath = describeArc(cx, cy, KNOB_RADIUS, ARC_START_DEG, valSweep);
            if (valArc) {
                valArc.setAttribute("d", valPath);
            } else {
                // Create value arc if it didn't exist (was at 0)
                valArc = document.createElementNS("http://www.w3.org/2000/svg", "path");
                valArc.setAttribute("class", "knob-value-arc");
                valArc.setAttribute("fill", "none");
                valArc.setAttribute("stroke", "#e8a84c");
                valArc.setAttribute("stroke-width", "4");
                valArc.setAttribute("stroke-linecap", "round");
                valArc.setAttribute("d", valPath);
                // Insert after bg arc (first child)
                var bgArc = svgEl.querySelector("path");
                if (bgArc && bgArc.nextSibling) {
                    svgEl.insertBefore(valArc, bgArc.nextSibling);
                } else {
                    svgEl.appendChild(valArc);
                }
            }
        } else if (valArc) {
            valArc.removeAttribute("d");
        }

        // Update indicator line
        var line = svgEl.querySelector(".knob-indicator");
        if (line) {
            line.setAttribute("x2", indicator.x);
            line.setAttribute("y2", indicator.y);
        }
    }

    // Active knob animations: prefixedKey -> { from, to, start, duration, raf }
    var knobAnimations = {};
    var KNOB_ANIM_DURATION = 80; // ms — smooth but fast

    /** Immediate knob update (used during user drag). */
    function updateKnobVisual(prefixedKey, value) {
        // Cancel any running animation for this knob
        if (knobAnimations[prefixedKey]) {
            cancelAnimationFrame(knobAnimations[prefixedKey].raf);
            delete knobAnimations[prefixedKey];
        }
        updateKnobVisualDirect(prefixedKey, value);
    }

    // Track last update time per knob to detect rapid streaming
    var knobLastUpdateTime = {};

    /** Animated knob update (used for incoming device changes). */
    function updateKnobVisualAnimated(prefixedKey, targetValue) {
        var container = slotContentEl.querySelector('[data-param-key="' + prefixedKey + '"]');
        if (!container) return;

        var meta = getKnobMeta(prefixedKey);
        if (!meta) { updateKnobVisualDirect(prefixedKey, targetValue); return; }

        var svgEl = container.querySelector(".knob-svg");
        if (!svgEl) { updateKnobVisualDirect(prefixedKey, targetValue); return; }

        // During rapid hardware knob turning (updates < 100ms apart),
        // skip animation and set directly for instant response.
        var now = performance.now();
        var lastTime = knobLastUpdateTime[prefixedKey] || 0;
        knobLastUpdateTime[prefixedKey] = now;
        if (now - lastTime < 100) {
            // Cancel any running animation
            if (knobAnimations[prefixedKey]) {
                cancelAnimationFrame(knobAnimations[prefixedKey].raf);
                delete knobAnimations[prefixedKey];
            }
            updateKnobVisualDirect(prefixedKey, targetValue);
            return;
        }

        // Get current raw value from SVG data attribute
        var currentVal = parseFloat(svgEl.getAttribute("data-raw-value") || "0");
        if (isNaN(currentVal)) currentVal = meta.min || 0;
        var targetVal = parseFloat(targetValue);
        if (isNaN(targetVal)) targetVal = meta.min || 0;

        // Skip animation if change is tiny
        var range = (meta.max || 1) - (meta.min || 0);
        if (range > 0 && Math.abs(targetVal - currentVal) / range < 0.005) {
            updateKnobVisualDirect(prefixedKey, targetValue);
            return;
        }

        // Cancel existing animation
        if (knobAnimations[prefixedKey]) {
            cancelAnimationFrame(knobAnimations[prefixedKey].raf);
        }

        var valEl = container.querySelector('[data-knob-value="' + prefixedKey + '"]');

        var anim = { from: currentVal, to: targetVal, start: performance.now(), duration: KNOB_ANIM_DURATION };
        knobAnimations[prefixedKey] = anim;

        function step(now) {
            var elapsed = now - anim.start;
            var t = Math.min(1, elapsed / anim.duration);
            // Ease out cubic
            var ease = 1 - Math.pow(1 - t, 3);
            var val = anim.from + (anim.to - anim.from) * ease;

            updateKnobSVGInPlace(svgEl, val, meta);
            if (valEl) valEl.textContent = formatValue(val, meta);

            if (t < 1) {
                anim.raf = requestAnimationFrame(step);
            } else {
                delete knobAnimations[prefixedKey];
            }
        }
        anim.raf = requestAnimationFrame(step);
    }

    function getKnobMeta(prefixedKey) {
        var compState;
        var parts;
        if (isBusTab(activeSlot)) {
            parts = splitBusKey(prefixedKey);
            if (parts) compState = parts.bus.components[parts.comp];
        }
        if (!compState) {
            parts = splitPrefix(prefixedKey);
            compState = slots[typeof activeSlot === "number" ? activeSlot : 0].components[parts.comp];
        }
        if (!compState || !parts) return null;
        return findParamMeta(compState, parts.key);
    }

    function updateKnobVisualDirect(prefixedKey, value) {
        var container = slotContentEl.querySelector('[data-param-key="' + prefixedKey + '"]');
        if (!container) return;

        var meta = getKnobMeta(prefixedKey);
        var svgEl = container.querySelector(".knob-svg");
        if (svgEl) {
            updateKnobSVGInPlace(svgEl, value, meta);
        }
        var valEl = container.querySelector('[data-knob-value="' + prefixedKey + '"]');
        if (valEl) {
            valEl.textContent = formatValue(value, meta);
        }
    }

    // ------------------------------------------------------------------
    // Send param
    // ------------------------------------------------------------------

    function sendParamValue(slot, compPrefix, key, value) {
        if (isBusTab(slot)) {
            // Every bus's keys carry their own prefix; one message for all.
            send({
                type: "set_master_fx_param",
                key: compPrefix + ":" + key,
                value: String(value)
            });
        } else {
            send({
                type: "set_param",
                slot: slot,
                key: compPrefix + ":" + key,
                value: String(value)
            });
        }
    }

    // ------------------------------------------------------------------
    // Preset browser
    // ------------------------------------------------------------------

    function renderPresetBrowser(level, compPrefix, compState) {
        if (!level.list_param || !level.count_param) return null;

        var countVal = getCompParamValue(compState, compPrefix, level.count_param);
        var currentVal = getCompParamValue(compState, compPrefix, level.list_param);
        var nameVal = level.name_param ? getCompParamValue(compState, compPrefix, level.name_param) : null;

        var count = parseInt(countVal, 10) || 0;
        var current = parseInt(currentVal, 10) || 0;

        var container = document.createElement("div");
        container.className = "preset-browser";
        container.setAttribute("data-preset-browser", compPrefix);

        function markPresetLoading() {
            compState.pendingPresetChange = true;
            var sec = slotContentEl.querySelector('.component-section[data-component="' + compPrefix + '"]');
            if (sec) sec.classList.add('loading-preset');
        }

        var prevBtn = document.createElement("button");
        prevBtn.className = "preset-nav-btn";
        prevBtn.textContent = "\u25C0";
        prevBtn.disabled = current <= 0;
        prevBtn.onclick = function () {
            if (current > 0) {
                sendParamValue(activeSlot, compPrefix, level.list_param, current - 1);
                markPresetLoading();
            }
        };

        var nextBtn = document.createElement("button");
        nextBtn.className = "preset-nav-btn";
        nextBtn.textContent = "\u25B6";
        nextBtn.disabled = current >= count - 1;
        nextBtn.onclick = function () {
            if (current < count - 1) {
                sendParamValue(activeSlot, compPrefix, level.list_param, current + 1);
                markPresetLoading();
            }
        };

        var nameSpan = document.createElement("span");
        nameSpan.className = "preset-name";
        nameSpan.setAttribute("data-preset-name", compPrefix);
        var displayName = nameVal || ("Preset " + current);
        nameSpan.textContent = (current + 1) + "/" + count + "  " + displayName;

        container.appendChild(prevBtn);
        container.appendChild(nameSpan);
        container.appendChild(nextBtn);
        return container;
    }

    // ------------------------------------------------------------------
    // Param list items
    // ------------------------------------------------------------------

    function renderParamItem(entry, compPrefix, compState) {
        var key = null;
        var label = null;
        var navLevel = null;

        if (typeof entry === "string") {
            key = entry;
        } else if (entry && entry.level) {
            navLevel = entry.level;
            label = entry.label || entry.level;
        } else if (entry && entry.key) {
            key = entry.key;
            label = entry.label;
        }

        // Navigation link
        if (navLevel) {
            var row = document.createElement("div");
            row.className = "param-row param-nav-link";
            row.setAttribute("data-nav-level", navLevel);
            row.innerHTML = '<span class="param-nav-label">' + escapeHtml(label) + '</span>' +
                            '<span class="param-nav-arrow">\u203A</span>';
            row.onclick = function () {
                compState.navStack.push(navLevel);
                resolveCompLevel(compState);
                renderSlot();
            };
            return row;
        }

        // Editable param
        if (!key) return null;
        var meta = findParamMeta(compState, key);
        var value = getCompParamValue(compState, compPrefix, key);
        if (!label && meta) label = meta.name;
        if (!label) label = key;

        var row = document.createElement("div");
        row.className = "param-row";
        row.setAttribute("data-param-row", compPrefix + ":" + key);

        var labelSpan = document.createElement("span");
        labelSpan.className = "param-label";
        labelSpan.textContent = label;
        row.appendChild(labelSpan);

        var controlWrap = document.createElement("div");
        controlWrap.className = "param-control";

        if (!meta) {
            var valSpan = document.createElement("span");
            valSpan.className = "param-value-text";
            valSpan.setAttribute("data-param-value", compPrefix + ":" + key);
            valSpan.textContent = value !== undefined ? String(value) : "?";
            controlWrap.appendChild(valSpan);
        } else if (meta.type === "enum" && meta.options) {
            var select = document.createElement("select");
            select.className = "param-select";
            select.setAttribute("data-param-input", compPrefix + ":" + key);
            for (var i = 0; i < meta.options.length; i++) {
                var opt = document.createElement("option");
                opt.value = String(i);
                opt.textContent = meta.options[i];
                if (String(i) === String(value)) opt.selected = true;
                select.appendChild(opt);
            }
            select.onchange = function () {
                var v = select.value;
                compState.params[compPrefix + ":" + key] = v;
                sendParamValue(activeSlot, compPrefix, key, parseInt(v, 10));
            };
            controlWrap.appendChild(select);
            var choicePin = makePinButton(compPrefix, key, meta);
            if (choicePin) controlWrap.appendChild(choicePin);
        } else if (meta.type === "float" || meta.type === "int") {
            var slider = document.createElement("input");
            slider.type = "range";
            slider.className = "param-slider";
            slider.setAttribute("data-param-input", compPrefix + ":" + key);
            slider.min = meta.min !== undefined ? meta.min : 0;
            slider.max = meta.max !== undefined ? meta.max : (meta.type === "int" ? 127 : 1);
            slider.step = meta.step || (meta.type === "int" ? 1 : 0.01);
            slider.value = value !== undefined ? value : slider.min;

            var valDisplay = document.createElement("span");
            valDisplay.className = "param-slider-value";
            valDisplay.setAttribute("data-param-value", compPrefix + ":" + key);
            valDisplay.textContent = formatValue(value !== undefined ? value : slider.min, meta);

            (function(capturedKey, capturedComp, capturedMeta) {
                slider.oninput = function () {
                    var v = capturedMeta.type === "int" ? parseInt(slider.value, 10) : parseFloat(slider.value);
                    compState.params[capturedComp + ":" + capturedKey] = String(v);
                    valDisplay.textContent = formatValue(v, capturedMeta);
                    if (!sendThrottleTimer) {
                        sendThrottleTimer = setTimeout(function () {
                            sendThrottleTimer = null;
                        }, SEND_INTERVAL);
                        sendParamValue(activeSlot, capturedComp, capturedKey, v);
                    }
                };
                slider.onchange = function () {
                    var v = capturedMeta.type === "int" ? parseInt(slider.value, 10) : parseFloat(slider.value);
                    sendParamValue(activeSlot, capturedComp, capturedKey, v);
                };
            })(key, compPrefix, meta);

            controlWrap.appendChild(slider);
            controlWrap.appendChild(valDisplay);
            var rowPin = makePinButton(compPrefix, key, meta);
            if (rowPin) controlWrap.appendChild(rowPin);
        } else {
            var valSpan2 = document.createElement("span");
            valSpan2.className = "param-value-text";
            valSpan2.setAttribute("data-param-value", compPrefix + ":" + key);
            valSpan2.textContent = value !== undefined ? String(value) : "?";
            controlWrap.appendChild(valSpan2);
        }

        row.appendChild(controlWrap);
        return row;
    }

    // ------------------------------------------------------------------
    // Breadcrumb (per-component)
    // ------------------------------------------------------------------

    function renderBreadcrumb(compState, compPrefix, containerEl) {
        containerEl.innerHTML = "";
        if (!compState.hierarchy || !compState.hierarchy.levels) return;

        var stack = compState.navStack;
        for (var i = 0; i < stack.length; i++) {
            if (i > 0) {
                var sep = document.createElement("span");
                sep.className = "breadcrumb-sep";
                sep.textContent = " > ";
                containerEl.appendChild(sep);
            }

            var levelName = stack[i];
            var levelObj = compState.hierarchy.levels[levelName];
            var label = (levelObj && levelObj.label) ? levelObj.label : levelName;

            if (i < stack.length - 1) {
                var link = document.createElement("a");
                link.textContent = label;
                link.setAttribute("data-nav-index", String(i));
                link.onclick = (function (idx, cs) {
                    return function () {
                        cs.navStack = cs.navStack.slice(0, idx + 1);
                        renderSlot();
                    };
                })(i, compState);
                containerEl.appendChild(link);
            } else {
                var span = document.createElement("span");
                span.className = "breadcrumb-current";
                span.textContent = label;
                containerEl.appendChild(span);
            }
        }
    }

    // ------------------------------------------------------------------
    // Incremental param value updates (avoid full re-render)
    // ------------------------------------------------------------------

    function updateParamValues(slot, changedParams) {
        if (slot !== activeSlot) return;
        if (!changedParams) { renderSlot(); return; }

        var needsFullRender = false;

        for (var prefixedKey in changedParams) {
            var value = changedParams[prefixedKey];
            var parts = splitPrefix(prefixedKey);
            var compState = slots[slot].components[parts.comp];
            if (!compState) continue;

            // Skip if user is dragging this knob
            if (dragging && dragging.component === parts.comp && dragging.key === parts.key && dragging.slot === slot) continue;

            // Try to update knob visual (animated for smooth transitions)
            var knobContainer = slotContentEl.querySelector('[data-param-key="' + prefixedKey + '"]');
            if (knobContainer) {
                updateKnobVisualAnimated(prefixedKey, parseFloat(value));
            }

            // Try to update param row control
            var paramRow = slotContentEl.querySelector('[data-param-row="' + prefixedKey + '"]');
            if (paramRow) {
                var input = paramRow.querySelector('[data-param-input="' + prefixedKey + '"]');
                if (input) {
                    if (input.tagName === "SELECT") {
                        input.value = String(value);
                    } else if (input.type === "range") {
                        input.value = value;
                    }
                }
                var valDisplay = paramRow.querySelector('[data-param-value="' + prefixedKey + '"]');
                if (valDisplay) {
                    var meta = findParamMeta(compState, parts.key);
                    valDisplay.textContent = formatValue(value, meta);
                }
            }

            // Update preset browser if relevant — check all navStack levels
            // since the preset browser may render from an ancestor (e.g. root).
            var presetBrowser = slotContentEl.querySelector('[data-preset-browser="' + parts.comp + '"]');
            if (presetBrowser && compState.hierarchy && compState.hierarchy.levels) {
                for (var psi = 0; psi < compState.navStack.length; psi++) {
                    var psLevel = compState.hierarchy.levels[compState.navStack[psi]];
                    if (psLevel && (parts.key === psLevel.list_param ||
                                    parts.key === psLevel.name_param ||
                                    parts.key === psLevel.count_param)) {
                        needsFullRender = true;
                        break;
                    }
                }
            }

            // Update mapped knob name — triggers re-render if section not yet in DOM
            var knobNameMatch = prefixedKey.match(/knob_(\d+)_name$/);
            if (knobNameMatch) {
                var knobNameIdx = knobNameMatch[1];
                var knobNameContainer = slotContentEl.querySelector('[data-mapped-knob="' + knobNameIdx + '"]');
                if (!knobNameContainer) {
                    needsFullRender = true;
                } else {
                    var knobLabelEl = knobNameContainer.querySelector('.knob-label');
                    if (knobLabelEl) knobLabelEl.textContent = value;
                }
            }

            // Update mapped knob value — text and SVG arc
            var knobMatch = prefixedKey.match(/knob_(\d+)_value$/);
            if (knobMatch) {
                var knobValIdx = knobMatch[1];
                var knobValContainer = slotContentEl.querySelector('[data-mapped-knob="' + knobValIdx + '"]');
                if (knobValContainer) {
                    var knobValEl = knobValContainer.querySelector('[data-mapped-knob-value="' + knobValIdx + '"]');
                    if (knobValEl) knobValEl.textContent = value || "-";
                    var numVal = parseFloat(value);
                    if (!isNaN(numVal)) {
                        var norm;
                        if (String(value).indexOf("%") >= 0) norm = Math.max(0, Math.min(1, numVal / 100));
                        else if (numVal >= 0 && numVal <= 1) norm = numVal;
                        else if (numVal >= 0 && numVal <= 127) norm = numVal / 127;
                        else norm = 0.5;
                        var svgEl = knobValContainer.querySelector(".knob-svg");
                        if (svgEl) updateKnobSVGInPlace(svgEl, norm, { min: 0, max: 1, type: "float" });
                    }
                }
            }

            // Update slot settings / LFO params inline
            var settingRow = slotContentEl.querySelector('[data-setting-row="' + prefixedKey + '"]');
            if (settingRow) {
                var sInput = settingRow.querySelector('[data-setting-input="' + prefixedKey + '"]');
                if (sInput) sInput.value = value;
                var sMeta = findSettingMeta(prefixedKey);
                var sVal = settingRow.querySelector('[data-setting-value="' + prefixedKey + '"]');
                if (sVal) {
                    if (sMeta && sMeta.format) sVal.textContent = sMeta.format(value);
                    else sVal.textContent = value || "—";
                }
                var sToggle = settingRow.querySelector('.setting-toggle');
                if (sToggle) {
                    sToggle.textContent = value === "1" ? "On" : "Off";
                    sToggle.className = "setting-toggle" + (value === "1" ? " active" : "");
                }
                // If this is the sync field, the rate_hz/rate_div visibility depends on it.
                if (/lfo\d:sync$/.test(prefixedKey)) {
                    needsFullRender = true;
                }
            }
        }

        if (needsFullRender) renderSlot();
    }

    /**
     * Is this component's section folded?
     *
     * `state.collapsed[key]` records only what the USER chose -- a key is
     * absent until the header is clicked. Everything else is DERIVED, because
     * the right default depends on something that arrives after the state is
     * built: a component that ships its own web_ui.html must open, or the
     * panel is invisible and nothing on screen says one exists. That is
     * exactly how a MIDI FX panel went missing -- the section rendered, folded,
     * as a single line under a 3000px synth panel. The lead position opens too,
     * so a slot is never a stack of closed triangles.
     */
    function sectionCollapsed(state, compKey, leadKey) {
        var chosen = state.collapsed[compKey];
        if (chosen === true || chosen === false) return chosen;
        if (state.customUI && state.customUI[compKey]) return false;
        return compKey !== leadKey;
    }

    // ------------------------------------------------------------------
    // Render a single component section
    // ------------------------------------------------------------------

    function renderComponentSection(compKey, compState, isCollapsed) {
        var section = document.createElement("div");
        section.className = "component-section" + (compState.pendingPresetChange ? " loading-preset" : "");
        section.setAttribute("data-component", compKey);

        // Header bar
        var header = document.createElement("div");
        header.className = "component-header" + (isCollapsed ? " collapsed" : "");
        var arrow = document.createElement("span");
        arrow.className = "component-toggle";
        arrow.textContent = isCollapsed ? "\u25B6" : "\u25BC";
        var title = document.createElement("span");
        title.className = "component-title";
        var displayLabel = COMPONENT_LABELS[compKey] || compKey;
        var moduleName = compState.module || "";
        title.textContent = displayLabel + (moduleName ? " - " + moduleName : "");
        header.appendChild(arrow);
        header.appendChild(title);
        header.onclick = function () {
            var st = slots[activeSlot];
            st.collapsed[compKey] = !sectionCollapsed(st, compKey, "synth");
            renderSlot();
        };
        section.appendChild(header);

        if (isCollapsed) return section;

        // A component that ships its own web_ui.html draws it here instead of
        // the generated rows. Honour the slot's Default override so the
        // generated controls stay reachable.
        var slotState = slots[activeSlot];
        var compCustomUrl = (typeof activeSlot === "number" && slotState)
            ? (slotState.customUI[compKey] || "") : "";
        if (compCustomUrl && compKey !== "synth" && !slotState.useDefaultUI) {
            var popOut = makePopOutButton(customUIUrlFor(compCustomUrl, compKey), activeSlot);
            popOut.classList.add("component-popout");
            popOut.onclick = (function (orig) {
                return function (ev) { ev.stopPropagation(); return orig.apply(this, arguments); };
            })(popOut.onclick);
            header.appendChild(popOut);
            section.appendChild(
                buildCustomUIFrame(compKey, compCustomUrl,
                                   (COMPONENT_LABELS[compKey] || compKey) + " UI"));
            return section;
        }

        // Body
        var body = document.createElement("div");
        body.className = "component-body";

        // Resolve auto-navigation
        resolveCompLevel(compState);

        // Breadcrumb
        if (compState.hierarchy && compState.hierarchy.levels && compState.navStack.length > 1) {
            var bcEl = document.createElement("nav");
            bcEl.className = "slot-breadcrumb";
            renderBreadcrumb(compState, compKey, bcEl);
            body.appendChild(bcEl);
        }

        var level = getCompCurrentLevel(compState);
        if (!level) {
            var noLevel = document.createElement("p");
            noLevel.className = "text-muted";
            noLevel.textContent = "No parameters available";
            body.appendChild(noLevel);
            section.appendChild(body);
            return section;
        }

        // Preset browser — render from the first navStack level that has one,
        // so modules that put their preset selector on "root" (auto-nav parent
        // via children) still show a preset picker at the current level.
        var presetLevel = null;
        if (level.list_param && level.count_param) {
            presetLevel = level;
        } else if (compState.hierarchy && compState.hierarchy.levels) {
            for (var pi = 0; pi < compState.navStack.length; pi++) {
                var ancestor = compState.hierarchy.levels[compState.navStack[pi]];
                if (ancestor && ancestor.list_param && ancestor.count_param) {
                    presetLevel = ancestor;
                    break;
                }
            }
        }
        if (presetLevel) {
            var presetEl = renderPresetBrowser(presetLevel, compKey, compState);
            if (presetEl) body.appendChild(presetEl);
        }

        // Knob row
        var knobs = level.knobs || [];
        if (knobs.length > 0) {
            var knobRow = document.createElement("div");
            knobRow.className = "knob-row";
            for (var i = 0; i < knobs.length && i < 8; i++) {
                var knobKey = knobs[i];
                var meta = findParamMeta(compState, knobKey);
                var value = getCompParamValue(compState, compKey, knobKey);
                var knobEl = renderKnob(compKey, compState, knobKey, meta, value);
                knobRow.appendChild(knobEl);
            }
            body.appendChild(knobRow);
        }

        // Param list
        var params = level.params || [];
        if (params.length > 0) {
            var paramList = document.createElement("div");
            paramList.className = "param-list";
            for (var j = 0; j < params.length; j++) {
                var item = renderParamItem(params[j], compKey, compState);
                if (item) paramList.appendChild(item);
            }
            body.appendChild(paramList);
        }

        // Empty state
        if (knobs.length === 0 && params.length === 0 && !level.list_param) {
            var empty = document.createElement("p");
            empty.className = "text-muted";
            empty.textContent = "No parameters on this level";
            body.appendChild(empty);
        }

        section.appendChild(body);
        return section;
    }

    // ------------------------------------------------------------------
    // Rendering
    // ------------------------------------------------------------------

    function renderSlot() {
        slotContentEl.innerHTML = "";
        debugEl.innerHTML = "";
        if (slotHeaderControlsEl) slotHeaderControlsEl.innerHTML = "";
        renderSlotName();

        // Clear custom UI iframe state on re-render.
        // The DOM is about to be rebuilt, so every registered frame is stale.
        for (var fk in customUIFrames) {
            if (customUIFrames[fk] && customUIFrames[fk].timer) clearTimeout(customUIFrames[fk].timer);
        }
        customUIFrames = {};

        if (isBusTab(activeSlot)) {
            renderBus(buses[activeSlot]);
            return;
        }

        if (activeSlot === "tool") {
            renderTool();
            return;
        }

        if (activeSlot === "mixer") {
            slotTitleEl.textContent = "Mixer";
            if (mixer) {
                mixer.render(slotContentEl);
            } else {
                debugEl.innerHTML = '<p class="text-muted">The mixer script did not load. Reload the page.</p>';
            }
            return;
        }

        var s = slots[activeSlot];
        slotTitleEl.textContent = "Slot " + (activeSlot + 1);

        // Check if any component has data
        var hasAnyData = false;
        var hasAnyModule = false;
        for (var i = 0; i < COMPONENT_KEYS.length; i++) {
            var comp = s.components[COMPONENT_KEYS[i]];
            if (comp.module) hasAnyModule = true;
            if (comp.hierarchy || comp.chainParams || Object.keys(comp.params).length > 0) {
                hasAnyData = true;
            }
        }

        // Before any data arrives, show a loading spinner.
        if (!hasAnyData && !hasAnyModule && (waitingForData || !isConnected)) {
            debugEl.innerHTML = '<div class="loading-state"><span class="loading-spinner"></span> Loading slot...</div>';
            return;
        }

        // Two separate questions. The Interface toggle is slot-wide — it flips
        // every panel in the slot, which is what renderComponentSection already
        // assumes by reading useDefaultUI — so it has to appear whenever ANY
        // component has a panel. Gating it on the synth left an FX-only slot
        // with no way back to the generated controls.
        var synthCustomUrl = s.customUI.synth || "";
        var hasAnyCustomUI = hasAnyModule && COMPONENT_KEYS.some(function (k) {
            return s.customUI[k];
        });
        if (hasAnyCustomUI && slotHeaderControlsEl) {
            slotHeaderControlsEl.appendChild(renderUiModeToggle(s));
            // Pop-out stays synth-only; an FX section carries its own.
            if (synthCustomUrl) {
                slotHeaderControlsEl.appendChild(
                    makePopOutButton(customUIUrlFor(synthCustomUrl, "synth"), activeSlot));
            }
        }

        // Only a synth panel takes over the slot view.
        if (synthCustomUrl && !s.useDefaultUI) {
            renderCustomUI(s);
            return;
        }

        // Render mapped knobs at the top (hardware knob assignments)
        var knobSection = renderMappedKnobs(s);
        if (knobSection) slotContentEl.appendChild(knobSection);

        // Render component sections in order: midi_fx, synth, fx1, fx2
        var renderedCount = 0;
        for (var k = 0; k < COMPONENT_ORDER.length; k++) {
            var compKey = COMPONENT_ORDER[k];
            var compState = s.components[compKey];
            if (!compState || !compState.module) continue;

            var section = renderComponentSection(
                compKey, compState, sectionCollapsed(s, compKey, "synth"));
            slotContentEl.appendChild(section);
            renderedCount++;
        }

        // If no modules are loaded, note it — but still fall through to render
        // slot settings (volume, channels, LFOs) since those work independently
        // of modules and users may want to configure them first.
        if (renderedCount === 0 && !knobSection) {
            var emptyNote = document.createElement("p");
            emptyNote.className = "text-muted";
            emptyNote.textContent = "No modules loaded in this slot";
            slotContentEl.appendChild(emptyNote);
        }

        // Render slot settings (including LFO) at the bottom
        var settingsSection = renderSlotSettings(s);
        if (settingsSection) slotContentEl.appendChild(settingsSection);
    }

    // ------------------------------------------------------------------
    // Slot name, and renaming the slot's preset
    //
    // A slot's name IS its preset's name, so Rename does what saving under an
    // edited name does on Move: the preset file is rewritten in place (every
    // set using it sees the new name), and an unsaved slot is saved as a new
    // preset. The Move does it -- only it can assemble the slot's patch -- and
    // answers with rename_result; the name itself comes back as slot:name.
    // ------------------------------------------------------------------

    // One rename UI at a time, for the slot it was opened on.
    var rename = { slot: -1, editing: false, pending: false, error: "", draft: "" };
    var RENAME_MAX = 63;

    function slotNameOf(slot) {
        var p = slots[slot] && slots[slot].components.synth.params;
        var v = p ? p["slot:name"] : undefined;
        return (v === undefined) ? null : String(v);   // null: not heard yet
    }

    // force: the rename state itself changed. Anything else that redraws the
    // slot (a hierarchy arriving, a name echo) leaves an open form alone, so
    // the text being typed keeps its focus and caret.
    function renderSlotName(force) {
        if (!slotNameEl) return;
        var slot = activeSlot;
        var editing = typeof slot === "number" && rename.editing && rename.slot === slot;
        if (editing && !force) {
            var open = slotNameEl.querySelector("form");
            if (open && open.getAttribute("data-slot") === String(slot)) return;
        }
        slotNameEl.textContent = "";
        if (typeof slot !== "number") return;
        var name = slotNameOf(slot);

        if (!editing) {
            var label = document.createElement("span");
            label.className = "slot-name-text" + (name ? "" : " text-muted");
            label.textContent = name === null ? "—" : (name || "Unsaved");
            if (!name && name !== null) label.title = "This slot hasn't been saved as a preset yet.";
            slotNameEl.appendChild(label);

            var btn = document.createElement("button");
            btn.type = "button";
            btn.className = "btn btn-secondary slot-name-btn";
            btn.textContent = name ? "Rename" : "Save as preset";
            btn.setAttribute("aria-label", (name ? "Rename slot " : "Name and save slot ") + (slot + 1));
            btn.disabled = name === null || !isConnected;
            btn.onclick = function () {
                rename = { slot: slot, editing: true, pending: false, error: "", draft: name || "" };
                renderSlotName(true);
                var input = slotNameEl.querySelector("input");
                if (input) { input.focus(); input.select(); }
            };
            slotNameEl.appendChild(btn);
            return;
        }

        var form = document.createElement("form");
        form.className = "slot-name-form";
        form.setAttribute("data-slot", String(slot));
        var inputId = "slot-name-input";
        var lab = document.createElement("label");
        lab.className = "sr-only";
        lab.setAttribute("for", inputId);
        lab.textContent = "Preset name for slot " + (slot + 1);
        var input = document.createElement("input");
        input.type = "text";
        input.id = inputId;
        input.className = "slot-name-input";
        input.maxLength = RENAME_MAX;
        input.autocomplete = "off";
        input.spellcheck = false;
        input.value = rename.draft;
        input.disabled = rename.pending;
        input.oninput = function () { rename.draft = input.value; };

        var save = document.createElement("button");
        save.type = "submit";
        save.className = "btn btn-primary slot-name-btn";
        save.textContent = rename.pending ? "Saving…" : "Save";
        save.disabled = rename.pending;
        var cancel = document.createElement("button");
        cancel.type = "button";
        cancel.className = "btn btn-secondary slot-name-btn";
        cancel.textContent = "Cancel";
        cancel.disabled = rename.pending;
        cancel.onclick = function () { rename.editing = false; renderSlotName(true); };
        input.onkeydown = function (e) {
            if (e.key === "Escape" && !rename.pending) { e.preventDefault(); cancel.onclick(); }
        };

        var status = document.createElement("span");
        status.className = "slot-name-status" + (rename.error ? " error" : "");
        status.setAttribute("role", rename.error ? "alert" : "status");
        status.textContent = rename.error || (rename.pending ? "Saving on Move…" : "");

        form.onsubmit = function (e) {
            e.preventDefault();
            var v = input.value.trim();
            var why = slotNameProblem(v);
            if (why) { rename.error = why; renderSlotName(true); return; }
            if (v === name) { rename.editing = false; renderSlotName(true); return; }
            rename.pending = true;
            rename.error = "";
            rename.draft = v;
            send({ type: "rename_slot", slot: slot, value: v });
            renderSlotName(true);
        };

        form.appendChild(lab);
        form.appendChild(input);
        form.appendChild(save);
        form.appendChild(cancel);
        form.appendChild(status);
        slotNameEl.appendChild(form);
    }

    // The same rules the manager and the Move apply, so the answer is immediate.
    function slotNameProblem(v) {
        if (!v) return "The name is empty.";
        if (v.length > RENAME_MAX) return "The name is too long (" + RENAME_MAX + " characters at most).";
        if (/["\\\u0000-\u001f\u007f]/.test(v)) return "The name can't contain quotes, backslashes or control characters.";
        if (v === "Untitled") return "\"Untitled\" is the name of an unsaved slot.";
        return "";
    }

    function handleRenameResult(msg) {
        if (msg.slot !== rename.slot || !rename.pending) return;
        rename.pending = false;
        if (msg.ok) {
            rename.editing = false;
            rename.error = "";
            // The name also arrives as slot:name; show it now rather than wait.
            if (slots[msg.slot]) slots[msg.slot].components.synth.params["slot:name"] = msg.name;
        } else {
            rename.error = msg.error || "Move refused the rename.";
        }
        if (activeSlot === msg.slot) renderSlotName(true);
    }

    // ------------------------------------------------------------------
    // Pinning a parameter to the Mixer
    //
    // A numeric knob on a Slot tab can be pinned to that slot's Mixer strip.
    // Pins belong to the Set and the server owns them: the button sends
    // pin_mixer and shows what mixer_pins says, never its own guess.
    // ------------------------------------------------------------------

    var PIN_SVG = '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true" focusable="false">' +
        '<path d="M9.5 1.5l5 5-1.4 1.4-1-1-2.6 2.6.4 3.4-1.4 1.4-2.8-2.8-3.2 3.2-.7-.7 3.2-3.2-2.8-2.8 1.4-1.4 3.4.4 2.6-2.6-1-1z" fill="currentColor"/></svg>';

    // The pins of whatever tab is showing: a slot's, or an FX bus's.
    function activePins() {
        if (isBusTab(activeSlot)) return buses[activeSlot].pins || [];
        if (typeof activeSlot === "number") return slots[activeSlot].pins || [];
        return [];
    }

    function makePinButton(comp, key, meta) {
        if (typeof activeSlot !== "number" && !isBusTab(activeSlot)) return null; // slots and FX buses
        if (!meta) return null;
        var numeric = (meta.type === "float" || meta.type === "int" || !meta.type) &&
            typeof meta.min === "number" && typeof meta.max === "number" && meta.max > meta.min;
        var choice = meta.type === "enum" && meta.options && meta.options.length >= 2;
        if (!numeric && !choice) return null;
        var full = comp + ":" + key;
        var name = (meta && meta.name) || key;
        var b = document.createElement("button");
        b.type = "button";
        b.className = "pin-btn";
        b.setAttribute("data-pin-key", full);
        b.setAttribute("data-pin-name", name);
        b.innerHTML = PIN_SVG;
        // Never start a knob drag from the pin.
        b.addEventListener("mousedown", function (e) { e.stopPropagation(); });
        b.addEventListener("touchstart", function (e) { e.stopPropagation(); }, { passive: true });
        b.onclick = function (e) {
            e.stopPropagation();
            var pinned = activePins().indexOf(full) >= 0;
            var m = { type: "pin_mixer", key: full, value: pinned ? "0" : "1" };
            // A bus key names its own strip; a slot's needs the slot.
            if (typeof activeSlot === "number") m.slot = activeSlot;
            send(m);
        };
        paintPinButton(b, activePins().indexOf(full) >= 0);
        return b;
    }

    function paintPinButton(b, pinned) {
        var name = b.getAttribute("data-pin-name");
        b.setAttribute("aria-pressed", pinned ? "true" : "false");
        b.setAttribute("aria-label", (pinned ? "Unpin " : "Pin ") + name + (pinned ? " from the Mixer" : " to the Mixer"));
        b.title = pinned ? "Pinned to the Mixer \u2014 click to unpin" : "Pin to the Mixer";
        b.classList.toggle("pinned", pinned);
    }

    function refreshPinButtons() {
        if (typeof activeSlot !== "number" && !isBusTab(activeSlot)) return;
        var pins = activePins();
        var btns = slotContentEl.querySelectorAll("[data-pin-key]");
        for (var i = 0; i < btns.length; i++) {
            paintPinButton(btns[i], pins.indexOf(btns[i].getAttribute("data-pin-key")) >= 0);
        }
    }

    // A refused pin (four per slot, a parameter that cannot be a fader) is
    // said out loud and shown briefly; nothing else changes.
    var pinNotice = null;
    function announcePin(text) {
        if (!pinNotice) {
            pinNotice = document.createElement("div");
            pinNotice.className = "pin-notice";
            pinNotice.setAttribute("role", "status");
            document.body.appendChild(pinNotice);
        }
        pinNotice.textContent = text.charAt(0).toUpperCase() + text.slice(1);
        pinNotice.classList.add("show");
        clearTimeout(announcePin.timer);
        announcePin.timer = setTimeout(function () { pinNotice.classList.remove("show"); }, 4000);
    }

    // ------------------------------------------------------------------
    // Mapped Knobs section
    // ------------------------------------------------------------------

    // Knobs 1-8, as REAL knobs on the parameters they are mapped to: the range
    // comes from that component's chain_params, the value from its params, and
    // a drag writes the parameter like any other control here (the chain then
    // moves the hardware knob's position to match). Turning the knob on the
    // Move arrives as an ordinary param_update on the same key.
    function renderMappedKnobs(s) {
        var map = s.knobMap || [];
        if (!map.length) return null;

        var section = document.createElement("div");
        section.className = "mapped-knobs-section";
        section.setAttribute("role", "group");
        section.setAttribute("aria-label", "Knobs 1 to 8");

        var grid = document.createElement("div");
        grid.className = "knob-grid";
        for (var k = 0; k < map.length; k++) {
            var m = map[k];
            var compState = s.components[m.comp];
            var meta = compState ? findParamMeta(compState, m.param) : null;
            var tile;
            if (compState && meta && (meta.type === "float" || meta.type === "int" || !meta.type) &&
                typeof meta.min === "number" && typeof meta.max === "number") {
                tile = renderKnob(m.comp, compState, m.param, meta, getCompParamValue(compState, m.comp, m.param));
            } else {
                // Not drawable as a knob (yet): its component's metadata has not
                // arrived, or it is not a numeric range. Say what it is.
                tile = document.createElement("div");
                tile.className = "knob-container mapped-knob-pending";
                var v = compState ? getCompParamValue(compState, m.comp, m.param) : undefined;
                tile.innerHTML = '<div class="knob-label">' + escapeHtml(meta && meta.name ? meta.name : m.param) + '</div>' +
                    '<div class="knob-value">' + escapeHtml(v !== undefined ? formatValue(v, meta) : "\u2014") + '</div>';
            }
            tile.setAttribute("data-mapped-knob", String(m.knob));
            var where = COMPONENT_LABELS[m.comp] || m.comp;
            tile.title = "Knob " + m.knob + " \u00b7 " + where + " \u00b7 " + (meta && meta.name ? meta.name : m.param);
            var badge = document.createElement("div");
            badge.className = "mapped-knob-number";
            badge.textContent = "Knob " + m.knob;
            tile.insertBefore(badge, tile.firstChild);
            grid.appendChild(tile);
        }
        section.appendChild(grid);
        return section;
    }

    // ------------------------------------------------------------------
    // Slot Settings section
    // ------------------------------------------------------------------

    var LFO_SHAPES = ["Sine", "Tri", "Saw", "Square", "S&H", "Swishy"];
    var LFO_DIVISIONS = [
        "16bar", "15bar", "14bar", "13bar", "12bar", "11bar", "10bar", "9bar",
        "8bar", "7bar", "6bar", "5bar", "4bar", "3bar", "2bar",
        "1/1", "1/1T", "1/2", "1/2T", "1/4", "1/4T", "1/8", "1/8T",
        "1/16", "1/16T", "1/32", "1/32T"
    ];

    var SLOT_SETTINGS = [
        { key: "slot:volume", label: "Volume", type: "float", min: 0, max: 4, step: 0.05,
          format: function(v) { var n = parseFloat(v); if (isNaN(n)) return "—"; return Math.round(n * 100) + "%"; } },
        { key: "slot:muted", label: "Muted", type: "bool" },
        { key: "slot:soloed", label: "Soloed", type: "bool" },
        { key: "slot:receive_channel", label: "Receive Channel", type: "enum_int",
          options: (function() {
              var opts = [{ val: "0", label: "All" }];
              for (var i = 1; i <= 16; i++) opts.push({ val: String(i), label: String(i) });
              return opts;
          })() },
        { key: "slot:forward_channel", label: "Forward Channel", type: "enum_int",
          options: (function() {
              var opts = [
                  { val: "-2", label: "THRU" },
                  { val: "-1", label: "Auto" }
              ];
              for (var i = 0; i <= 15; i++) opts.push({ val: String(i), label: String(i + 1) });
              return opts;
          })() }
    ];

    function lfoParams(n) {
        var pfx = "lfo" + n + ":";
        return [
            { key: pfx + "shape", label: "Shape", type: "enum_int",
              options: LFO_SHAPES.map(function(name, i) { return { val: String(i), label: name }; }) },
            { key: pfx + "polarity", label: "Mode", type: "enum_int",
              options: [{ val: "0", label: "Unipolar" }, { val: "1", label: "Bipolar" }] },
            { key: pfx + "sync", label: "Sync", type: "enum_int",
              options: [{ val: "0", label: "Free" }, { val: "1", label: "Sync" }] },
            { key: pfx + "rate_hz", label: "Rate", type: "float", min: 0.1, max: 20.0, step: 0.1,
              format: function(v) { var n = parseFloat(v); if (isNaN(n)) return "—"; return n.toFixed(1) + " Hz"; },
              showWhen: pfx + "sync", showEqual: "0" },
            { key: pfx + "rate_div", label: "Rate Div", type: "enum_int",
              options: LFO_DIVISIONS.map(function(name, i) { return { val: String(i), label: name }; }),
              showWhen: pfx + "sync", showEqual: "1" },
            { key: pfx + "depth", label: "Depth", type: "float", min: -1, max: 1, step: 0.01,
              format: function(v) { var n = parseFloat(v); if (isNaN(n)) return "—"; return Math.round(n * 100) + "%"; } },
            { key: pfx + "target", label: "Target", type: "lfo_target" },
            { key: pfx + "target_param", label: "Param", type: "lfo_target_param", targetKey: pfx + "target" }
        ];
    }

    var LFO_SETTINGS = [
        { id: "lfo1", label: "LFO 1", enableKey: "lfo1:enabled", params: lfoParams(1) },
        { id: "lfo2", label: "LFO 2", enableKey: "lfo2:enabled", params: lfoParams(2) }
    ];

    var lfoCollapsed = { lfo1: true, lfo2: true };

    // findSettingMeta returns the metadata entry for a slot-level or LFO param
    // key, used by incremental updateParamValues to format and reconcile values.
    function findSettingMeta(key) {
        for (var i = 0; i < SLOT_SETTINGS.length; i++) {
            if (SLOT_SETTINGS[i].key === key) return SLOT_SETTINGS[i];
        }
        for (var li = 0; li < LFO_SETTINGS.length; li++) {
            var ps = LFO_SETTINGS[li].params;
            for (var pi = 0; pi < ps.length; pi++) {
                if (ps[pi].key === key) return ps[pi];
            }
            if (LFO_SETTINGS[li].enableKey === key) return { type: "bool" };
        }
        return null;
    }

    // readSlotParam reads a slot-level param from synthParams, checking both
    // prefixed ("synth:key") and unprefixed ("key") forms.
    function readSlotParam(synthParams, key) {
        var v = synthParams["synth:" + key];
        if (v === undefined || v === "") v = synthParams[key];
        return v === undefined ? "" : v;
    }

    // renderSettingRow renders a single slot/LFO param row with full editing
    // support for all the types used in SLOT_SETTINGS and LFO_SETTINGS.
    // Returns null if the setting is gated by showWhen and should be hidden.
    function renderSettingRow(setting, synthParams, slotState, extraClass) {
        if (setting.showWhen) {
            var guard = readSlotParam(synthParams, setting.showWhen);
            if (String(guard) !== String(setting.showEqual)) return null;
        }

        var rawVal = readSlotParam(synthParams, setting.key);

        var row = document.createElement("div");
        row.className = "param-row" + (extraClass ? " " + extraClass : "");
        row.setAttribute("data-setting-row", setting.key);

        var label = document.createElement("span");
        label.className = "param-label";
        label.textContent = setting.label;
        row.appendChild(label);

        if (setting.type === "bool") {
            var btn = document.createElement("button");
            btn.className = "setting-toggle" + (rawVal === "1" ? " active" : "");
            btn.textContent = rawVal === "1" ? "On" : "Off";
            btn.onclick = (function(key) {
                return function(e) {
                    if (e) e.stopPropagation();
                    var isOn = this.classList.contains("active");
                    send({ type: "set_param", slot: activeSlot, key: key, value: isOn ? "0" : "1" });
                };
            })(setting.key);
            row.appendChild(btn);

        } else if (setting.type === "float") {
            var input = document.createElement("input");
            input.type = "range";
            input.min = setting.min;
            input.max = setting.max;
            input.step = setting.step;
            input.value = (rawVal === "" ? setting.min : rawVal);
            input.setAttribute("data-setting-input", setting.key);
            input.oninput = (function(key, fmt) {
                return function(e) {
                    send({ type: "set_param", slot: activeSlot, key: key, value: e.target.value });
                    var disp = e.target.parentElement.querySelector('[data-setting-value]');
                    if (disp) disp.textContent = fmt ? fmt(e.target.value) : e.target.value;
                };
            })(setting.key, setting.format);
            row.appendChild(input);

            var valDisp = document.createElement("span");
            valDisp.className = "param-value";
            valDisp.setAttribute("data-setting-value", setting.key);
            valDisp.textContent = setting.format ? setting.format(rawVal) : (rawVal || "—");
            row.appendChild(valDisp);

        } else if (setting.type === "enum_int") {
            var sel = document.createElement("select");
            sel.className = "lfo-select";
            sel.setAttribute("data-setting-input", setting.key);
            var opts = setting.options || [];
            var foundMatch = false;
            for (var oi = 0; oi < opts.length; oi++) {
                var opt = document.createElement("option");
                opt.value = opts[oi].val;
                opt.textContent = opts[oi].label;
                if (String(rawVal) === String(opts[oi].val)) {
                    opt.selected = true;
                    foundMatch = true;
                }
                sel.appendChild(opt);
            }
            if (!foundMatch && rawVal !== "") {
                // Keep UI in sync with current server value even if it's not
                // in our options list (don't lose state on unexpected values).
                var fallback = document.createElement("option");
                fallback.value = rawVal;
                fallback.textContent = rawVal;
                fallback.selected = true;
                sel.appendChild(fallback);
            }
            sel.onchange = (function(key) {
                return function(e) {
                    send({ type: "set_param", slot: activeSlot, key: key, value: e.target.value });
                    // If toggling sync, re-render to swap rate_hz <-> rate_div
                    if (/lfo\d:sync$/.test(key)) setTimeout(function() { renderSlot(); }, 50);
                };
            })(setting.key);
            row.appendChild(sel);

        } else if (setting.type === "lfo_target") {
            var tgtSelect = document.createElement("select");
            tgtSelect.className = "lfo-select";
            tgtSelect.setAttribute("data-setting-input", setting.key);
            var tgtOpts = [{ val: "", label: "(none)" }];
            for (var ci = 0; ci < COMPONENT_KEYS.length; ci++) {
                var ck = COMPONENT_KEYS[ci];
                if (slotState.components[ck] && slotState.components[ck].module) {
                    tgtOpts.push({ val: ck, label: ck + " (" + slotState.components[ck].module + ")" });
                }
            }
            for (var ti = 0; ti < tgtOpts.length; ti++) {
                var topt = document.createElement("option");
                topt.value = tgtOpts[ti].val;
                topt.textContent = tgtOpts[ti].label;
                if (tgtOpts[ti].val === rawVal) topt.selected = true;
                tgtSelect.appendChild(topt);
            }
            tgtSelect.onchange = (function(key) {
                return function(e) {
                    send({ type: "set_param", slot: activeSlot, key: key, value: e.target.value });
                    var paramKey = key.replace(":target", ":target_param");
                    send({ type: "set_param", slot: activeSlot, key: paramKey, value: "" });
                    setTimeout(function() { renderSlot(); }, 100);
                };
            })(setting.key);
            row.appendChild(tgtSelect);

        } else if (setting.type === "lfo_target_param") {
            var targetComp = readSlotParam(synthParams, setting.targetKey);
            var paramSelect = document.createElement("select");
            paramSelect.className = "lfo-select";
            paramSelect.setAttribute("data-setting-input", setting.key);
            var noneOpt = document.createElement("option");
            noneOpt.value = "";
            noneOpt.textContent = "(none)";
            paramSelect.appendChild(noneOpt);
            if (targetComp && slotState.components[targetComp] && slotState.components[targetComp].chainParams) {
                var cp = slotState.components[targetComp].chainParams;
                for (var cpi = 0; cpi < cp.length; cpi++) {
                    var cpItem = cp[cpi];
                    if (!cpItem || !cpItem.key) continue;
                    var cpType = (cpItem.type || "").toLowerCase();
                    if (cpType === "float" || cpType === "int" || cpType === "" ||
                        (cpItem.min !== undefined && cpItem.max !== undefined)) {
                        var pOpt = document.createElement("option");
                        pOpt.value = cpItem.key;
                        pOpt.textContent = cpItem.name || cpItem.key;
                        if (cpItem.key === rawVal) pOpt.selected = true;
                        paramSelect.appendChild(pOpt);
                    }
                }
            }
            paramSelect.onchange = (function(key) {
                return function(e) {
                    send({ type: "set_param", slot: activeSlot, key: key, value: e.target.value });
                };
            })(setting.key);
            row.appendChild(paramSelect);

        } else {
            var valSpan = document.createElement("span");
            valSpan.className = "param-value";
            valSpan.setAttribute("data-setting-value", setting.key);
            valSpan.textContent = rawVal || "—";
            row.appendChild(valSpan);
        }

        return row;
    }

    function renderSlotSettings(s) {
        var section = document.createElement("div");
        section.className = "component-section slot-settings-section";

        var header = document.createElement("div");
        header.className = "component-header";
        header.innerHTML = '<span class="component-title">Slot Settings</span>';
        section.appendChild(header);

        var synthParams = s.components.synth.params;

        for (var i = 0; i < SLOT_SETTINGS.length; i++) {
            var setting = SLOT_SETTINGS[i];
            var row = renderSettingRow(setting, synthParams, s);
            if (row) section.appendChild(row);
        }

        // Collapsible LFO sections
        for (var li = 0; li < LFO_SETTINGS.length; li++) {
            var lfo = LFO_SETTINGS[li];
            var enableVal = synthParams["synth:" + lfo.enableKey] || synthParams[lfo.enableKey] || "0";
            var isEnabled = (enableVal === "1");
            var isOpen = !lfoCollapsed[lfo.id];

            var lfoHeader = document.createElement("div");
            lfoHeader.className = "lfo-header";
            lfoHeader.setAttribute("data-lfo-id", lfo.id);

            var toggle = document.createElement("span");
            toggle.className = "component-toggle";
            toggle.textContent = isOpen ? "\u25BC" : "\u25B6";

            var lfoTitle = document.createElement("span");
            lfoTitle.className = "component-title";
            lfoTitle.textContent = lfo.label;

            var lfoStatus = document.createElement("span");
            lfoStatus.className = "lfo-status" + (isEnabled ? " active" : "");
            lfoStatus.textContent = isEnabled ? "On" : "Off";

            lfoHeader.appendChild(toggle);
            lfoHeader.appendChild(lfoTitle);
            lfoHeader.appendChild(lfoStatus);
            lfoHeader.onclick = (function(id) {
                return function() {
                    lfoCollapsed[id] = !lfoCollapsed[id];
                    renderSlot();
                };
            })(lfo.id);
            section.appendChild(lfoHeader);

            if (isOpen) {
                // Enable toggle row
                var enableRow = document.createElement("div");
                enableRow.className = "param-row lfo-param-row";
                enableRow.setAttribute("data-setting-row", lfo.enableKey);
                var enableLabel = document.createElement("span");
                enableLabel.className = "param-label";
                enableLabel.textContent = "Enabled";
                var enableBtn = document.createElement("button");
                enableBtn.className = "setting-toggle" + (isEnabled ? " active" : "");
                enableBtn.textContent = isEnabled ? "On" : "Off";
                enableBtn.onclick = (function(key) {
                    return function(e) {
                        e.stopPropagation();
                        var isOn = this.classList.contains("active");
                        send({ type: "set_param", slot: activeSlot, key: key, value: isOn ? "0" : "1" });
                    };
                })(lfo.enableKey);
                enableRow.appendChild(enableLabel);
                enableRow.appendChild(enableBtn);
                section.appendChild(enableRow);

                for (var pi = 0; pi < lfo.params.length; pi++) {
                    var row = renderSettingRow(lfo.params[pi], synthParams, s, "lfo-param-row");
                    if (row) section.appendChild(row);
                }
            }
        }

        return section;
    }

    // ------------------------------------------------------------------
    // Custom Module Web UI (iframe)
    // ------------------------------------------------------------------

    // Segmented Custom/Default switch shown when the synth has a web_ui.html.
    // Lets the user fall back to the auto-generated parameter UI for this slot
    // (session-only — resets on refresh, like the collapse state).
    function renderUiModeToggle(s) {
        var bar = document.createElement("div");
        bar.className = "ui-mode-toggle";

        var label = document.createElement("span");
        label.className = "ui-mode-label";
        label.textContent = "Interface";
        bar.appendChild(label);

        var group = document.createElement("div");
        group.className = "ui-mode-group";

        [["Custom", false], ["Default", true]].forEach(function (opt) {
            var btn = document.createElement("button");
            btn.type = "button";
            btn.className = "ui-mode-btn" + (s.useDefaultUI === opt[1] ? " active" : "");
            btn.textContent = opt[0];
            btn.setAttribute("aria-pressed", s.useDefaultUI === opt[1] ? "true" : "false");
            btn.onclick = function () {
                if (s.useDefaultUI === opt[1]) return;
                s.useDefaultUI = opt[1];
                renderSlot();
            };
            group.appendChild(btn);
        });

        bar.appendChild(group);
        return bar;
    }

    // "Pop out" button — opens the module UI as its own top-level window, free
    // of the iframe's sandbox/sizing. The standalone page talks to
    // /ws/remote-ui directly (see schwung-remote-api.js), so it keeps working
    // even if this manager tab is closed.
    function makePopOutButton(url, slot) {
        var popBtn = document.createElement("button");
        popBtn.type = "button";
        popBtn.className = "custom-ui-popout-btn";
        popBtn.textContent = "Pop out ↗";
        popBtn.title = "Open this UI in its own window";
        popBtn.onclick = function () {
            var sep = url.indexOf("?") === -1 ? "?" : "&";
            // The Tool tab is not a numbered slot — standalone mode keys off
            // tool=1 to use the overtake-tool channel (subscribe_tool / set on
            // slot 0 / refetch_tool) instead of a per-slot subscribe.
            // Master FX is not a numbered slot either: standalone mode keys
            // off master_fx=1 to use subscribe_master_fx and
            // set_master_fx_param, whose keys already carry their own
            // "master_fx:fxN:" prefix.
            // A send bus the same, with bus=<id>.
            var qs = (slot === "tool") ? "schwungStandalone=1&tool=1"
                   : (slot === "master") ? "schwungStandalone=1&master_fx=1"
                   : isBusTab(slot) ? "schwungStandalone=1&bus=" + buses[slot].def.id
                   : "schwungStandalone=1&slot=" + slot;
            var popUrl = url + sep + qs;
            // One window per COMPONENT on a bus -- keying on the slot alone
            // would make fx2's pop-out replace fx1's.
            var winKey = isBusTab(slot)
                ? "schwungPopout_" + slot + "_" + encodeURIComponent(componentOfUrl(url))
                : "schwungPopout_" + slot;
            window.open(popUrl, winKey);
        };
        return popBtn;
    }

    function renderTool() {
        slotTitleEl.textContent = tool.id || "Tool";

        if (!tool.customUI || !tool.customUI.url) {
            var note = document.createElement("p");
            note.className = "text-muted";
            note.textContent = tool.id
                ? ("Loading " + tool.id + "…")
                : (tool.known
                    ? "No tool loaded. Open a tool on the Move and it will appear here."
                    : "Connecting to the Move — checking for a loaded tool…");
            slotContentEl.appendChild(note);
            return;
        }

        // Pop-out button (own window), same as slots.
        if (slotHeaderControlsEl) {
            slotHeaderControlsEl.appendChild(makePopOutButton(tool.customUI.url, "tool"));
        }

        slotContentEl.appendChild(buildCustomUIFrame("tool", tool.customUI.url, "Tool UI"));
    }

    /** Tag a module UI URL with the component it is driving, so the page can
     *  address the right parameter prefix ("fx1:mix" rather than "synth:mix")
     *  and a popped-out window can ask for the right metadata. */
    /** The component customUIUrlFor stamped into a panel URL, for window naming. */
    function componentOfUrl(url) {
        var m = /[?&]component=([^&]*)/.exec(url || "");
        return m ? decodeURIComponent(m[1]) : "";
    }

    function customUIUrlFor(url, comp) {
        if (!url) return url;
        return url + (url.indexOf("?") >= 0 ? "&" : "?") + "component=" + encodeURIComponent(comp);
    }

    /** Build the iframe + loading overlay for one component's custom UI and
     *  register it so messages can be routed back to the right component. */
    function buildCustomUIFrame(comp, url, title) {
        var container = document.createElement("div");
        container.className = "custom-ui-container";

        var iframe = document.createElement("iframe");
        iframe.className = "custom-ui-iframe";
        iframe.src = customUIUrlFor(url, comp);
        iframe.sandbox = "allow-scripts allow-same-origin";
        iframe.setAttribute("title", title || "Custom Module UI");
        container.appendChild(iframe);

        // Cleared when the iframe subscribes (module JS ran), on load, or after
        // a timeout — whichever comes first.
        var overlay = document.createElement("div");
        overlay.className = "custom-ui-loading";
        overlay.innerHTML = '<span class="loading-spinner"></span> Loading module…';
        container.appendChild(overlay);

        var entry = { iframe: iframe, subscribed: false, overlay: overlay, timer: null };
        customUIFrames[comp] = entry;
        iframe.addEventListener("load", function () { hideCustomUILoading(comp); });
        entry.timer = setTimeout(function () { hideCustomUILoading(comp); }, 10000);
        return container;
    }

    /** Which component owns the iframe a postMessage came from. */
    function componentForSource(source) {
        for (var comp in customUIFrames) {
            var e = customUIFrames[comp];
            if (e && e.iframe && e.iframe.contentWindow === source) return comp;
        }
        return null;
    }

    function renderCustomUI(s) {
        // SIGNAL-FLOW order, the same one the generated path uses: a MIDI FX
        // runs BEFORE the synth, so its section belongs above the synth panel.
        // Rendering the synth iframe first and looping COMPONENT_KEYS after it
        // put the MIDI FX dead last -- below a panel that measures itself at
        // ~3000px, which is a long way past "not visible".
        for (var k = 0; k < COMPONENT_ORDER.length; k++) {
            var compKey = COMPONENT_ORDER[k];
            var compState = s.components[compKey];
            if (compKey === "synth") {
                // The synth's own panel replaces its section entirely.
                slotContentEl.appendChild(
                    buildCustomUIFrame("synth", s.customUI.synth, "Custom Module UI"));
                continue;
            }
            if (!compState.module) continue;
            slotContentEl.appendChild(
                renderComponentSection(
                    compKey, compState, sectionCollapsed(s, compKey, "synth")));
        }

        // Slot settings (volume, sends, channels, LFO, …) are module-independent
        // and must still render with a custom synth UI — the non-custom path
        // appends them at the bottom, so do the same here.
        var settingsSection = renderSlotSettings(s);
        if (settingsSection) slotContentEl.appendChild(settingsSection);
    }

    // Listen for postMessage from custom UI iframes.
    window.addEventListener("message", function (e) {
        // Only accept messages from a registered module iframe, and remember
        // which component it speaks for.
        var srcComp = componentForSource(e.source);
        if (!srcComp) return;

        var msg = e.data;
        if (!msg || !msg.type) return;

        switch (msg.type) {
            case "height":
                // The panel measured itself, so stop guessing at 60vh. Clamped
                // both ways: a module that mis-measures should look wrong, not
                // produce a 40000px frame or collapse to a sliver.
                var hf = customUIFrames[srcComp];
                var h = Number(msg.height);
                if (hf && hf.iframe && isFinite(h) && h > 0) {
                    hf.iframe.style.height =
                        Math.min(Math.max(Math.round(h), 200), 4000) + "px";
                }
                break;
            case "getParam":
                handleIframeGetParam(msg, srcComp);
                break;
            case "setParam":
                handleIframeSetParam(msg, srcComp);
                break;
            case "subscribe":
                customUIFrames[srcComp].subscribed = true;
                // Module JS has run — clear the loading overlay.
                hideCustomUILoading(srcComp);
                // Bulk-seed: push every param value the parent has already
                // cached (from the host's initial fetch) in one message, so the
                // iframe paints real values immediately instead of pulling them
                // back one-by-one via getParam. Late-arriving values still flow
                // through the normal param_update path below.
                seedCustomUI(srcComp);
                break;
            case "getHierarchy":
                handleIframeGetHierarchy(msg, srcComp);
                break;
            case "getChainParams":
                handleIframeGetChainParams(msg, srcComp);
                break;
            case "resubscribe":
                // Iframe asked for a fresh device read (e.g. tool polling for the
                // playhead). For the Tool tab, re-fetch params only — NOT a full
                // subscribe (that would re-send custom_ui and reload the iframe).
                if (activeSlot === "tool") {
                    send({ type: "refetch_tool" });
                } else if (isBusTab(activeSlot)) {
                    send({ type: "subscribe_bus", key: buses[activeSlot].def.id });
                } else if (typeof activeSlot === "number") {
                    send({ type: "subscribe", slot: activeSlot });
                }
                break;
        }
    });

    function handleIframeGetParam(msg, comp) {
        if (!msg.key || !msg.id) return;
        var slot = activeSlot;

        // Tool view: params come from the overtake tool's cache (overtake_dsp:*).
        if (slot === "tool") {
            postToIframe({ type: "paramResult", id: msg.id, value: tool.params[msg.key] || "" }, comp);
            return;
        }
        // FX bus view: the key is "master_fx:fx1:cutoff", which splitPrefix
        // cannot parse -- it splits on the FIRST colon and would look for a
        // component called "master_fx". Read from the bus's cache instead.
        if (isBusTab(slot)) {
            var mp = splitBusKey(msg.key);
            var mc = (mp && mp.bus === buses[slot]) ? mp.bus.components[mp.comp] : null;
            postToIframe({ type: "paramResult", id: msg.id,
                           value: mc ? (mc.params[msg.key] || "") : "" }, comp);
            return;
        }
        if (typeof slot !== "number") return;

        // The key is like "synth:cutoff" — send via WebSocket get_param
        // (not currently supported by the WS protocol, so read from local state).
        var parts = splitPrefix(msg.key);
        var compState = slots[slot].components[parts.comp];
        var value = compState ? (compState.params[msg.key] || "") : "";

        // Targeted: request ids come from a per-page counter in
        // schwung-remote-api.js ("req_<n>_<ms>"), so two panels can mint the
        // same id in the same millisecond and a broadcast reply would resolve
        // in the wrong one with another component's value.
        postToIframe({ type: "paramResult", id: msg.id, value: value }, comp);
    }

    function handleIframeSetParam(msg, comp) {   // comp unused: no reply is sent
        if (!msg.key) return;
        var slot = activeSlot;

        // Tool view: route on slot 0; the manager dispatches by the
        // "overtake_dsp:" key prefix, so the slot number is ignored.
        if (slot === "tool") {
            send({ type: "set_param", slot: 0, key: msg.key, value: String(msg.value) });
            return;
        }
        // FX bus writes go through set_master_fx_param, not set_param: the
        // key already carries its own "master_fx:fxN:" / "send1:fxN:" prefix
        // and there is no chain slot to address.
        if (isBusTab(slot)) {
            send({ type: "set_master_fx_param", key: msg.key, value: String(msg.value) });
            return;
        }
        if (typeof slot !== "number") return;

        send({
            type: "set_param",
            slot: slot,
            key: msg.key,
            value: String(msg.value)
        });
    }

    function handleIframeGetHierarchy(msg, comp) {
        var slot = activeSlot;
        if (isBusTab(slot)) {
            var mc = buses[slot].components[comp];
            postToIframe({ type: "hierarchy", id: msg.id || null, component: comp,
                           data: mc ? mc.hierarchy : null }, comp);
            return;
        }
        if (typeof slot !== "number") return;
        var compState = slots[slot].components[comp || "synth"];
        postToIframe({
            type: "hierarchy",
            id: msg.id || null,
            component: comp || "synth",
            data: compState ? compState.hierarchy : null
        }, comp);
    }

    function handleIframeGetChainParams(msg, comp) {
        var slot = activeSlot;
        if (isBusTab(slot)) {
            var mc = buses[slot].components[comp];
            postToIframe({ type: "chainParams", id: msg.id || null, component: comp,
                           data: mc ? mc.chainParams : null }, comp);
            return;
        }
        if (typeof slot !== "number") return;
        var compState = slots[slot].components[comp || "synth"];
        postToIframe({
            type: "chainParams",
            id: msg.id || null,
            component: comp || "synth",
            data: compState ? compState.chainParams : null
        }, comp);
    }

    /** Post to one component's iframe, or to every live one when comp is omitted. */
    function postToIframe(msg, comp) {
        if (comp) {
            var one = customUIFrames[comp];
            if (one && one.iframe && one.iframe.contentWindow)
                one.iframe.contentWindow.postMessage(msg, "*");
            return;
        }
        for (var k in customUIFrames) {
            var e = customUIFrames[k];
            if (e && e.subscribed && e.iframe && e.iframe.contentWindow)
                e.iframe.contentWindow.postMessage(msg, "*");
        }
    }

    // Remove one component's loading overlay (idempotent).
    function hideCustomUILoading(comp) {
        var e = customUIFrames[comp];
        if (!e) return;
        if (e.timer) { clearTimeout(e.timer); e.timer = null; }
        if (e.overlay && e.overlay.parentNode) e.overlay.parentNode.removeChild(e.overlay);
        e.overlay = null;
    }

    // Push all params the parent has already cached for the active slot to the
    // custom UI iframe in a single paramUpdate. Called when the iframe first
    // subscribes so it can seed its controls without a round-trip per param.
    function seedCustomUI(comp) {
        if (!customUIFrames[comp]) return;

        // Tool view: seed from the overtake tool's cached params.
        if (activeSlot === "tool") {
            var keys = Object.keys(tool.params);
            if (keys.length > 0) postToIframe({ type: "paramUpdate", params: tool.params }, comp);
            return;
        }

        // FX bus view: seed from that position's cached params. Without
        // this the panel opens blank and only fills in when something moves.
        if (isBusTab(activeSlot)) {
            var mc = buses[activeSlot].components[comp];
            if (mc && Object.keys(mc.params).length > 0)
                postToIframe({ type: "paramUpdate", params: mc.params }, comp);
            return;
        }

        if (typeof activeSlot !== "number") return;
        var comps = slots[activeSlot].components;
        var params = {};
        var any = false;
        for (var k = 0; k < COMPONENT_KEYS.length; k++) {
            var cp = comps[COMPONENT_KEYS[k]];
            if (!cp || !cp.params) continue;
            for (var key in cp.params) {
                if (Object.prototype.hasOwnProperty.call(cp.params, key)) {
                    params[key] = cp.params[key];
                    any = true;
                }
            }
        }
        if (any) postToIframe({ type: "paramUpdate", params: params }, comp);
    }

    function renderBus(bus) {
        var label = bus.def.label + (bus.def.tab === "master" ? "" : " FX");
        slotTitleEl.textContent = label;

        var hasAnyModule = false;
        var hasAnyData = false;
        for (var i = 0; i < bus.keys.length; i++) {
            var comp = bus.components[bus.keys[i]];
            if (comp.module) hasAnyModule = true;
            if (comp.hierarchy || comp.chainParams || Object.keys(comp.params).length > 0) {
                hasAnyData = true;
            }
        }

        var none = "No effects loaded in " + label;
        if (!hasAnyData && !hasAnyModule) {
            if (waitingForData || !isConnected) {
                debugEl.innerHTML = '<div class="loading-state"><span class="loading-spinner"></span> Loading ' +
                    escapeHtml(label) + '...</div>';
            } else {
                debugEl.innerHTML = '<p class="text-muted">' + escapeHtml(none) + '</p>';
            }
            return;
        }

        if (!hasAnyModule) {
            slotContentEl.innerHTML = '<p class="text-muted">' + escapeHtml(none) + '</p>';
            return;
        }

        var hasAnyCustomUI = bus.keys.some(function (k) {
            return bus.components[k].module && bus.customUI[k];
        });
        if (hasAnyCustomUI && slotHeaderControlsEl) {
            slotHeaderControlsEl.appendChild(renderUiModeToggle(bus));
        }

        var renderedCount = 0;
        for (var k = 0; k < bus.keys.length; k++) {
            var compKey = bus.keys[k];
            var compState = bus.components[compKey];
            if (!compState.module) continue;

            var section = renderBusSection(
                bus, compKey, compState,
                sectionCollapsed(bus, compKey, bus.keys[0]));
            slotContentEl.appendChild(section);
            renderedCount++;
        }

        if (renderedCount === 0) {
            slotContentEl.innerHTML = '<p class="text-muted">' + escapeHtml(none) + '</p>';
        }
    }

    // ------------------------------------------------------------------
    // FX bus section rendering
    // ------------------------------------------------------------------

    function renderBusSection(bus, compKey, compState, isCollapsed) {
        var section = document.createElement("div");
        section.className = "component-section";
        section.setAttribute("data-component", compKey);

        // Header bar
        var header = document.createElement("div");
        header.className = "component-header" + (isCollapsed ? " collapsed" : "");
        var arrow = document.createElement("span");
        arrow.className = "component-toggle";
        arrow.textContent = isCollapsed ? "\u25B6" : "\u25BC";
        var title = document.createElement("span");
        title.className = "component-title";
        var displayLabel = bus.labels[compKey] || compKey;
        var moduleName = compState.module || "";
        title.textContent = displayLabel + (moduleName ? " - " + moduleName : "");
        header.appendChild(arrow);
        header.appendChild(title);
        header.onclick = function () {
            bus.collapsed[compKey] =
                !sectionCollapsed(bus, compKey, bus.keys[0]);
            renderSlot();
        };
        section.appendChild(header);

        if (isCollapsed) return section;

        // A Master FX module that ships its own web_ui.html draws it here
        // instead of the generated rows -- the same treatment an audio FX gets
        // in a chain slot, which is the whole point of #354. The Interface
        // override still wins, so the generated controls stay reachable.
        var mfxCustomUrl = bus.customUI[compKey] || "";
        if (mfxCustomUrl && !bus.useDefaultUI) {
            var popOut = makePopOutButton(customUIUrlFor(mfxCustomUrl, compKey), bus.def.tab);
            popOut.classList.add("component-popout");
            popOut.onclick = (function (orig) {
                return function (ev) { ev.stopPropagation(); return orig.apply(this, arguments); };
            })(popOut.onclick);
            header.appendChild(popOut);
            section.appendChild(
                buildCustomUIFrame(compKey, mfxCustomUrl,
                                   (bus.labels[compKey] || compKey) + " UI"));
            return section;
        }

        // Body
        var body = document.createElement("div");
        body.className = "component-body";

        // Resolve auto-navigation
        resolveCompLevel(compState);

        // Breadcrumb
        if (compState.hierarchy && compState.hierarchy.levels && compState.navStack.length > 1) {
            var bcEl = document.createElement("nav");
            bcEl.className = "slot-breadcrumb";
            renderBreadcrumb(compState, compKey, bcEl);
            body.appendChild(bcEl);
        }

        var level = getCompCurrentLevel(compState);
        if (!level) {
            var noLevel = document.createElement("p");
            noLevel.className = "text-muted";
            noLevel.textContent = "No parameters available";
            body.appendChild(noLevel);
            section.appendChild(body);
            return section;
        }

        // Preset browser — render from the first navStack level that has one,
        // so modules that put their preset selector on "root" (auto-nav parent
        // via children) still show a preset picker at the current level.
        var presetLevel = null;
        if (level.list_param && level.count_param) {
            presetLevel = level;
        } else if (compState.hierarchy && compState.hierarchy.levels) {
            for (var pi = 0; pi < compState.navStack.length; pi++) {
                var ancestor = compState.hierarchy.levels[compState.navStack[pi]];
                if (ancestor && ancestor.list_param && ancestor.count_param) {
                    presetLevel = ancestor;
                    break;
                }
            }
        }
        if (presetLevel) {
            var presetEl = renderPresetBrowser(presetLevel, compKey, compState);
            if (presetEl) body.appendChild(presetEl);
        }

        // Knob row
        var knobs = level.knobs || [];
        if (knobs.length > 0) {
            var knobRow = document.createElement("div");
            knobRow.className = "knob-row";
            for (var i = 0; i < knobs.length && i < 8; i++) {
                var knobKey = knobs[i];
                var meta = findParamMeta(compState, knobKey);
                var value = getBusParamValue(compState, compKey, knobKey);
                var knobEl = renderKnob(compKey, compState, knobKey, meta, value);
                knobRow.appendChild(knobEl);
            }
            body.appendChild(knobRow);
        }

        // Param list
        var params = level.params || [];
        if (params.length > 0) {
            var paramList = document.createElement("div");
            paramList.className = "param-list";
            for (var j = 0; j < params.length; j++) {
                var item = renderParamItem(params[j], compKey, compState);
                if (item) paramList.appendChild(item);
            }
            body.appendChild(paramList);
        }

        // Empty state
        if (knobs.length === 0 && params.length === 0 && !level.list_param) {
            var empty = document.createElement("p");
            empty.className = "text-muted";
            empty.textContent = "No parameters on this level";
            body.appendChild(empty);
        }

        section.appendChild(body);
        return section;
    }

    /** Get param value from an FX bus component state. */
    function getBusParamValue(compState, compPrefix, key) {
        var v = compState.params[compPrefix + ":" + key];
        if (v !== undefined) return v;
        return compState.params[key];
    }

    function updateBusParamValues(changedParams) {
        if (!isBusTab(activeSlot)) return;
        if (!changedParams) { renderSlot(); return; }

        var needsFullRender = false;

        for (var fullKey in changedParams) {
            var value = changedParams[fullKey];
            var parts = splitBusKey(fullKey);
            if (!parts || parts.bus !== buses[activeSlot]) continue;
            var compState = parts.bus.components[parts.comp];
            if (!compState) continue;

            // Skip if user is dragging this knob
            if (dragging && dragging.component === parts.comp && dragging.key === parts.key && dragging.slot === activeSlot) continue;

            // Try to update knob visual (animated for smooth transitions)
            var knobContainer = slotContentEl.querySelector('[data-param-key="' + fullKey + '"]');
            if (knobContainer) {
                updateKnobVisualAnimated(fullKey, parseFloat(value));
            }

            // Try to update param row control
            var paramRow = slotContentEl.querySelector('[data-param-row="' + fullKey + '"]');
            if (paramRow) {
                var input = paramRow.querySelector('[data-param-input="' + fullKey + '"]');
                if (input) {
                    if (input.tagName === "SELECT") {
                        input.value = String(value);
                    } else if (input.type === "range") {
                        input.value = value;
                    }
                }
                var valDisplay = paramRow.querySelector('[data-param-value="' + fullKey + '"]');
                if (valDisplay) {
                    var meta = findParamMeta(compState, parts.key);
                    valDisplay.textContent = formatValue(value, meta);
                }
            }

            // Update preset browser if relevant
            var presetBrowser = slotContentEl.querySelector('[data-preset-browser="' + parts.comp + '"]');
            if (presetBrowser) {
                var level = getCompCurrentLevel(compState);
                if (level && (parts.key === level.list_param ||
                              parts.key === level.name_param ||
                              parts.key === level.count_param)) {
                    needsFullRender = true;
                }
            }
        }

        if (needsFullRender) renderSlot();
    }

    // ------------------------------------------------------------------
    // Status indicator
    // ------------------------------------------------------------------

    function setStatus(connected) {
        if (!statusEl) return;
        if (connected) {
            statusEl.innerHTML = '<span class="status-dot connected"></span> Connected';
            statusEl.className = "remote-ui-status connected";
        } else {
            var label = "Reconnecting...";
            if (reconnectAttempts > 0) {
                label = "Reconnecting... (attempt " + reconnectAttempts + ")";
            }
            statusEl.innerHTML = '<span class="status-dot disconnected"></span> ' + label;
            statusEl.className = "remote-ui-status disconnected";
        }
    }

    // ------------------------------------------------------------------
    // Init
    // ------------------------------------------------------------------

    connect();
})();

"use strict";
// All dynamic content goes through textContent / createTextNode; never innerHTML.
// The CSP forbids inline styles: set `style` through the property (el's `style` prop) or el.style.setProperty,
// never setAttribute("style", ...).

const root = document.getElementById("root");
const toastBox = document.getElementById("toast");

// Enter that confirms an IME candidate (zhuyin, pinyin) must not submit; 229 covers Safari, which ends composition before keydown.
const isEnter = (e) => e.key === "Enter" && !e.isComposing && e.keyCode !== 229;

const SVG_NS = "http://www.w3.org/2000/svg";
// 24x24 stroke icons (paths only), drawn in currentColor.
const circle = (cx, cy, r) => `M${cx - r} ${cy}a${r} ${r} 0 1 0 ${2 * r} 0a${r} ${r} 0 1 0 ${-2 * r} 0`;
const ICONS = {
  dashboard: ["M3 11l9-8 9 8", "M5 10v10h14V10", "M10 20v-6h4v6"],
  folder: ["M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
  bulb: ["M9 18h6", "M10 21h4", "M12 3a6 6 0 0 0-4 10.5c.7.7 1 1.5 1 2.5h6c0-1 .3-1.8 1-2.5A6 6 0 0 0 12 3z"],
  key: [circle(8, 15, 4), "M10.8 12.2L20 3", "M16 7l3 3", "M14 9l2 2"],
  users: ["M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2", circle(9, 7, 4), "M22 21v-2a4 4 0 0 0-3-3.9", "M16 3.1a4 4 0 0 1 0 7.8"],
  settings: ["M4 6h6", "M14 6h6", "M4 12h12", "M20 12h0", "M4 18h2", "M10 18h10", circle(12, 6, 2), circle(18, 12, 2), circle(8, 18, 2)],
  logout: ["M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4", "M16 17l5-5-5-5", "M21 12H9"],
  plus: ["M12 5v14", "M5 12h14"],
  alert: ["M12 3l10 18H2z", "M12 10v5", "M12 18h0"],
  lock: ["M5 11h14v10H5z", "M8 11V7a4 4 0 0 1 8 0v4"],
  up: ["M12 19V5", "M5 12l7-7 7 7"],
  down: ["M12 5v14", "M19 12l-7 7-7-7"],
  play: ["M7 4l13 8-13 8z"],
  chevron: ["M6 9l6 6 6-6"],
  copy: ["M9 9h11v11H9z", "M5 15H4V4h11v1"],
  more: ["M5 12h0", "M12 12h0", "M19 12h0"],
};

function icon(name) {
  const svg = document.createElementNS(SVG_NS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": "2",
    "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", class: "ico",
  })) svg.setAttribute(k, v);
  for (const d of ICONS[name]) {
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", d);
    svg.append(path);
  }
  return svg;
}

// The mark: a house with one lit window.
function mark() {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  const house = document.createElementNS(SVG_NS, "path");
  house.setAttribute("d", "M3 11l9-8 9 8v10H3z");
  for (const [k, v] of Object.entries({ fill: "none", stroke: "currentColor", "stroke-width": "1.8", "stroke-linejoin": "round" })) house.setAttribute(k, v);
  const win = document.createElementNS(SVG_NS, "rect");
  for (const [k, v] of Object.entries({ x: "9.5", y: "11.5", width: "5", height: "5", rx: "0.6", fill: "var(--glow)" })) win.setAttribute(k, v);
  svg.append(house, win);
  return svg;
}

// Properties the DOM does not reflect under the same name; everything with a dash (aria-*, data-*) is an attribute too.
const ATTRS = new Set(["role", "for"]);
// `style` is deliberately a property (el.style = "..." is CSSOM, which the CSP allows); setAttribute("style") is not.
function el(tag, props, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === null || v === undefined) continue;
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (k.includes("-") || ATTRS.has(k)) { if (v !== false) e.setAttribute(k, v === true ? "" : v); }
    else e[k] = v;
  }
  for (const c of children.flat(Infinity)) if (c !== null && c !== undefined && c !== false && c !== "") e.append(c);
  return e;
}

let idCounter = 0;
const uid = () => "u" + (++idCounter);

/** A labelled form control with an optional hint and an error slot; `setError` marks it invalid. */
function field(labelText, input, opts = {}) {
  const id = input.id || (input.id = uid());
  const hint = opts.hint ? el("span", { class: "field-hint", id: id + "-hint" }, opts.hint) : null;
  const err = el("span", { class: "field-err", id: id + "-err", role: "alert" });
  input.setAttribute("aria-describedby", (hint ? id + "-hint " : "") + id + "-err");
  const box = el("div", { class: "field" + (opts.cls ? " " + opts.cls : "") }, el("label", { for: id }, labelText), input, hint, err);
  box.setError = (message) => { err.textContent = message || ""; input.setAttribute("aria-invalid", String(!!message)); };
  return box;
}

function checkbox(labelNodes, props) {
  const box = el("input", { type: "checkbox", ...props });
  return [el("label", { class: "check" }, box, el("span", {}, labelNodes)), box];
}

/** A row of toggle buttons that picks one value. */
function seg({ label: text, options, current, onpick, key }) {
  return el("div", { class: "seg", role: "group", "aria-label": text },
    options.map(([value, name]) => el("button", {
      type: "button", class: "btn", "aria-pressed": String(current === value), "data-k": key + ":" + value, onclick: () => onpick(value),
    }, name)));
}

// ---------------------------------------------------------------- dialogs

/** Shows a modal <dialog>; build(close, titleId) returns its content, and the promise resolves to what close() got (null when dismissed). */
function openDialog(build, cls) {
  return new Promise((resolve) => {
    const titleId = uid();
    const dlg = el("dialog", { class: cls || "", "aria-labelledby": titleId });
    let result = null;
    const close = (value) => { result = value; dlg.close(); };
    dlg.addEventListener("close", () => { dlg.remove(); resolve(result); });
    dlg.addEventListener("click", (e) => { if (e.target === dlg) close(null); });
    dlg.append(build(close, titleId));
    document.body.append(dlg);
    dlg.showModal();
  });
}

/**
 * In-page password dialog. A real form with a username field, unlike prompt(), so password
 * managers can fill it, and WebViews that drop prompt() still work. Resolves to null on cancel.
 */
function askPassword({ title, hint, username, fresh }) {
  return openDialog((close, titleId) => {
    const pw = el("input", { type: "password", autocomplete: fresh ? "new-password" : "current-password", autofocus: true });
    const pwField = field(fresh ? "New password" : "Password", pw, { hint: fresh ? "At least 12 characters." : null });
    return el("form", {
      class: "dialog-body", method: "dialog",
      onsubmit: (e) => {
        e.preventDefault();
        if (!pw.value) return pwField.setError("Enter the password.");
        if (fresh && pw.value.length < 12) return pwField.setError("Use at least 12 characters.");
        close(pw.value);
      },
    },
      el("h2", { id: titleId }, title),
      hint ? el("p", { class: "muted" }, hint) : "",
      el("input", { type: "text", value: username || "", autocomplete: "username", readOnly: true, tabIndex: -1, class: "sr-only", "aria-hidden": "true" }),
      pwField,
      el("div", { class: "row" },
        el("button", { type: "button", class: "btn", onclick: () => close(null) }, "Cancel"),
        el("button", { type: "submit", class: "btn btn-primary" }, "Continue")));
  });
}

/** Asks before something destructive. Resolves to true when confirmed; Cancel has the focus. */
async function confirmDialog({ title, body, action }) {
  const answer = await openDialog((close, titleId) => el("form", { class: "dialog-body", onsubmit: (e) => { e.preventDefault(); close(true); } },
    el("h2", { id: titleId }, title),
    body ? el("p", { class: "muted" }, body) : "",
    el("div", { class: "row" },
      el("button", { type: "button", class: "btn", autofocus: true, onclick: () => close(null) }, "Cancel"),
      el("button", { type: "submit", class: "btn btn-danger" }, action))));
  return answer === true;
}

/** Lets an admin tick the groups a user is limited to. Resolves to the picked names (none = no limit), or null when dismissed. */
function pickGroupsDialog(user) {
  return openDialog((close, titleId) => {
    const boxes = data.groups.map((g) => {
      const [labelNode, box] = checkbox(g.name, { checked: (user.scope || []).includes(g.name) });
      return { labelNode, box, name: g.name };
    });
    return el("form", {
      class: "dialog-body",
      onsubmit: (e) => { e.preventDefault(); close(boxes.filter((b) => b.box.checked).map((b) => b.name)); },
    },
      el("h2", { id: titleId }, "Limit " + user.username + " to groups"),
      el("p", { class: "muted" }, "Leave everything unchecked for no limit."),
      boxes.length ? boxes.map((b) => b.labelNode) : el("p", {}, "There are no groups yet."),
      el("div", { class: "row" },
        el("button", { type: "button", class: "btn", onclick: () => close(null) }, "Cancel"),
        el("button", { type: "submit", class: "btn btn-primary" }, "Save limit")));
  });
}

async function api(method, path, body, afterReauth) {
  const res = await fetch("/api/domus" + path, {
    method,
    headers: { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try { data = await res.json(); } catch (_) { /* empty body */ }
  // A session the Companion app started needs the password again for sensitive actions.
  if (res.status === 403 && data && data.reauth && !afterReauth) {
    const password = await askPassword({
      title: "Enter your password",
      hint: "This action needs your password again.",
      username: me && me.username,
    });
    if (password) {
      await api("POST", "/session/reauth", { password });
      return api(method, path, body, true);
    }
  }
  if (!res.ok) throw new Error((data && data.message) || "Request failed (" + res.status + ")");
  return data;
}

let toastTimer;
function toast(text, isError) {
  toastBox.textContent = text;
  toastBox.classList.toggle("err", !!isError);
  toastBox.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toastBox.classList.remove("show"), isError ? 4000 : 2000);
}

/** Runs fn and reports failures as a toast. */
async function guarded(fn) {
  try { await fn(); } catch (e) { toast(e.message, true); }
}

// navigator.clipboard only exists on https/localhost; domus is often opened over plain http.
async function copyText(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch { /* fall through to the textarea */ }
  const ta = el("textarea", { value: text });
  ta.style.cssText = "position:fixed;opacity:0";
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { /* fall through */ }
  ta.remove();
  return ok;
}

function copyButton(text, label, key) {
  return el("button", {
    type: "button", class: "btn" + (label ? "" : " btn-icon"), "aria-label": label ? null : "Copy " + text, "data-k": key,
    onclick: async () => toast((await copyText(text)) ? "Copied" : "Select the text and copy it manually", false),
  }, icon("copy"), label || "");
}

const fmtDate = (secs) => new Date(secs * 1000).toLocaleDateString();
const fmtDateTime = (secs) => (secs ? new Date(secs * 1000).toLocaleString() : "");
function ago(secs) {
  if (!secs) return "never";
  const d = Math.max(0, Math.floor(Date.now() / 1000 - secs));
  if (d < 90) return "just now";
  if (d < 5400) return Math.round(d / 60) + " minutes ago";
  if (d < 129600) return Math.round(d / 3600) + " hours ago";
  return Math.round(d / 86400) + " days ago";
}

// ------------------------------------------------------------------ data

const TABS = [
  ["dashboard", "dashboard", "Dashboard"],
  ["groups", "folder", "Groups", true],
  ["devices", "bulb", "Devices"],
  ["tokens", "key", "Tokens"],
  ["users", "users", "Users", true],
  ["settings", "settings", "Settings"],
];

/** the logged-in user: { id, username, is_admin } */
let me = null;
let version = "";
const visibleTabs = () => TABS.filter(([, , , adminOnly]) => !adminOnly || me.is_admin);

/** hue (admins only), entities (lights + scenes + group lights), groups, tokens, users (admins only) */
let data = null;
/** per-view state that survives re-renders */
const ui = { tab: "dashboard", draft: null, sub: "pick", fresh: null, open: null, trans: null, setupOpen: false, dragging: false };

async function load() {
  const [hue, entities, rooms, groups, tokens, users, sessions, apps] = await Promise.all([
    me.is_admin ? api("GET", "/hue") : null,
    api("GET", "/lights"), api("GET", "/rooms"), api("GET", "/groups"), api("GET", "/tokens"),
    me.is_admin ? api("GET", "/users") : [],
    api("GET", "/sessions"),
    api("GET", "/apps"),
  ]);
  data = { hue, entities, rooms, groups, tokens, users, sessions, apps };
}

const isScene = (e) => e.entity_id.startsWith("scene.");
const isGroupLight = (e) => e.entity_id.startsWith("light.domus_group_");
// A scene's name is "Room: Name", so sorting by name also groups scenes by room.
const byName = (a, b) => (a.name || a.entity_id).localeCompare(b.name || b.entity_id, undefined, { numeric: true, sensitivity: "base" });
const realLights = () => data.entities.filter((e) => !isScene(e) && !isGroupLight(e)).sort(byName);
const scenes = () => data.entities.filter(isScene).sort(byName);
const find = (id) => data.entities.find((e) => e.entity_id === id);
const label = (id) => { const e = find(id); return (e && e.name) || id; };
const nameOf = (e) => e.name || e.entity_id;
const sceneRoom = (s) => (nameOf(s).includes(": ") ? nameOf(s).split(": ")[0] : "Other");
const sceneShort = (s) => (nameOf(s).includes(": ") ? nameOf(s).split(": ").slice(1).join(": ") : nameOf(s));
const groupSwitchId = (name) => "light.domus_group_" + name;

// ------------------------------------------------------------ light model

// Color temperature is shown in Kelvin, the unit people know; the API takes mireds or Kelvin.
const kelvin = (mirek) => Math.round(1e6 / mirek);
const unreachable = (e) => e.state === "unavailable";
const isOn = (e) => e.state === "on";
const supports = (e, mode) => (e.supported_color_modes || []).includes(mode);
const TRANSITIONS = [[null, "Default"], [0, "Instant"], [1, "1 s"], [3, "3 s"], [10, "10 s"]];
const SWATCHES = [["#ff3b30", "Red"], ["#ff9500", "Orange"], ["#ffd60a", "Yellow"], ["#34c759", "Green"], ["#00c7be", "Teal"], ["#0a84ff", "Blue"], ["#bf5af2", "Purple"], ["#ff2d92", "Pink"]];
const pctOf = (e) => (typeof e.brightness === "number" ? Math.round(e.brightness / 255 * 100) : null);

// CIE xy to a display color, for the lamp's wash and disc.
function xyToHex([x, y]) {
  if (!y) return "#ffffff";
  const X = x / y, Z = (1 - x - y) / y;
  let c = [X * 1.656492 - 0.354851 - Z * 0.255038, -X * 0.707196 + 1.655397 + Z * 0.036152, X * 0.051713 - 0.121364 + Z * 1.01153];
  const top = Math.max(...c, 1);
  c = c.map((v) => Math.max(0, v) / top).map((v) => (v <= 0.0031308 ? 12.92 * v : 1.055 * v ** (1 / 2.4) - 0.055));
  return "#" + c.map((v) => Math.round(Math.min(1, v) * 255).toString(16).padStart(2, "0")).join("");
}

const KELVIN_STOPS = [[2000, [255, 178, 74]], [3200, [255, 217, 160]], [4500, [255, 255, 255]], [5500, [207, 226, 255]], [6500, [158, 196, 255]]];
function kelvinHex(k) {
  const found = KELVIN_STOPS.findIndex(([stop]) => stop >= k);
  const i = Math.max(1, found === -1 ? KELVIN_STOPS.length - 1 : found);
  const [k0, c0] = KELVIN_STOPS[i - 1], [k1, c1] = KELVIN_STOPS[i];
  const f = Math.min(1, Math.max(0, (k - k0) / (k1 - k0)));
  return "#" + c0.map((v, j) => Math.round(v + (c1[j] - v) * f).toString(16).padStart(2, "0")).join("");
}

/** The colour a lamp's wash and disc take: its xy colour, else its white temperature. */
function lampColor(l) {
  if (l.color_mode === "xy" && l.xy_color) return xyToHex(l.xy_color);
  return kelvinHex(l.color_temp ? kelvin(l.color_temp) : 3000);
}

function lightStateText(l) {
  if (unreachable(l)) return "Can't reach this light. Check its power and Zigbee range.";
  if (!isOn(l)) return "Off";
  const pct = pctOf(l);
  const parts = [pct === null ? "On" : pct + "%"];
  if (l.color_mode === "color_temp" && l.color_temp) parts.push(kelvin(l.color_temp) + " K");
  if (l.color_mode === "xy" && l.xy_color) parts.push("color");
  return parts.join(", ");
}

function roomStats(r) {
  const members = r.entity_ids.map(find).filter(Boolean);
  const live = members.filter((m) => !unreachable(m));
  const on = live.filter(isOn);
  const levels = on.map(pctOf).filter((p) => p !== null);
  const pct = levels.length ? Math.round(levels.reduce((a, b) => a + b, 0) / levels.length) : null;
  return { members, live, on, pct, total: r.entity_ids.length };
}

function roomMetaText(r) {
  const s = roomStats(r);
  if (!s.on.length) return "All off";
  return s.on.length + " of " + s.total + " on" + (s.pct === null ? "" : ", " + s.pct + "%");
}

function summaryText() {
  const all = realLights();
  if (!all.length) return "No lights yet.";
  const on = all.filter(isOn).length;
  return on === 0 ? "Every light is off." : on + " of " + all.length + " lights are on.";
}

function lightsFact() {
  const all = realLights(), down = all.filter(unreachable).length;
  return all.length + ", " + all.filter(isOn).length + " on" + (down ? ", " + down + " unreachable" : "");
}

function paintLamp(li, l) {
  const on = isOn(l);
  li.style.setProperty("--lamp", lampColor(l));
  li.style.setProperty("--p", on ? String((pctOf(l) ?? 100) / 100) : "0");
  li.classList.toggle("is-on", on);
  li.querySelector(".lamp-state").textContent = lightStateText(l);
  li.querySelector(".switch").setAttribute("aria-checked", String(on));
}

// Called while a slider moves: a full render would replace the input mid-drag.
function paintLive() {
  for (const li of document.querySelectorAll("[data-light]")) {
    const l = find(li.dataset.light);
    if (l) paintLamp(li, l);
  }
  for (const node of document.querySelectorAll("[data-room]")) {
    const r = data.rooms[+node.dataset.room];
    node.querySelector(".room-meta").textContent = roomMetaText(r);
    node.querySelector(".switch").setAttribute("aria-checked", String(roomStats(r).on.length > 0));
  }
  const summary = document.getElementById("summary");
  if (summary) summary.textContent = summaryText();
  const fact = document.getElementById("lights-fact");
  if (fact) fact.textContent = lightsFact();
}

/** Shows a change on screen before the bridge confirms it; the next sync puts the real state back. */
function previewLights(ids, patch) {
  for (const id of ids) {
    const l = find(id);
    if (l && !unreachable(l)) Object.assign(l, patch);
  }
  paintLive();
}

// Sends one test command (a light, or several driven together) and refreshes what the page shows.
async function control(ids, extra) {
  const body = { ...extra };
  if (ids.length === 1) body.entity_id = ids[0]; else body.entity_ids = ids;
  if (ui.trans !== null) body.transition = ui.trans;
  await api("POST", "/devices/test", body);
  await syncLights();
}

const toBrightness = (pct) => Math.round(pct / 100 * 255);

/**
 * Shows a change on screen, sends it, and puts the bridge's answer on screen. When the request
 * fails the optimistic change is rolled back by reloading the real state.
 */
async function drive(ids, patch, extra) {
  previewLights(ids, patch);
  try {
    await control(ids, extra);
  } catch (e) {
    await syncLights();
    throw e;
  }
}

function activateScene(s) {
  return guarded(async () => {
    const r = await api("POST", "/devices/test", { entity_id: s.entity_id });
    s.state = r.state || s.state;
    toast("Activated " + nameOf(s), false);
    render();
  });
}

// ----------------------------------------------------------- shared parts

function switchBtn({ on, label: text, disabled, onclick, key }) {
  return el("button", {
    type: "button", class: "switch", role: "switch", "aria-checked": String(on), "aria-label": text, disabled, "data-k": key, onclick,
  }, el("span", { class: "knob" }));
}

/** A range with its label and value. `preview` runs on every move, `send` once when the user lets go. */
function slider({ label: text, min, max, step, value, fmt, preview, send, cls, key }) {
  const id = uid();
  const input = el("input", { type: "range", id, class: cls || "", "data-k": key });
  input.min = min; input.max = max; input.step = step || 1; input.value = value;
  const out = el("output", { for: id }, fmt(input.value));
  // A live refresh would replace the input mid-drag, so syncLights holds off until the drag ends.
  input.addEventListener("pointerdown", () => { ui.dragging = true; });
  input.addEventListener("pointercancel", () => { ui.dragging = false; });
  input.addEventListener("input", () => { ui.dragging = true; out.textContent = fmt(input.value); preview(+input.value); });
  input.addEventListener("change", () => { ui.dragging = false; guarded(() => send(+input.value)); });
  return el("div", { class: "ctl" }, el("label", { for: id }, text), input, out);
}

const rgbOf = (hex) => [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16));

function lightPanel(l, key) {
  const ids = [l.entity_id];
  const rows = [slider({
    label: "Brightness", min: 1, max: 100, value: pctOf(l) ?? 100, fmt: (v) => v + "%", key: key + ":bri",
    preview: (v) => previewLights(ids, { state: "on", brightness: toBrightness(v) }),
    send: (v) => control(ids, { brightness_pct: v }),
  })];
  if (supports(l, "color_temp")) {
    rows.push(slider({
      label: "White temperature", min: 2000, max: 6500, step: 100, value: l.color_temp ? kelvin(l.color_temp) : 3000, fmt: (v) => v + " K",
      cls: "kelvin", key: key + ":ct",
      preview: (v) => previewLights(ids, { state: "on", color_mode: "color_temp", color_temp: Math.round(1e6 / v) }),
      send: (v) => control(ids, { color_temp_kelvin: v }),
    }));
  }
  if (supports(l, "xy")) {
    const setColor = (hex) => guarded(() => control(ids, { rgb_color: rgbOf(hex) }));
    const custom = el("input", { type: "color", value: l.xy_color ? xyToHex(l.xy_color) : "#ffffff", "aria-label": "Custom color", "data-k": key + ":custom" });
    custom.addEventListener("change", () => setColor(custom.value));
    rows.push(el("div", { class: "ctl" }, el("span", { class: "ctl-name" }, "Color"),
      el("div", { class: "swatches", role: "group", "aria-label": "Color" },
        SWATCHES.map(([hex, name]) => el("button", {
          type: "button", class: "sw", style: "background:" + hex, "aria-label": name, title: name, "data-k": key + ":sw:" + hex,
          onclick: () => setColor(hex),
        })), custom),
      el("span", {})));
  }
  rows.push(el("div", { class: "ctl" }, el("span", { class: "ctl-name" }, "Transition"),
    seg({ label: "Transition", options: TRANSITIONS, current: ui.trans, onpick: (v) => { ui.trans = v; render(); }, key: key + ":trans" }),
    el("span", {})));
  return el("div", { class: "lamp-panel", id: "panel-" + l.entity_id }, rows);
}

/** One light. `ctx` keeps keys unique when a light shows in two lists; `detail` adds what Devices needs. */
function lampRow(l, ctx, detail) {
  const down = unreachable(l);
  const openKey = ctx + ":" + l.entity_id;
  const open = ui.open === openKey && !down;
  const caps = [el("span", { class: "badge" }, "dim")];
  if (supports(l, "color_temp")) caps.push(el("span", { class: "badge" }, "white temp"));
  if (supports(l, "xy")) caps.push(el("span", { class: "badge" }, "color"));
  if (down) caps.push(el("span", { class: "badge bad" }, "Unreachable"));
  const li = el("li", { class: "lamp" + (down ? " is-down" : "") + (open ? " is-open" : ""), "data-light": l.entity_id },
    el("span", { class: "lamp-wash", "aria-hidden": "true" }),
    el("span", { class: "lamp-meter", "aria-hidden": "true" }),
    el("span", { class: "lamp-disc", "aria-hidden": "true" }),
    el("button", {
      type: "button", class: "lamp-main", disabled: down, "aria-expanded": down ? null : String(open),
      "aria-controls": open ? "panel-" + l.entity_id : null, "data-k": "lamp:" + openKey,
      onclick: () => { ui.open = open ? null : openKey; render(); },
    },
      el("span", { class: "lamp-name" }, nameOf(l)),
      el("span", { class: "lamp-state" }),
      detail ? el("span", { class: "lamp-detail" }, el("code", {}, l.entity_id), caps) : "",
      down ? "" : icon("chevron")),
    switchBtn({
      on: isOn(l), label: nameOf(l), disabled: down, key: "sw:" + openKey,
      onclick: () => guarded(() => drive([l.entity_id], { state: isOn(l) ? "off" : "on" }, { on: !isOn(l) })),
    }),
    open ? lightPanel(l, openKey) : "");
  paintLamp(li, l);
  return li;
}

const roomIndex = (r) => data.rooms.indexOf(r);

function roomControls(r, ctx) {
  const s = roomStats(r);
  return {
    toggle: switchBtn({
      on: s.on.length > 0, label: r.name + ", all lights", disabled: !s.live.length, key: "room:" + ctx + ":" + roomIndex(r),
      onclick: () => {
        const turnOn = roomStats(r).on.length === 0;
        return guarded(() => drive(r.entity_ids, { state: turnOn ? "on" : "off" }, { on: turnOn }));
      },
    }),
    dim: slider({
      label: "Brightness", min: 1, max: 100, value: s.pct ?? 100, fmt: (v) => v + "%", key: "roombri:" + ctx + ":" + roomIndex(r),
      preview: (v) => previewLights(r.entity_ids, { state: "on", brightness: toBrightness(v) }),
      send: (v) => control(r.entity_ids, { brightness_pct: v }),
    }),
  };
}

function sceneChip(s, room) {
  return el("button", {
    type: "button", class: "btn scene", "aria-label": "Activate " + nameOf(s), title: s.entity_id, "data-k": "scene:" + s.entity_id,
    onclick: () => activateScene(s),
  }, icon("play"), room ? sceneShort(s) : nameOf(s));
}

// ------------------------------------------------------------ auth screen

/** The house on the sign-in card: each filled field lights one window. */
function facade(count) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 132 96");
  svg.setAttribute("class", "facade");
  svg.setAttribute("aria-hidden", "true");
  const add = (tag, attrs) => { const n = document.createElementNS(SVG_NS, tag); for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v); svg.append(n); return n; };
  const line = { fill: "none", stroke: "currentColor", "stroke-width": "2", "stroke-linejoin": "round", "stroke-linecap": "round" };
  add("path", { d: "M8 44L66 6l58 38", ...line });
  add("path", { d: "M18 40v50h96V40", ...line });
  add("path", { d: "M2 90h128", ...line });
  add("path", { d: "M58 90V64h16v26", ...line });
  const wins = [[28, 52], [88, 52], [58, 30]].slice(0, count).map(([x, y]) => add("rect", { x, y, width: 16, height: 16, rx: 1.5, class: "win", "stroke-width": "2" }));
  return { svg, wins };
}

function authScreen(setupDone, needCode) {
  const user = el("input", { type: "text", autocomplete: "username", autocapitalize: "none", spellcheck: false });
  const pw = el("input", { type: "password", autocomplete: setupDone ? "current-password" : "new-password" });
  const code = el("input", { type: "text", autocomplete: "off", spellcheck: false });
  const userField = field("Username", user);
  const pwField = field("Password", pw, { hint: setupDone ? null : "At least 12 characters." });
  const codeField = field("Setup code", code, { hint: "domus prints it in its log when it starts." });
  const askCode = !setupDone && needCode;
  const inputs = askCode ? [user, pw, code] : [user, pw];
  const { svg, wins } = facade(inputs.length);
  const light = () => inputs.forEach((input, i) => wins[i].classList.toggle("lit", input.value !== ""));
  inputs.forEach((input) => input.addEventListener("input", light));

  const submit = (e) => {
    e.preventDefault();
    userField.setError(user.value ? "" : "Enter a username.");
    pwField.setError(!pw.value ? "Enter the password." : !setupDone && pw.value.length < 12 ? "Use at least 12 characters." : "");
    if (askCode) codeField.setError(code.value ? "" : "Enter the setup code from the domus log.");
    if (!user.value || !pw.value || (!setupDone && pw.value.length < 12) || (askCode && !code.value)) return;
    guarded(async () => {
      await api("POST", setupDone ? "/login" : "/setup", { username: user.value, password: pw.value, setup_code: code.value });
      await start();
    });
  };

  root.replaceChildren(el("main", { class: "auth" }, el("div", { class: "auth-card" },
    svg, el("h1", {}, "domus"),
    el("p", { class: "muted" }, setupDone ? "Log in to continue." : "First-time setup: choose the admin username and password."),
    el("form", {
      onsubmit: submit, noValidate: true,
      // Implicit submission fires on the Enter that confirms an IME candidate; isEnter filters it out.
      onkeydown: (e) => { if (e.key === "Enter" && !isEnter(e)) e.preventDefault(); },
    },
      userField, pwField, askCode ? codeField : "",
      el("button", { type: "submit", class: "btn btn-primary" }, setupDone ? "Log in" : "Create admin")))));
}

function errorScreen(message) {
  root.replaceChildren(el("main", { class: "auth" }, el("div", { class: "auth-card" },
    el("h1", {}, "Can't reach domus"),
    el("p", { class: "muted" }, message + ". Check that the server is running and that this device is on the same network."),
    el("div", {}, el("button", { type: "button", class: "btn btn-primary", onclick: () => start().catch((e) => errorScreen(e.message)) }, "Try again")))));
}

// ------------------------------------------------------------------ shell

async function logout() {
  await guarded(async () => {
    await api("POST", "/logout");
    ui.draft = null;
    ui.fresh = null;
    ui.open = null;
    await start();
  });
}

function bridgeStatus() {
  if (!data.hue) return "";
  const { paired, running } = data.hue;
  return el("div", { class: "bridge" },
    el("span", { class: "dot" + (!paired ? " off" : running ? "" : " bad") }),
    !paired ? "Bridge not paired" : running ? "Bridge connected" : "Bridge disconnected");
}

const navLink = ([id, ico, name], key) => el("a", {
  class: "nav-link", href: "#" + id, "aria-current": id === ui.tab ? "page" : null, "data-k": key + id,
}, icon(ico), name);

/** The phone's overflow: the tabs that do not fit the bottom bar, plus who is signed in and Log out. */
function moreSheet(rest) {
  return openDialog((close, titleId) => el("div", { class: "dialog-body" },
    el("h2", { id: titleId, class: "sr-only" }, "More"),
    rest.map((t) => { const a = navLink(t, "more:"); a.addEventListener("click", () => close(null)); return a; }),
    bridgeStatus(),
    el("p", { class: "muted small" }, "Signed in as " + me.username + (version ? ". domus " + version : "")),
    el("div", { class: "row" },
      el("button", { type: "button", class: "btn", onclick: () => { close(null); logout(); } }, icon("logout"), "Log out"))), "sheet");
}

function shell() {
  const tabs = visibleTabs();
  const main = el("main", { id: "main" });
  const side = el("nav", { class: "side", "aria-label": "Main" },
    el("div", { class: "wordmark" }, mark(), "domus"),
    tabs.map((t) => navLink(t, "nav:")),
    el("div", { class: "side-foot" },
      bridgeStatus(),
      el("div", { class: "who" }, el("span", { class: "muted who-name" }, me.username),
        el("button", { type: "button", class: "btn", "data-k": "logout", onclick: logout }, icon("logout"), "Log out")),
      version ? el("span", { class: "muted version", title: "domus " + version }, "domus " + version) : ""));
  const primary = tabs.slice(0, 4), rest = tabs.slice(4);
  const bottom = el("nav", { class: "bottomnav", "aria-label": "Main" },
    primary.map(([id, ico, name]) => el("a", { href: "#" + id, "aria-current": id === ui.tab ? "page" : null, "data-k": "bottom:" + id }, icon(ico), name)),
    el("button", {
      type: "button", class: rest.some(([id]) => id === ui.tab) ? "is-current" : "", "aria-haspopup": "dialog", "data-k": "bottom:more",
      onclick: () => moreSheet(rest),
    }, icon("more"), "More"));
  const top = el("div", { class: "topbar" }, el("div", { class: "wordmark" }, mark(), "domus"), bridgeStatus());
  root.replaceChildren(el("div", { class: "app" }, side, el("div", {}, top, main)), bottom);
  return main;
}

// -------------------------------------------------------------- dashboard

const rooms = () => data.rooms || [];

function roomCard(r) {
  const { toggle, dim } = roomControls(r, "dash");
  const mine = scenes().filter((s) => sceneRoom(s) === r.name);
  return el("section", { class: "card", "data-room": String(roomIndex(r)), "aria-label": r.name },
    el("div", { class: "room-head" }, el("h2", {}, r.name), el("span", { class: "room-meta" }, roomMetaText(r)), toggle),
    mine.length ? el("div", { class: "scenes", role: "group", "aria-label": r.name + " scenes" }, mine.map((s) => sceneChip(s, r))) : "",
    dim,
    el("ul", { class: "lamps" }, r.entity_ids.map(find).filter(Boolean).sort(byName).map((l) => lampRow(l, "dash"))));
}

function setupCard() {
  const first = data.groups[0];
  return el("details", { class: "card setup", open: ui.setupOpen, ontoggle: (e) => { ui.setupOpen = e.target.open; } },
    el("summary", { "data-k": "setup" }, "Set up hasscontrol on the watch", el("span", { class: "chev" }, icon("chevron"))),
    el("ol", { class: "steps" },
      el("li", {}, "Enter the server URL.", el("div", { class: "copyline" }, el("code", {}, location.origin), copyButton(location.origin, null, "copy:url"))),
      el("li", {}, "Create an access token in ", el("a", { href: "#tokens" }, "Tokens"), " and paste it."),
      first
        ? el("li", {}, "Enter the group name exactly.", el("div", { class: "copyline" }, el("code", {}, first.name), copyButton(first.name, null, "copy:group")))
        : el("li", {}, "Create a group in ", me.is_admin ? el("a", { href: "#groups" }, "Groups") : "Groups", " and enter its exact name."),
      el("li", {}, "After changing a group, run ", el("b", {}, "Refresh entities"), " on the watch.")),
    location.protocol === "https:" ? "" : el("p", { class: "muted small setup-note" }, "hasscontrol needs an https:// URL with a trusted certificate; see the README for a reverse proxy."));
}

function dashboard() {
  const all = realLights();
  const down = all.filter(unreachable);
  const realRooms = rooms().filter((r) => r.kind === "room");
  const zones = rooms().filter((r) => r.kind !== "room");
  const housed = new Set(realRooms.flatMap((r) => r.entity_ids));
  const loose = all.filter((l) => !housed.has(l.entity_id));
  const roomNames = new Set(realRooms.map((r) => r.name));
  const looseScenes = scenes().filter((s) => !roomNames.has(sceneRoom(s)));
  const hue = data.hue;

  const mainCol = el("div", { class: "stack" },
    hue && hue.paired && !hue.running ? el("div", { class: "notice bad", role: "status" }, icon("alert"),
      el("span", {}, "The Hue Bridge is not answering. Lights show their last known state. ", el("a", { href: "#settings" }, "Check the bridge"))) : "",
    down.length ? el("div", { class: "notice", role: "status" }, icon("alert"),
      "Can't reach " + down.map(nameOf).join(", ") + ". The watch shows " + (down.length === 1 ? "it" : "them") + " as unavailable.") : "",
    !all.length ? el("section", { class: "card" }, el("div", { class: "empty" },
      el("b", {}, "No lights yet"),
      me.is_admin ? el("span", {}, "Pair the Hue Bridge and its lights, rooms and scenes show up here.") : el("span", {}, "Ask an admin to pair the Hue Bridge."),
      me.is_admin ? el("a", { class: "btn btn-primary", href: "#settings" }, "Pair the Hue Bridge") : "")) : "",
    realRooms.map(roomCard),
    loose.length ? el("section", { class: "card", "aria-label": "Other lights" },
      el("div", { class: "room-head" }, el("h2", {}, "Other lights")),
      el("ul", { class: "lamps" }, loose.map((l) => lampRow(l, "dash")))) : "",
    looseScenes.length ? el("section", { class: "card" }, el("h2", {}, "Other scenes"),
      el("div", { class: "scenes" }, looseScenes.map((s) => sceneChip(s, null)))) : "");

  const sideCol = el("aside", { class: "stack", "aria-label": "Status and setup" },
    el("section", { class: "card" }, el("h2", {}, "At a glance"),
      el("dl", { class: "glance" },
        el("dt", {}, "Lights"), el("dd", { id: "lights-fact" }, lightsFact()),
        el("dt", {}, "Scenes"), el("dd", {}, scenes().length + ", from the Hue Bridge"),
        el("dt", {}, "Groups"), el("dd", {}, data.groups.length + ", " + data.groups.filter((g) => g.expose_light).length + " with an all-lights switch"),
        hue ? [el("dt", {}, "Hue Bridge"), el("dd", {}, !hue.paired ? "Not paired" : (hue.running ? "Connected, " : "Disconnected, ") + hue.ip)] : "")),
    zones.length ? el("section", { class: "card" }, el("h2", {}, "Zones"), zones.map((z) => {
      const { toggle, dim } = roomControls(z, "dash");
      return el("div", { class: "zone", "data-room": String(roomIndex(z)) },
        el("div", { class: "room-head" }, el("h3", {}, z.name), el("span", { class: "room-meta" }, roomMetaText(z)), toggle), dim);
    })) : "",
    setupCard());

  return [
    el("header", { class: "page-head" }, el("h1", { id: "summary", "aria-live": "polite" }, summaryText())),
    el("div", { class: "dash" }, mainCol, sideCol),
  ];
}

// ----------------------------------------------------------------- groups

const GROUP_NAME = /^[A-Za-z0-9_-]{1,64}$/;

function newDraft(group) {
  return group
    ? { name: group.name, orig: group.name, members: [...group.members], expose: group.expose_light, isNew: false, dirty: false }
    : { name: "", orig: null, members: [], expose: false, isNew: true, dirty: false };
}

function groups() {
  if (!ui.draft || (!ui.draft.isNew && !data.groups.some((g) => g.name === ui.draft.orig))) {
    ui.draft = data.groups.length ? newDraft(data.groups[0]) : newDraft(null);
  }
  const d = ui.draft;
  const select = (name) => { ui.draft = name === null ? newDraft(null) : newDraft(data.groups.find((g) => g.name === name)); render(); };
  const list = el("nav", { class: "glist-wrap", "aria-label": "Groups" }, el("ul", { class: "glist" },
    data.groups.map((g) => el("li", {}, el("button", {
      type: "button", class: "btn", "aria-current": !d.isNew && g.name === d.orig ? "true" : null, "data-k": "group:" + g.name,
      onclick: () => select(g.name),
    }, el("span", {
      class: "gname",
      // The name is cut with an ellipsis when long; the tooltip shows it whole only then.
      onmouseenter: (e) => { e.currentTarget.title = e.currentTarget.scrollWidth > e.currentTarget.clientWidth ? g.name : ""; },
    }, g.name), " ", el("span", { class: "count muted" }, String(g.members.length))))),
    el("li", {}, el("button", { type: "button", class: "btn", "aria-current": d.isNew ? "true" : null, "data-k": "group:new", onclick: () => select(null) },
      icon("plus"), "New group"))));
  const gsel = el("select", { "data-k": "gsel", onchange: (e) => select(e.target.value === "" ? null : e.target.value) },
    data.groups.map((g) => el("option", { value: g.name, selected: !d.isNew && g.name === d.orig }, g.name)),
    el("option", { value: "", selected: d.isNew }, "New group"));
  return [
    el("header", { class: "page-head" }, el("h1", {}, "Groups"), el("p", {}, "The watch imports one group and shows its members in this order.")),
    data.hue.paired ? "" : el("div", { class: "notice" }, icon("alert"),
      el("span", {}, "There are no lights to pick yet. ", el("a", { href: "#settings" }, "Pair the Hue Bridge"), " first.")),
    el("div", { class: "split" }, list, el("div", {}, field("Group", gsel, { cls: "gsel" }), groupEditor(d))),
  ];
}

// The watch face is round: rows stop at 44% of the diameter from the centre, and the scroll arc's inner edge is at 47.7%,
// which leaves 3.7% (about 10px on a 264px face) between a row end and the arc.
const WATCH_ROW_REACH = 0.44;
// A row never gets narrower than this share of the diameter (room for about 8 characters and the state);
// where the chord is shorter the row keeps this width and fades out instead.
const WATCH_ROW_MIN = 0.4;

/** The round watch: a title, a list that scrolls inside the circle, and a scroll arc along the rim. */
function watchPreview() {
  const title = el("div", { class: "w-title" });
  const list = el("ul", { class: "w-list", tabIndex: 0, "aria-label": "Watch preview, scrollable" });
  const scroll = document.createElementNS(SVG_NS, "svg");
  scroll.setAttribute("viewBox", "0 0 100 100");
  scroll.setAttribute("class", "w-scroll");
  scroll.setAttribute("aria-hidden", "true");
  const arc = (stroke) => {
    const path = document.createElementNS(SVG_NS, "path");
    const attrs = { d: "M90.96 24.4A48.3 48.3 0 0 1 90.96 75.6", pathLength: "100", fill: "none", stroke, "stroke-width": "1.2", "stroke-linecap": "round" };
    for (const [k, v] of Object.entries(attrs)) path.setAttribute(k, v);
    scroll.append(path);
    return path;
  };
  arc("rgb(255 255 255 / .2)");
  const thumb = arc("#e4e9e3");
  const face = el("div", { class: "watch-face" }, title, list, scroll);
  const watch = el("div", { class: "watch", role: "group", "aria-label": "Watch preview" }, face);

  // Sizes every row to the chord of the circle at the row's height, so rows narrow near the top and bottom.
  function layout() {
    const size = face.clientWidth;
    if (!size) return;
    const centre = size / 2, radius = size * WATCH_ROW_REACH, floor = size * WATCH_ROW_MIN;
    const top0 = list.offsetTop - list.scrollTop;
    for (const item of list.children) {
      const top = top0 + item.offsetTop, bottom = top + item.offsetHeight;
      const far = Math.max(Math.abs(top - centre), Math.abs(bottom - centre));
      // the chord of the circle at whichever row edge is farther from the centre
      const chord = 2 * Math.sqrt(Math.max(0, radius * radius - far * far));
      item.style.width = Math.floor(Math.max(floor, chord)) + "px";
      item.style.opacity = chord >= floor ? "" : String(Math.max(0, chord / floor) ** 2);
    }
    const total = list.scrollHeight, visible = list.clientHeight;
    const scrolls = total > visible + 1;
    scroll.style.display = scrolls ? "" : "none";
    if (!scrolls) return;
    const length = Math.max(12, visible / total * 100);
    const start = list.scrollTop / (total - visible) * (100 - length);
    thumb.setAttribute("stroke-dasharray", length + " 200");
    thumb.setAttribute("stroke-dashoffset", String(-start));
  }
  list.addEventListener("scroll", layout, { passive: true });
  new ResizeObserver(layout).observe(face);

  /** Shows the group as the watch will: the name, an optional all-lights row, then the members in order. */
  function show(d) {
    const anyOn = d.members.some((id) => { const e = find(id); return e && !isScene(e) && isOn(e); });
    title.textContent = d.name || "New group";
    list.replaceChildren(...[
      d.expose ? el("li", { class: "w-item" }, el("span", {}, "All lights"), el("span", { class: anyOn ? "w-on" : "" }, anyOn ? "On" : "Off")) : "",
      ...d.members.map((id) => {
        const e = find(id);
        const scene = e && isScene(e);
        const down = e && unreachable(e);
        const on = e && !scene && isOn(e);
        return el("li", { class: "w-item" + (down ? " down" : "") }, el("span", {}, label(id)),
          el("span", { class: on ? "w-on" : "" }, scene ? icon("play") : down ? "n/a" : on ? "On" : "Off"));
      }),
    ].filter(Boolean));
    if (!d.members.length && !d.expose) list.append(el("li", { class: "w-empty" }, "No members yet"));
    layout();
  }
  return { watch, show };
}

/** The checkboxes for every light, scene and other group's switch, with a search box. */
function memberPicker(d, onchange) {
  const pickRow = (e) => {
    const [row, box] = checkbox([nameOf(e), unreachable(e) ? el("span", { class: "badge bad" }, "Unreachable") : ""], { checked: d.members.includes(e.entity_id) });
    box.addEventListener("change", () => {
      d.members = box.checked ? [...d.members, e.entity_id] : d.members.filter((x) => x !== e.entity_id);
      onchange();
    });
    row.className = "pick";
    row.dataset.name = nameOf(e).toLowerCase();
    return row;
  };
  const section = (title, items) => (items.length ? el("fieldset", { class: "picks" }, el("legend", {}, title), items.map(pickRow)) : "");
  const byRoom = new Map();
  for (const s of scenes()) {
    if (!byRoom.has(sceneRoom(s))) byRoom.set(sceneRoom(s), []);
    byRoom.get(sceneRoom(s)).push(s);
  }
  const own = groupSwitchId(d.orig || "");
  const picks = el("div", {},
    section("Lights", realLights()),
    [...byRoom].map(([room, items]) => section("Scenes in " + room, items)),
    section("Group switches", data.entities.filter((e) => isGroupLight(e) && e.entity_id !== own).sort(byName)));
  const none = el("p", { class: "muted pick-none" }, "No lights or scenes yet.");
  none.hidden = picks.children.length > 0;
  const search = el("input", { type: "search", placeholder: "Search lights and scenes", "aria-label": "Search lights and scenes" });
  search.addEventListener("input", () => {
    const q = search.value.trim().toLowerCase();
    let shown = 0;
    for (const set of picks.querySelectorAll("fieldset")) {
      let any = false;
      for (const p of set.querySelectorAll(".pick")) { const hit = p.dataset.name.includes(q); p.hidden = !hit; any = any || hit; }
      set.hidden = !any;
      shown += any ? 1 : 0;
    }
    none.textContent = picks.children.length ? "Nothing matches “" + search.value + "”." : "No lights or scenes yet.";
    none.hidden = shown > 0;
  });
  return [search, picks, none];
}

function groupEditor(d) {
  const nameInput = el("input", { type: "text", value: d.name, readOnly: !d.isNew, autocomplete: "off", autocapitalize: "none", spellcheck: false, placeholder: "Garmin" });
  const nameField = field("Group name", nameInput, { cls: "grow", hint: d.isNew ? "Letters, digits, _ and -, up to 64. The watch needs this exact name." : "A saved group keeps its name." });
  const [exposeLabel, exposeBox] = checkbox(["Add an all-lights switch "], { checked: d.expose });
  const exposeCode = el("code", {});
  exposeLabel.lastChild.append(exposeCode);
  const warn = el("div", { class: "warn-slot" });
  const order = el("ol", { class: "members" });
  const watch = watchPreview();
  const hint = el("span", { class: "muted small", role: "status" }, d.dirty ? "Unsaved changes" : "");
  const announce = el("p", { class: "sr-only", "aria-live": "polite" });
  const ids = { pickTab: uid(), orderTab: uid(), pickPane: uid(), orderPane: uid() };
  const pickTab = el("button", { type: "button", class: "tab", role: "tab", id: ids.pickTab, "aria-controls": ids.pickPane, "data-k": "tab:pick" });
  const orderTab = el("button", { type: "button", class: "tab", role: "tab", id: ids.orderTab, "aria-controls": ids.orderPane, "data-k": "tab:order" });
  const pickPane = el("div", { role: "tabpanel", id: ids.pickPane, "aria-labelledby": ids.pickTab });
  const orderPane = el("div", { role: "tabpanel", id: ids.orderPane, "aria-labelledby": ids.orderTab });

  function nameProblem() {
    const name = d.name.trim();
    if (name === "") return "Enter a group name.";
    if (name.length > 64) return "Use 64 characters or fewer.";
    return GROUP_NAME.test(name) ? "" : "Use letters, digits, _ and - only.";
  }
  function markDirty() {
    d.dirty = true;
    hint.textContent = "Unsaved changes";
    exposeCode.textContent = groupSwitchId(d.name || "<name>");
  }
  function syncSub(focus) {
    for (const [tab, pane, sub] of [[pickTab, pickPane, "pick"], [orderTab, orderPane, "order"]]) {
      const active = ui.sub === sub;
      tab.setAttribute("aria-selected", String(active));
      tab.tabIndex = active ? 0 : -1;
      pane.hidden = !active;
      if (active && focus) tab.focus();
    }
  }
  for (const [tab, sub] of [[pickTab, "pick"], [orderTab, "order"]]) {
    tab.addEventListener("click", () => { ui.sub = sub; syncSub(); });
    tab.addEventListener("keydown", (e) => {
      if (e.key !== "ArrowLeft" && e.key !== "ArrowRight") return;
      ui.sub = sub === "pick" ? "order" : "pick";
      syncSub(true);
    });
  }
  function move(id, delta) {
    const i = d.members.indexOf(id), j = i + delta;
    if (j < 0 || j >= d.members.length) return;
    [d.members[i], d.members[j]] = [d.members[j], d.members[i]];
    markDirty(); derived();
    announce.textContent = label(id) + " moved to position " + (j + 1) + " of " + d.members.length + ".";
    // keep the focus on the same direction, or on the other arrow when this one just reached the end of the list
    const again = order.querySelector(`[data-move="${CSS.escape(id + ":" + delta)}"]`);
    const other = order.querySelector(`[data-move="${CSS.escape(id + ":" + -delta)}"]`);
    (again && !again.disabled ? again : other).focus();
  }
  let dragId = null;
  function memberRow(id, i) {
    const e = find(id);
    const li = el("li", { draggable: "true" },
      el("span", { class: "pos" }, String(i + 1)),
      el("span", { class: "grow" }, label(id)),
      e && unreachable(e) ? el("span", { class: "badge bad" }, "Unreachable") : "",
      el("button", { type: "button", class: "btn btn-icon", "aria-label": "Move " + label(id) + " up", disabled: i === 0, "data-move": id + ":-1", onclick: () => move(id, -1) }, icon("up")),
      el("button", { type: "button", class: "btn btn-icon", "aria-label": "Move " + label(id) + " down", disabled: i === d.members.length - 1, "data-move": id + ":1", onclick: () => move(id, 1) }, icon("down")));
    const clearDrop = () => li.classList.remove("drop-before", "drop-after");
    li.addEventListener("dragstart", () => { dragId = id; li.classList.add("drag"); });
    li.addEventListener("dragend", () => { li.classList.remove("drag"); clearDrop(); });
    li.addEventListener("dragover", (ev) => {
      ev.preventDefault();
      if (!dragId || dragId === id) return;
      // the dragged item takes over the target's slot: above it when moving up, below it when moving down
      const goingDown = d.members.indexOf(dragId) < d.members.indexOf(id);
      li.classList.toggle("drop-after", goingDown);
      li.classList.toggle("drop-before", !goingDown);
    });
    li.addEventListener("dragleave", clearDrop);
    li.addEventListener("drop", () => {
      clearDrop();
      if (!dragId || dragId === id) return;
      const target = d.members.indexOf(id);
      d.members = d.members.filter((x) => x !== dragId);
      d.members.splice(target, 0, dragId);
      markDirty(); derived();
    });
    return li;
  }
  function derived() {
    const count = d.members.length + (d.expose ? 1 : 0);
    pickTab.textContent = "Pick members (" + d.members.length + ")";
    orderTab.textContent = "Order (" + count + ")";
    warn.replaceChildren(count > 12
      ? el("div", { class: "notice", role: "status" }, icon("alert"), count + " entries. Older watches may fail to load more than about a dozen.")
      : "");
    order.replaceChildren(
      d.expose ? el("li", { class: "fixed" }, icon("lock"), el("span", { class: "grow" }, "All lights"), el("span", { class: "badge" }, "always first")) : "",
      ...d.members.map(memberRow));
    if (!count) order.append(el("li", { class: "fixed" }, "Nothing picked yet. Choose members in the other tab."));
    watch.show(d);
  }

  nameInput.addEventListener("input", () => { d.name = nameInput.value; markDirty(); nameField.setError(d.name ? nameProblem() : ""); derived(); });
  exposeBox.addEventListener("change", () => { d.expose = exposeBox.checked; markDirty(); derived(); });
  pickPane.append(...memberPicker(d, () => { markDirty(); derived(); }));
  orderPane.append(el("p", { class: "muted small order-note" }, "Drag a row, or use its arrows."), order, announce);
  derived(); syncSub();
  exposeCode.textContent = groupSwitchId(d.name || "<name>");

  const save = () => guarded(async () => {
    const problem = nameProblem();
    nameField.setError(problem);
    if (problem) { nameInput.focus(); throw new Error("Group name: letters, digits, _ and - only (max 64)."); }
    await saveGroup(d);
  });
  return el("section", { class: "card", "aria-label": d.isNew ? "New group" : "Group " + d.orig },
    el("div", { class: "row row-top editor-head" }, nameField,
      d.isNew ? "" : el("div", { class: "row editor-acts" },
        copyButton(d.orig, "Copy name", "copy:name"),
        el("button", { type: "button", class: "btn btn-danger", "data-k": "group:delete", onclick: () => guarded(() => deleteGroup(d)) }, "Delete"))),
    exposeLabel, warn,
    el("div", { class: "editor" },
      el("div", {}, el("div", { class: "tabs", role: "tablist", "aria-label": "Members" }, pickTab, orderTab), pickPane, orderPane),
      el("div", { class: "watch-wrap" }, el("p", {}, "On the watch"), watch.watch)),
    el("div", { class: "savebar" }, el("button", { type: "button", class: "btn btn-primary", "data-k": "group:save", onclick: save }, "Save group"), hint));
}

async function saveGroup(d) {
  const name = d.name.trim();
  await api("PUT", "/groups/" + encodeURIComponent(name), { members: d.members, expose_light: d.expose });
  ui.draft = { ...d, name, orig: name, isNew: false, dirty: false };
  await refresh();
  toast("Group “" + name + "” saved. Run Refresh entities on the watch.", false);
}

async function deleteGroup(d) {
  if (!(await confirmDialog({ title: "Delete group “" + d.orig + "”?", body: "The watch loses it the next time it refreshes.", action: "Delete group" }))) return;
  await api("DELETE", "/groups/" + encodeURIComponent(d.orig));
  ui.draft = null;
  await refresh();
  toast("Group deleted", false);
}

// ---------------------------------------------------------------- devices

/** When the scene was last activated: its state is a timestamp, or "unknown" before the first time. */
const sceneLastActivated = (s) => {
  const at = Date.parse(s.state);
  return Number.isNaN(at) ? "never" : ago(at / 1000);
};

function devices() {
  const all = realLights(), rs = rooms(), ss = scenes();
  return [
    el("header", { class: "page-head" }, el("h1", {}, "Devices"), el("p", {}, "Test your Hue setup without the watch.")),
    el("div", { class: "stack" },
      el("section", { class: "card" }, el("h2", {}, "Lights"),
        all.length ? el("ul", { class: "lamps" }, all.map((l) => lampRow(l, "dev", true)))
          : el("div", { class: "empty" }, el("b", {}, "No lights yet"), el("span", {}, "Pair the Hue Bridge in ", el("a", { href: "#settings" }, "Settings"), "."))),
      rs.length ? el("section", { class: "card" }, el("h2", {}, "Rooms and zones"), rs.map((r) => {
        const { toggle, dim } = roomControls(r, "dev");
        return el("div", { class: "zone", "data-room": String(roomIndex(r)) },
          el("div", { class: "room-head" }, el("h3", {}, r.name, " ", el("span", { class: "badge" }, r.kind), " ", el("span", { class: "badge" }, r.entity_ids.length + " lights")),
            el("span", { class: "room-meta" }, roomMetaText(r)), toggle),
          el("p", { class: "muted small" }, r.entity_ids.map(label).join(", ")), dim);
      })) : "",
      el("section", { class: "card" }, el("h2", {}, "Scenes"),
        ss.length ? el("ul", { class: "rows rows-tight" }, ss.map((s) => el("li", {},
          el("div", {}, el("div", { class: "name" }, nameOf(s)), el("div", { class: "meta id-line" }, el("code", {}, s.entity_id)),
            el("div", { class: "meta", title: Number.isNaN(Date.parse(s.state)) ? null : new Date(s.state).toLocaleString() }, "Last activated " + sceneLastActivated(s))),
          el("div", { class: "acts" }, el("button", { type: "button", class: "btn act", "data-k": "act:" + s.entity_id, onclick: () => activateScene(s) }, icon("play"), "Activate")))))
          : el("div", { class: "empty" }, el("b", {}, "No scenes found"), el("span", {}, "Scenes you make in the Hue app show up here.")))),
  ];
}

// ----------------------------------------------------------------- tokens

function tokens() {
  const name = el("input", { type: "text", placeholder: "Garmin watch", autocomplete: "off" });
  const nameField = field("Token name", name, { cls: "grow" });
  const boxes = data.groups.map((g) => { const [lab, box] = checkbox(g.name, {}); return { lab, box, name: g.name }; });
  const [readOnlyLabel, readOnly] = checkbox("Read-only (cannot control lights)", {});
  const expiry = el("select", {}, [["", "Never expires"], ["7", "7 days"], ["30", "30 days"], ["90", "90 days"], ["365", "1 year"]].map(([v, text]) => el("option", { value: v }, text)));
  const create = () => guarded(async () => {
    const tokenName = name.value.trim();
    nameField.setError(tokenName ? "" : "Name the token so you can tell it apart later.");
    if (!tokenName) return name.focus();
    const picked = boxes.filter((b) => b.box.checked).map((b) => b.name);
    const t = await api("POST", "/tokens", {
      name: tokenName,
      scope: picked.length ? picked : null,
      read_only: readOnly.checked,
      expires_days: expiry.value ? Number(expiry.value) : null,
    });
    ui.fresh = { value: t.token, name: tokenName };
    await refresh();
    document.getElementById("fresh-copy")?.focus();
  });
  name.addEventListener("keydown", (e) => { if (isEnter(e)) create(); });

  const fresh = ui.fresh && el("div", { class: "fresh", role: "alert" },
    el("b", {}, "Copy the token for “" + ui.fresh.name + "” now"),
    el("span", {}, "domus keeps only a hash, so this is the one time the token is shown."),
    el("code", { tabIndex: 0, "aria-label": "New token" }, ui.fresh.value),
    el("div", { class: "row" },
      el("button", {
        type: "button", class: "btn btn-primary", id: "fresh-copy", "data-k": "fresh:copy",
        onclick: async () => toast((await copyText(ui.fresh.value)) ? "Token copied" : "Select the text and copy it manually", false),
      }, icon("copy"), "Copy token"),
      el("button", { type: "button", class: "btn", "data-k": "fresh:done", onclick: () => { ui.fresh = null; render(); } }, "Done, hide it")));

  const revoke = async (t) => {
    if (!(await confirmDialog({ title: "Revoke token “" + t.name + "”?", body: "Clients using it stop working.", action: "Revoke token" }))) return;
    await api("DELETE", "/tokens/" + t.id);
    await refresh();
    toast("Token revoked", false);
  };
  const expired = (t) => t.expires && t.expires * 1000 < Date.now();
  const expiryText = (t) => (!t.expires ? "Never expires" : (expired(t) ? "Expired " : "Expires ") + fmtDate(t.expires));

  return [
    el("header", { class: "page-head" }, el("h1", {}, "Tokens"), el("p", {}, "Long-lived access tokens for the watch and other clients. Only a hash is stored.")),
    el("div", { class: "stack" },
      el("section", { class: "card" }, el("h2", {}, "Create a token"),
        el("div", { class: "formgrid" },
          el("div", { class: "row row-top" }, nameField, field("Expiry", expiry, { cls: "grow" })),
          boxes.length ? el("fieldset", { class: "scope" },
            el("legend", {}, me.scope ? "Limit to groups (leave all unchecked for all of yours)" : "Limit to groups (leave all unchecked for full access)"),
            boxes.map((b) => b.lab)) : "",
          readOnlyLabel,
          el("div", {}, el("button", { type: "button", class: "btn btn-primary", "data-k": "token:create", onclick: create }, icon("plus"), "Create token"))),
        fresh),
      el("section", { class: "card" }, el("h2", {}, "Your tokens"),
        data.tokens.length ? el("ul", { class: "rows" }, data.tokens.map((t) => el("li", {},
          el("div", {},
            el("div", { class: "name" }, t.name, t.read_only ? el("span", { class: "badge" }, "read-only") : "",
              expired(t) ? el("span", { class: "badge bad" }, "expired") : ""),
            el("div", { class: "meta" }, (t.scope ? "Groups: " + t.scope.join(", ") : "Full access") + (me.is_admin ? ". Owner " + (t.username || "unknown") : "")),
            el("div", { class: "meta", title: fmtDateTime(t.last_used) }, "Last used " + ago(t.last_used) + ". " + expiryText(t) + ". Created " + fmtDate(t.created))),
          el("div", { class: "acts" }, el("button", { type: "button", class: "btn btn-danger", "data-k": "revoke:" + t.id, onclick: () => guarded(() => revoke(t)) }, "Revoke")))))
          : el("div", { class: "empty" }, el("b", {}, "No tokens yet"), el("span", {}, "Create one above, then paste it into hasscontrol on the watch.")))),
  ];
}

// ------------------------------------------------------------------ users

function userActions(u) {
  const self = u.id === me.id;
  const resetPassword = async () => {
    const password = await askPassword({ title: "New password for " + u.username, username: u.username, fresh: true });
    if (!password) return;
    await api("PUT", "/users/" + u.id, { password });
    toast("Password changed. They must log in again.", false);
  };
  const limitGroups = async () => {
    const picked = await pickGroupsDialog(u);
    if (picked === null) return;
    await api("PUT", "/users/" + u.id, { scope: picked.length ? picked : null });
    await refresh();
    toast(picked.length ? u.username + " is limited to " + picked.join(", ") : u.username + " can use every group", false);
  };
  const toggleAdmin = async () => {
    await api("PUT", "/users/" + u.id, { is_admin: !u.is_admin });
    await refresh();
  };
  const remove = async () => {
    if (!(await confirmDialog({ title: "Delete user “" + u.username + "”?", body: "Their tokens stop working.", action: "Delete user" }))) return;
    await api("DELETE", "/users/" + u.id);
    await refresh();
    toast("User deleted", false);
  };
  return el("div", { class: "acts" },
    el("button", { type: "button", class: "btn", "data-k": "pw:" + u.id, onclick: () => guarded(resetPassword) }, "Reset password"),
    el("button", { type: "button", class: "btn", "data-k": "role:" + u.id, onclick: () => guarded(toggleAdmin) }, u.is_admin ? "Make regular user" : "Make admin"),
    u.is_admin ? "" : el("button", { type: "button", class: "btn", "data-k": "scope:" + u.id, onclick: () => guarded(limitGroups) }, "Limit groups"),
    self ? "" : el("button", { type: "button", class: "btn btn-danger", "data-k": "del:" + u.id, onclick: () => guarded(remove) }, "Delete"));
}

function users() {
  const name = el("input", { type: "text", autocomplete: "off", autocapitalize: "none", spellcheck: false });
  const pw = el("input", { type: "password", autocomplete: "new-password" });
  const nameField = field("Username", name, { cls: "grow" });
  const pwField = field("Password", pw, { cls: "grow", hint: "At least 12 characters." });
  const [adminLabel, admin] = checkbox("Admin", {});
  const create = () => guarded(async () => {
    nameField.setError(name.value.trim() ? "" : "Enter a username.");
    pwField.setError(pw.value.length >= 12 ? "" : "Use at least 12 characters.");
    if (!name.value.trim() || pw.value.length < 12) return;
    await api("POST", "/users", { username: name.value.trim(), password: pw.value, is_admin: admin.checked });
    await refresh();
    toast("User created", false);
  });
  pw.addEventListener("keydown", (e) => { if (isEnter(e)) create(); });
  return [
    el("header", { class: "page-head" }, el("h1", {}, "Users"), el("p", {}, "Admins manage everything. Other users can only control lights and manage their own tokens.")),
    el("div", { class: "stack" },
      el("section", { class: "card" }, el("h2", {}, "People"),
        el("ul", { class: "rows" }, data.users.map((u) => el("li", {},
          el("div", {},
            el("div", { class: "name" }, u.username, u.id === me.id ? el("span", { class: "badge" }, "you") : "", u.is_admin ? el("span", { class: "badge role" }, "admin") : ""),
            el("div", { class: "meta" }, (u.is_admin ? "Manages everything" : u.scope ? "Groups: " + u.scope.join(", ") : "All groups") + ". Created " + fmtDate(u.created))),
          userActions(u))))),
      el("section", { class: "card" }, el("h2", {}, "Add a user"),
        el("div", { class: "formgrid" },
          el("div", { class: "row row-top" }, nameField, pwField),
          adminLabel,
          el("div", {}, el("button", { type: "button", class: "btn btn-primary", "data-k": "user:add", onclick: create }, icon("plus"), "Add user"))))),
  ];
}

// --------------------------------------------------------------- settings

function hueCard() {
  const { paired, running } = data.hue;
  const ip = el("input", { type: "text", value: data.hue.ip || "", placeholder: "192.168.1.2", inputMode: "decimal", autocomplete: "off" });
  const ipField = field("Bridge IP address", ip, { cls: "grow", hint: "Press the link button on the bridge first, then pair within 30 seconds." });
  const button = el("button", { type: "button", class: "btn btn-primary", "data-k": "hue:pair" }, paired ? "Pair again" : "Pair");
  button.addEventListener("click", () => guarded(async () => {
    ipField.setError(ip.value.trim() ? "" : "Enter the bridge's IP address, like 192.168.1.2.");
    if (!ip.value.trim()) return ip.focus();
    button.disabled = true;
    button.textContent = "Waiting for the bridge";
    try {
      await api("POST", "/hue/pair", { ip: ip.value });
    } catch (e) {
      button.disabled = false;
      button.textContent = paired ? "Pair again" : "Pair";
      throw e;
    }
    await refresh();
    toast("Paired", false);
  }));
  return el("section", { class: "card" }, el("h2", {}, "Hue Bridge"),
    el("p", { class: "bridge" },
      el("span", { class: "dot" + (!paired ? " off" : running ? "" : " bad") }),
      !paired ? "Not paired." : el("span", {}, running ? "Paired and connected at " : "Paired, but not answering at ", el("code", {}, data.hue.ip), ".")),
    paired && !running ? el("div", { class: "notice bad" }, icon("alert"), "Check the bridge's power and network cable, or pair again if its address changed.") : "",
    el("div", { class: "row row-top" }, ipField, el("div", { class: "label-gap" }, button)));
}

/** Home Assistant Companion apps that registered with this server. */
function appsCard() {
  const remove = async (a) => {
    if (!(await confirmDialog({ title: "Remove “" + a.device_name + "”?", body: "The app is logged out and has to register again.", action: "Remove app" }))) return;
    await api("DELETE", "/apps/" + a.id);
    await refresh();
    toast("App removed", false);
  };
  return el("section", { class: "card" }, el("h2", {}, "Companion apps"),
    el("p", { class: "card-note" }, "Official Home Assistant apps connected to domus. Removing one also logs that app out."),
    data.apps.length ? el("ul", { class: "rows" }, data.apps.map((a) => el("li", {},
      el("div", {}, el("div", { class: "name" }, a.device_name),
        el("div", { class: "meta" }, [a.app_name, a.app_version].join(" ") + " on " + [a.manufacturer, a.model].join(" ") + (me.is_admin ? ". Owner " + (a.username || "unknown") : "")),
        el("div", { class: "meta", title: fmtDateTime(a.last_seen) }, "Last seen " + ago(a.last_seen) + ". Registered " + fmtDate(a.created))),
      el("div", { class: "acts" }, el("button", { type: "button", class: "btn btn-danger", "data-k": "app:" + a.id, onclick: () => guarded(() => remove(a)) }, "Remove")))))
      : el("div", { class: "empty" }, el("b", {}, "No app has registered yet"), el("span", {}, "Add this server in the Home Assistant app and it appears here.")));
}

// "Mozilla/5.0 (Macintosh; ...) ... Firefox/130.0" -> "Firefox on macOS"
function describeAgent(ua) {
  if (!ua) return "Unknown device";
  const browser = [["Edg/", "Edge"], ["OPR/", "Opera"], ["Firefox/", "Firefox"], ["Chrome/", "Chrome"], ["Safari/", "Safari"], ["curl/", "curl"]]
    .find(([k]) => ua.includes(k));
  const os = [["iPhone", "iOS"], ["iPad", "iPadOS"], ["Android", "Android"], ["Windows", "Windows"], ["Mac OS X", "macOS"], ["Linux", "Linux"]]
    .find(([k]) => ua.includes(k));
  if (!browser && !os) return ua.slice(0, 40);
  return (browser ? browser[1] : "Browser") + (os ? " on " + os[1] : "");
}

function sessionRows() {
  return el("ul", { class: "rows" }, data.sessions.map((x) => el("li", {},
    el("div", {}, el("div", { class: "name" }, describeAgent(x.user_agent), x.current ? el("span", { class: "badge" }, "this device") : ""),
      el("div", { class: "meta", title: fmtDateTime(x.last_seen) }, "Last active " + ago(x.last_seen) + ". Signed in " + fmtDate(x.created))),
    x.current ? "" : el("div", { class: "acts" }, el("button", {
      type: "button", class: "btn btn-danger", "data-k": "session:" + x.id,
      onclick: () => guarded(async () => {
        await api("DELETE", "/sessions/" + x.id);
        await refresh();
      }),
    }, "Sign out")))));
}

function accountCard() {
  const current = el("input", { type: "password", autocomplete: "current-password" });
  const next = el("input", { type: "password", autocomplete: "new-password" });
  const currentField = field("Current password", current, { cls: "grow" });
  const nextField = field("New password", next, { cls: "grow", hint: "At least 12 characters." });
  const change = () => guarded(async () => {
    currentField.setError(current.value ? "" : "Enter your current password.");
    nextField.setError(next.value.length >= 12 ? "" : "Use at least 12 characters.");
    if (!current.value || next.value.length < 12) return;
    await api("POST", "/password", { current: current.value, new: next.value });
    current.value = next.value = "";
    toast("Password changed", false);
  });
  next.addEventListener("keydown", (e) => { if (isEnter(e)) change(); });
  const others = data.sessions.filter((x) => !x.current).length;
  const signOutOthers = async () => {
    if (!(await confirmDialog({ title: "Sign out of " + others + " other " + (others === 1 ? "device" : "devices") + "?", action: "Sign out" }))) return;
    await api("POST", "/sessions/revoke-others");
    await refresh();
    toast("Signed out of other devices", false);
  };
  return el("section", { class: "card" }, el("h2", {}, "Account"),
    el("p", { class: "signed-in" }, "Signed in as ", el("b", {}, me.username), me.is_admin ? " " : "", me.is_admin ? el("span", { class: "badge role" }, "admin") : ""),
    el("div", { class: "row row-top" }, currentField, nextField,
      el("div", { class: "label-gap" }, el("button", { type: "button", class: "btn", "data-k": "pw:change", onclick: change }, "Change password"))),
    el("h3", { class: "sessions-head" }, "Where you are signed in"),
    sessionRows(),
    el("div", { class: "row row-center sessions-foot" },
      others ? el("button", { type: "button", class: "btn", "data-k": "session:others", onclick: () => guarded(signOutOthers) }, "Sign out other devices") : "",
      el("span", { class: "muted small" }, "Sessions end after 24 hours without activity, or 7 days at most.")));
}

function settings() {
  return [
    el("header", { class: "page-head" }, el("h1", {}, "Settings"), el("p", {}, me.is_admin ? "Hue Bridge, connected apps and your account." : "Connected apps and your account.")),
    el("div", { class: "stack" }, me.is_admin ? hueCard() : "", appsCard(), accountCard()),
  ];
}

// ------------------------------------------------------------------ start

const focusables = () => [...root.querySelectorAll("a[href], button, input, select, summary, [tabindex='0']")];

/** Rebuilds the page, then puts back the focus and the sideways scroll of the scene rows, so a live update does not take them away. */
function render() {
  const active = document.activeElement;
  const tracked = active && root.contains(active);
  const key = tracked ? active.dataset.k : null;
  const index = tracked ? focusables().indexOf(active) : -1;
  const tag = tracked ? active.tagName : null;
  const sceneScroll = [...root.querySelectorAll(".scenes")].map((n) => n.scrollLeft);

  const views = { dashboard, groups, devices, tokens, users, settings };
  shell().replaceChildren(...views[ui.tab]().flat(Infinity).filter(Boolean));
  root.querySelectorAll(".scenes").forEach((n, i) => { n.scrollLeft = sceneScroll[i] ?? 0; });

  const byKey = key && document.querySelector(`[data-k="${CSS.escape(key)}"]`);
  if (byKey) return byKey.focus({ preventScroll: true });
  const byIndex = index >= 0 ? focusables()[index] : null;
  if (byIndex && byIndex.tagName === tag) byIndex.focus({ preventScroll: true });
}

async function refresh() {
  await load();
  render();
}

function tabFromHash() {
  const t = location.hash.slice(1);
  if (visibleTabs().some(([id]) => id === t)) ui.tab = t;
}

window.addEventListener("hashchange", () => {
  if (!data) return;
  const before = ui.tab;
  tabFromHash();
  render();
  if (ui.tab !== before) window.scrollTo(0, 0);
});

// ------------------------------------------------------------ live updates

// The server pushes the id of every light or scene that changes; we just reload the list
// (debounced) and redraw only the views that show it, so forms being edited are left alone.
let events = null;
let liveTimer = null;

function stopLive() {
  if (events) events.close();
  events = null;
  clearTimeout(liveTimer);
}

async function syncLights() {
  if (!data) return;
  try {
    data.entities = await api("GET", "/lights");
  } catch {
    return;
  }
  if (ui.dragging) return;
  if (ui.tab === "dashboard" || ui.tab === "devices") render();
}

function startLive() {
  stopLive();
  const es = events = new EventSource("/api/domus/events");
  const reload = () => {
    clearTimeout(liveTimer);
    liveTimer = setTimeout(syncLights, 300);
  };
  es.addEventListener("change", reload);
  es.onopen = reload; // also catches up on whatever was missed while disconnected
  es.onerror = () => {
    // The browser retries by itself unless the server refused (session ended): then re-check.
    if (es.readyState === EventSource.CLOSED) setTimeout(() => { if (events === es) start().catch(() => {}); }, 5000);
  };
}

async function start() {
  stopLive();
  const s = await api("GET", "/status");
  if (!s.setup_done) return authScreen(false, s.setup_code_required);
  if (!s.logged_in) return authScreen(true);
  me = s.user;
  version = s.version || "";
  if (!visibleTabs().some(([id]) => id === ui.tab)) ui.tab = "dashboard";
  tabFromHash();
  await load();
  render();
  startLive();
}

start().catch((e) => errorScreen(e.message));

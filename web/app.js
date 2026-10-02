"use strict";
// All dynamic content goes through textContent / createTextNode; never innerHTML.

const root = document.getElementById("root");
const toastBox = document.getElementById("toast");

const SVG_NS = "http://www.w3.org/2000/svg";
// 24x24 stroke icons (paths only), drawn in currentColor.
const circle = (cx, cy, r) => `M${cx - r} ${cy}a${r} ${r} 0 1 0 ${2 * r} 0a${r} ${r} 0 1 0 ${-2 * r} 0`;
const ICONS = {
  home: ["M3 11l9-8 9 8", "M5 10v10h14V10", "M10 20v-6h4v6"],
  dashboard: ["M3 3h7v9H3z", "M14 3h7v5h-7z", "M14 12h7v9h-7z", "M3 16h7v5H3z"],
  folder: ["M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
  bulb: ["M9 18h6", "M10 21h4", "M12 3a6 6 0 0 0-4 10.5c.7.7 1 1.5 1 2.5h6c0-1 .3-1.8 1-2.5A6 6 0 0 0 12 3z"],
  key: [circle(8, 15, 4), "M10.8 12.2L20 3", "M16 7l3 3", "M14 9l2 2"],
  users: ["M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2", circle(9, 7, 4), "M22 21v-2a4 4 0 0 0-3-3.9", "M16 3.1a4 4 0 0 1 0 7.8"],
  settings: ["M4 6h6", "M14 6h6", "M4 12h12", "M20 12h0", "M4 18h2", "M10 18h10", circle(12, 6, 2), circle(18, 12, 2), circle(8, 18, 2)],
  logout: ["M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4", "M16 17l5-5-5-5", "M21 12H9"],
  plus: ["M12 5v14", "M5 12h14"],
  alert: ["M12 3l10 18H2z", "M12 10v5", "M12 18h0"],
  lock: ["M5 11h14v10H5z", "M8 11V7a4 4 0 0 1 8 0v4"],
  grip: ["M9 6h0", "M15 6h0", "M9 12h0", "M15 12h0", "M9 18h0", "M15 18h0"],
  up: ["M12 19V5", "M5 12l7-7 7 7"],
  down: ["M12 5v14", "M19 12l-7 7-7-7"],
  play: ["M7 4l13 8-13 8z"],
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

function el(tag, props, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e[k] = v;
  }
  for (const c of children) if (c !== null && c !== undefined && c !== false && c !== "") e.append(c);
  return e;
}

async function api(method, path, body) {
  const res = await fetch("/api/domus" + path, {
    method,
    headers: { "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  let data = null;
  try { data = await res.json(); } catch (_) { /* empty body */ }
  if (!res.ok) throw new Error((data && data.message) || "Request failed (" + res.status + ")");
  return data;
}

let toastTimer;
function toast(text, isError) {
  toastBox.textContent = text;
  toastBox.style.background = isError ? "var(--danger)" : "";
  toastBox.style.color = isError ? "#fff" : "";
  toastBox.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toastBox.classList.remove("show"), isError ? 4000 : 1800);
}

/** Runs fn and reports failures as a toast. */
async function guarded(fn) {
  try { await fn(); } catch (e) { toast(e.message, true); }
}

// navigator.clipboard only exists on https/localhost; domus is often opened over plain http.
async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text);
    return true;
  }
  const ta = el("textarea", { value: text });
  ta.style.cssText = "position:fixed;opacity:0";
  document.body.append(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch { /* fall through */ }
  ta.remove();
  return ok;
}

function copyButton(text, label) {
  return el("button", {
    class: label ? "" : "icon",
    onclick: async () => toast((await copyText(text)) ? "Copied" : "Select the text and copy it manually", false),
  }, label || "Copy");
}

const fmtDate = (secs) => new Date(secs * 1000).toLocaleDateString();
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
const visibleTabs = () => TABS.filter(([, , , adminOnly]) => !adminOnly || me.is_admin);

/** hue (admins only), entities (lights + scenes + group lights), groups, tokens, users (admins only) */
let data = null;
/** per-view state that survives re-renders */
const ui = { tab: "dashboard", draft: null, sub: "pick", fresh: null };

async function load() {
  const [hue, entities, groups, tokens, users] = await Promise.all([
    me.is_admin ? api("GET", "/hue") : null,
    api("GET", "/lights"), api("GET", "/groups"), api("GET", "/tokens"),
    me.is_admin ? api("GET", "/users") : [],
  ]);
  data = { hue, entities, groups, tokens, users };
}

const isScene = (e) => e.entity_id.startsWith("scene.");
const isGroupLight = (e) => e.entity_id.startsWith("light.domus_group_");
const realLights = () => data.entities.filter((e) => !isScene(e) && !isGroupLight(e));
const scenes = () => data.entities.filter(isScene);
const find = (id) => data.entities.find((e) => e.entity_id === id);
const label = (id) => { const e = find(id); return (e && e.name) || id; };

// ------------------------------------------------------------ auth screen

function authScreen(setupDone) {
  const user = el("input", { type: "text", placeholder: "Username", autocomplete: "username" });
  const pw = el("input", { type: "password", placeholder: "Password", autocomplete: setupDone ? "current-password" : "new-password" });
  const go = el("button", {
    class: "primary",
    onclick: () => guarded(async () => {
      await api("POST", setupDone ? "/login" : "/setup", { username: user.value, password: pw.value });
      await start();
    }),
  }, setupDone ? "Log in" : "Create admin");
  for (const input of [user, pw]) input.addEventListener("keydown", (e) => { if (e.key === "Enter") go.click(); });
  root.replaceChildren(el("div", { class: "auth" },
    el("h2", {}, "domus"),
    el("p", { class: "sub" }, setupDone ? "Log in to continue." : "First-time setup: choose the admin username and password (at least 8 characters)."),
    user, pw, go));
}

// ------------------------------------------------------------------ shell

async function logout() {
  await guarded(async () => {
    await api("POST", "/logout");
    ui.draft = null;
    await start();
  });
}

function shell() {
  const main = el("main", {});
  const nav = el("nav", {}, el("h1", {}, icon("home"), " domus"),
    ...visibleTabs().map(([id, ico, name]) => el("a", {
      class: "tab" + (id === ui.tab ? " active" : ""),
      href: "#" + id,
    }, icon(ico), name)),
    data.hue ? el("div", { class: "bridge muted" },
      el("span", { class: "dot" + (data.hue.paired && data.hue.running ? "" : " off") }),
      !data.hue.paired ? "Bridge not paired" : data.hue.running ? "Bridge connected" : "Bridge disconnected") : "",
    el("div", { class: "account" },
      el("span", { class: "who muted" }, me.username),
      el("button", { class: "logout", title: "Log out", onclick: logout }, icon("logout"), "Log out")));
  root.replaceChildren(el("div", { class: "app" }, nav, main));
  return main;
}

function render() {
  const main = shell();
  const views = { dashboard, groups, devices, tokens, users, settings };
  main.replaceChildren(...views[ui.tab]());
}

async function refresh() {
  await load();
  render();
}

// -------------------------------------------------------------- dashboard

function dashboard() {
  const lights = realLights();
  const first = data.groups[0];
  const stat = (title, value, note) => el("div", { class: "card stat" },
    el("span", { class: "muted" }, title), el("b", {}, value), el("span", { class: "muted" }, note));
  return [
    el("h2", {}, "Dashboard"),
    el("p", { class: "sub" }, "Overview of what the watch can see."),
    el("div", { class: "cards" },
      stat("Lights", String(lights.length), lights.filter((l) => l.state === "on").length + " on"),
      stat("Scenes", String(scenes().length), "from the Hue Bridge"),
      stat("Groups", String(data.groups.length), data.groups.filter((g) => g.expose_light).length + " with an all-lights switch"),
      data.hue ? stat("Hue Bridge", !data.hue.paired ? "Not paired" : data.hue.running ? "Connected" : "Disconnected",
        data.hue.paired ? data.hue.ip : "Pair it in Settings") : ""),
    el("div", { class: "card" },
      el("h3", {}, "Set up hasscontrol on the watch"),
      el("ol", { class: "steps" },
        el("li", {}, "Server URL: ", el("code", {}, location.origin), " ", copyButton(location.origin)),
        el("li", {}, "Access token: create one in ", el("a", { href: "#tokens" }, "Tokens"), "."),
        el("li", {}, "Group: ", first ? el("code", {}, first.name) : "create one in ", first ? copyButton(first.name) : el("a", { href: "#groups" }, "Groups"), first ? " (exact name)" : ""),
        el("li", {}, "After changing a group, run ", el("b", {}, "Refresh entities"), " on the watch.")),
      location.protocol === "https:" ? "" : el("p", { class: "muted" }, "hasscontrol needs an https:// URL with a trusted certificate; see the README for a reverse proxy.")),
  ];
}

// ----------------------------------------------------------------- groups

const GROUP_NAME = /^[A-Za-z0-9_-]{1,64}$/;

function newDraft(group) {
  return group
    ? { name: group.name, members: [...group.members], expose: group.expose_light, isNew: false, dirty: false }
    : { name: "", members: [], expose: false, isNew: true, dirty: false };
}

function groups() {
  if (!ui.draft || (!ui.draft.isNew && !data.groups.some((g) => g.name === ui.draft.name))) {
    ui.draft = data.groups.length ? newDraft(data.groups[0]) : newDraft(null);
  }
  const d = ui.draft;
  const select = (name) => { ui.draft = name === null ? newDraft(null) : newDraft(data.groups.find((g) => g.name === name)); render(); };

  const list = el("div", { class: "card glist" },
    ...data.groups.map((g) => el("button", {
      class: !d.isNew && g.name === d.name ? "active" : "",
      onclick: () => select(g.name),
    }, g.name + " ", el("span", { class: "pill" }, String(g.members.length)))),
    el("button", { class: d.isNew ? "active" : "", onclick: () => select(null) }, icon("plus"), " New group"));

  const gsel = el("select", { class: "gsel", onchange: (e) => select(e.target.value === "" ? null : e.target.value) },
    ...data.groups.map((g) => el("option", { value: g.name, selected: !d.isNew && g.name === d.name }, g.name)),
    el("option", { value: "", selected: d.isNew }, "New group"));

  return [
    el("h2", {}, "Groups"),
    el("p", { class: "sub" }, "The watch imports one group and shows its members in this order."),
    data.hue.paired ? "" : el("p", { class: "warn" }, "Pair the Hue Bridge in Settings first; there are no lights to pick yet."),
    el("div", { class: "split" }, list, el("div", {}, gsel, groupEditor(d))),
  ];
}

function groupEditor(d) {
  const nameInput = el("input", { type: "text", placeholder: "Group name (letters, digits, _ and -)", value: d.name, readOnly: !d.isNew });
  nameInput.addEventListener("input", () => { d.name = nameInput.value; markDirty(); });
  const exposeBox = el("input", { type: "checkbox", checked: d.expose });
  const lightId = () => "light.domus_group_" + (d.name || "<name>");
  const exposeCode = el("code", {}, lightId());
  const warn = el("div", {});
  const order = el("ul", { class: "members" });
  const watch = el("div", { class: "watch" });
  const pickTab = el("button", { onclick: () => { ui.sub = "pick"; syncSub(); } });
  const orderTab = el("button", { onclick: () => { ui.sub = "order"; syncSub(); } }, "Order & preview");
  const editor = el("div", { class: "editor" });
  const save = el("button", { class: "primary", onclick: () => guarded(() => saveGroup(d)) }, "Save group");
  const hint = el("span", { class: "muted hint" });

  function markDirty() {
    d.dirty = true;
    hint.textContent = "Unsaved changes";
    exposeCode.textContent = lightId();
  }
  function syncSub() {
    editor.dataset.sub = ui.sub;
    pickTab.className = ui.sub === "pick" ? "active" : "";
    orderTab.className = ui.sub === "order" ? "active" : "";
  }
  function move(id, delta) {
    const i = d.members.indexOf(id), j = i + delta;
    if (j < 0 || j >= d.members.length) return;
    [d.members[i], d.members[j]] = [d.members[j], d.members[i]];
    markDirty(); derived();
  }
  function derived() {
    const count = d.members.length + (d.expose ? 1 : 0);
    pickTab.textContent = "Pick (" + d.members.length + ")";
    warn.replaceChildren(count > 12
      ? el("div", { class: "warn" }, icon("alert"), " " + count + " entries; older watches may fail to load more than about a dozen.")
      : "");
    let dragId = null;
    order.replaceChildren(
      d.expose ? el("li", { class: "fixed" }, el("span", {}, icon("lock")), el("span", { class: "grow" }, "All lights"), el("span", { class: "pill" }, "first")) : "",
      ...d.members.map((id) => {
        const li = el("li", { draggable: true },
          el("span", { class: "handle" }, icon("grip")),
          el("span", { class: "grow" }, label(id)),
          el("button", { class: "icon", onclick: () => move(id, -1), title: "Move up" }, icon("up")),
          el("button", { class: "icon", onclick: () => move(id, 1), title: "Move down" }, icon("down")));
        li.addEventListener("dragstart", () => { dragId = id; li.classList.add("drag"); });
        li.addEventListener("dragend", () => li.classList.remove("drag"));
        li.addEventListener("dragover", (e) => e.preventDefault());
        li.addEventListener("drop", () => {
          if (!dragId || dragId === id) return;
          const rest = d.members.filter((x) => x !== dragId);
          rest.splice(rest.indexOf(id), 0, dragId);
          d.members = rest; markDirty(); derived();
        });
        return li;
      }));
    const anyOn = d.members.some((id) => { const e = find(id); return e && !isScene(e) && e.state === "on"; });
    watch.replaceChildren(
      el("div", { class: "w-title" }, d.name || "New group"),
      d.expose ? el("div", { class: "w-item" }, el("span", {}, "All lights"), el("span", { class: anyOn ? "w-on" : "" }, anyOn ? "On" : "Off")) : "",
      ...d.members.map((id) => {
        const e = find(id);
        const scene = e && isScene(e);
        const on = e && !scene && e.state === "on";
        return el("div", { class: "w-item" }, el("span", {}, label(id)), el("span", { class: on ? "w-on" : "" }, scene ? icon("play") : on ? "On" : "Off"));
      }));
  }
  exposeBox.addEventListener("change", () => { d.expose = exposeBox.checked; markDirty(); derived(); });

  // picker: lights, scenes by room, then other groups' all-lights switches
  const pickRow = (e) => {
    const cb = el("input", { type: "checkbox", checked: d.members.includes(e.entity_id) });
    cb.addEventListener("change", () => {
      d.members = cb.checked ? [...d.members, e.entity_id] : d.members.filter((x) => x !== e.entity_id);
      markDirty(); derived();
    });
    const row = el("label", { class: "pick" }, cb, e.name || e.entity_id);
    row.dataset.name = (e.name || e.entity_id).toLowerCase();
    return row;
  };
  const picks = el("div", {});
  const search = el("input", { type: "search", placeholder: "Search lights and scenes…" });
  search.addEventListener("input", () => {
    const q = search.value.toLowerCase();
    picks.querySelectorAll(".pick").forEach((p) => { p.style.display = p.dataset.name.includes(q) ? "" : "none"; });
  });
  const own = "light.domus_group_" + d.name;
  const section = (title, rows) => rows.length ? [el("div", { class: "section-title" }, title), ...rows] : [];
  const byRoom = new Map();
  for (const s of scenes()) {
    const room = (s.name || "").includes(": ") ? s.name.split(": ")[0] : "Other";
    if (!byRoom.has(room)) byRoom.set(room, []);
    byRoom.get(room).push(s);
  }
  picks.replaceChildren(
    ...section("Lights", realLights().map(pickRow)),
    ...[...byRoom].flatMap(([room, items]) => section("Scenes · " + room, items.map(pickRow))),
    ...section("Group switches", data.entities.filter((e) => isGroupLight(e) && e.entity_id !== own).map(pickRow)));
  if (!picks.children.length) picks.append(el("p", { class: "muted" }, "No lights or scenes yet."));

  editor.append(
    el("div", { class: "pane-pick" }, el("div", { class: "section-title" }, "Pick members"), search, picks),
    el("div", { class: "pane-order" },
      el("div", { class: "section-title" }, "Order (drag or use arrows)"), order,
      el("details", { class: "wp", open: window.innerWidth > 760 }, el("summary", {}, "Watch preview"), watch)));
  derived(); syncSub();

  const buttons = [save, hint];
  return el("div", { class: "card" },
    el("div", { class: "row" },
      el("div", { class: "grow" }, nameInput),
      d.isNew ? "" : copyButton(d.name, "Copy name"),
      d.isNew ? "" : el("button", { class: "danger", onclick: () => guarded(() => deleteGroup(d)) }, "Delete")),
    el("label", { class: "row check", style: "margin:12px 0" }, exposeBox, "Add an all-lights switch ", exposeCode),
    warn,
    el("div", { class: "subtabs" }, pickTab, orderTab),
    editor,
    el("div", { class: "savebar" }, ...buttons));
}

async function saveGroup(d) {
  const name = d.name.trim();
  if (!GROUP_NAME.test(name)) throw new Error("Group name: letters, digits, _ and - only (max 64).");
  await api("PUT", "/groups/" + encodeURIComponent(name), { members: d.members, expose_light: d.expose });
  ui.draft = { ...d, name, isNew: false, dirty: false };
  await refresh();
  toast("Group “" + name + "” saved. Run Refresh entities on the watch.", false);
}

async function deleteGroup(d) {
  if (!confirm("Delete group “" + d.name + "”?")) return;
  await api("DELETE", "/groups/" + encodeURIComponent(d.name));
  ui.draft = null;
  await refresh();
  toast("Group deleted", false);
}

// ---------------------------------------------------------------- devices

function devices() {
  const lightRows = realLights().map((l) => {
    const on = l.state === "on";
    const pct = typeof l.brightness === "number" ? Math.round(l.brightness / 255 * 100) : null;
    const sw = el("button", {
      class: "switch act" + (on ? " on" : ""),
      title: on ? "Turn off" : "Turn on",
      onclick: () => guarded(async () => {
        const r = await api("POST", "/devices/test", { entity_id: l.entity_id, on: !on });
        l.state = r.state || (on ? "off" : "on");
        if (on) l.brightness = null;
        render();
      }),
    });
    return el("li", {},
      el("span", { class: "name" }, l.name || l.entity_id),
      sw,
      el("span", { class: "meta" }, el("code", {}, l.entity_id)),
      el("span", { class: "meta" }, on ? (pct === null ? "On" : "Brightness " + pct + "%") : "Off"));
  });
  const sceneRows = scenes().map((s) => el("li", {},
    el("span", { class: "name" }, s.name || s.entity_id),
    el("button", {
      class: "act",
      onclick: () => guarded(async () => {
        const r = await api("POST", "/devices/test", { entity_id: s.entity_id });
        s.state = r.state || s.state;
        toast("Activated " + (s.name || s.entity_id), false);
        render();
      }),
    }, "Activate"),
    el("span", { class: "meta" }, el("code", {}, s.entity_id)),
    el("span", { class: "meta" }, "Last activated: " + (s.state === "unknown" ? "never" : new Date(s.state).toLocaleString()))));
  return [
    el("h2", {}, "Devices"),
    el("p", { class: "sub" }, "Test your Hue setup without the watch."),
    el("div", { class: "card" }, el("div", { class: "section-title" }, "Lights"),
      lightRows.length ? el("ul", { class: "rows" }, ...lightRows) : el("p", { class: "muted" }, "No lights yet. Pair the Hue Bridge in Settings.")),
    el("div", { class: "card" }, el("div", { class: "section-title" }, "Scenes"),
      sceneRows.length ? el("ul", { class: "rows" }, ...sceneRows) : el("p", { class: "muted" }, "No scenes found.")),
  ];
}

// ----------------------------------------------------------------- tokens

function tokens() {
  const name = el("input", { type: "text", placeholder: "Token name, e.g. Garmin watch" });
  const boxes = data.groups.map((g) => ({ g, box: el("input", { type: "checkbox" }) }));
  const create = () => guarded(async () => {
    const picked = boxes.filter((b) => b.box.checked).map((b) => b.g.name);
    const t = await api("POST", "/tokens", { name: name.value, scope: picked.length ? picked : null });
    ui.fresh = t.token;
    await refresh();
  });
  const picker = boxes.length && el("div", { class: "scope" },
    el("p", { class: "muted" }, "Limit to groups (leave all unchecked for full access):"),
    ...boxes.map((b) => el("label", {}, b.box, " " + b.g.name)));
  name.addEventListener("keydown", (e) => { if (e.key === "Enter") create(); });
  const fresh = ui.fresh && el("div", { class: "warn" },
    "Copy this token now; it is shown only once. Click it to select all.",
    el("code", { class: "tok" }, ui.fresh),
    copyButton(ui.fresh, "Copy token"));
  return [
    el("h2", {}, "Tokens"),
    el("p", { class: "sub" }, "Long-lived access tokens. Only a hash is stored."),
    el("div", { class: "card" },
      el("div", { class: "row" }, el("div", { class: "grow" }, name), el("button", { class: "primary", onclick: create }, "Create token")),
      picker,
      fresh,
      data.tokens.length
        ? el("ul", { class: "rows" }, ...data.tokens.map((t) => el("li", {},
          el("span", { class: "name" }, t.name),
          el("button", {
            class: "danger act",
            onclick: () => guarded(async () => {
              if (!confirm("Revoke token “" + t.name + "”? Clients using it stop working.")) return;
              await api("DELETE", "/tokens/" + t.id);
              await refresh();
            }),
          }, "Revoke"),
          me.is_admin ? el("span", { class: "meta" }, "Owner " + (t.username || "unknown")) : "",
          el("span", { class: "meta" }, t.scope ? "Groups: " + t.scope.join(", ") : "Full access"),
          el("span", { class: "meta" }, "Created " + fmtDate(t.created)),
          el("span", { class: "meta" }, "Last used " + ago(t.last_used)))))
        : el("p", { class: "muted" }, "No tokens yet.")),
  ];
}

// ------------------------------------------------------------------ users

function users() {
  const name = el("input", { type: "text", placeholder: "Username", autocomplete: "off" });
  const pw = el("input", { type: "password", placeholder: "Password (at least 8 characters)", autocomplete: "new-password" });
  const admin = el("input", { type: "checkbox" });
  const create = () => guarded(async () => {
    await api("POST", "/users", { username: name.value, password: pw.value, is_admin: admin.checked });
    await refresh();
    toast("User created", false);
  });
  pw.addEventListener("keydown", (e) => { if (e.key === "Enter") create(); });
  return [
    el("h2", {}, "Users"),
    el("p", { class: "sub" }, "Admins manage everything; other users can only control lights and manage their own tokens."),
    el("div", { class: "card" },
      el("div", { class: "row" }, el("div", { class: "grow" }, name), el("div", { class: "grow" }, pw)),
      el("div", { class: "row check", style: "margin:12px 0" },
        el("label", {}, admin, " Admin"),
        el("button", { class: "primary", onclick: create }, "Add user")),
      el("ul", { class: "rows" }, ...data.users.map((u) => el("li", {},
        el("span", { class: "name" }, u.username + (u.id === me.id ? " (you)" : "")),
        u.id === me.id ? "" : el("button", {
          class: "danger act",
          onclick: () => guarded(async () => {
            if (!confirm("Delete user “" + u.username + "”? Their tokens stop working.")) return;
            await api("DELETE", "/users/" + u.id);
            await refresh();
          }),
        }, "Delete"),
        el("button", {
          class: "act",
          onclick: () => guarded(async () => {
            const password = prompt("New password for " + u.username + " (at least 8 characters):");
            if (!password) return;
            await api("PUT", "/users/" + u.id, { password });
            toast("Password changed; they must log in again.", false);
          }),
        }, "Reset password"),
        el("button", {
          class: "act",
          onclick: () => guarded(async () => {
            await api("PUT", "/users/" + u.id, { is_admin: !u.is_admin });
            await refresh();
          }),
        }, u.is_admin ? "Make regular user" : "Make admin"),
        el("span", { class: "meta" }, u.is_admin ? "Admin" : "User"),
        el("span", { class: "meta" }, "Created " + fmtDate(u.created)))))),
  ];
}

// --------------------------------------------------------------- settings

function hueCard() {
  const ip = el("input", { type: "text", placeholder: "Bridge IP, e.g. 192.168.1.2", value: data.hue.ip || "" });
  return el("div", { class: "card" },
    el("h3", {}, "Hue Bridge"),
    el("p", {}, data.hue.paired
      ? el("span", {}, el("span", { class: "dot" }), "Paired with ", el("code", {}, data.hue.ip), data.hue.running ? "" : " (not running)")
      : "Not paired. Press the link button on the bridge, then click Pair."),
    el("div", { class: "row" }, el("div", { class: "grow" }, ip),
      el("button", {
        class: "primary",
        onclick: () => guarded(async () => {
          await api("POST", "/hue/pair", { ip: ip.value });
          await refresh();
          toast("Paired", false);
        }),
      }, data.hue.paired ? "Re-pair" : "Pair")),
    el("p", { class: "muted" }, "Press the bridge's link button first."));
}

function accountCard() {
  const current = el("input", { type: "password", placeholder: "Current password", autocomplete: "current-password" });
  const next = el("input", { type: "password", placeholder: "New password (at least 8 characters)", autocomplete: "new-password" });
  const change = () => guarded(async () => {
    await api("POST", "/password", { current: current.value, new: next.value });
    current.value = next.value = "";
    toast("Password changed", false);
  });
  next.addEventListener("keydown", (e) => { if (e.key === "Enter") change(); });
  return el("div", { class: "card" },
    el("h3", {}, "Account"),
    el("p", {}, "Signed in as ", el("b", {}, me.username), me.is_admin ? " (admin)" : ""),
    el("div", { class: "row" }, el("div", { class: "grow" }, current), el("div", { class: "grow" }, next),
      el("button", { onclick: change }, "Change password")));
}

function settings() {
  return [
    el("h2", {}, "Settings"),
    el("p", { class: "sub" }, me.is_admin ? "Hue Bridge and account." : "Your account."),
    me.is_admin ? hueCard() : "",
    accountCard(),
  ];
}

// ------------------------------------------------------------------ start

function tabFromHash() {
  const t = location.hash.slice(1);
  if (visibleTabs().some(([id]) => id === t)) ui.tab = t;
}

window.addEventListener("hashchange", () => { if (data) { tabFromHash(); render(); } });

async function start() {
  const s = await api("GET", "/status");
  if (!s.setup_done) return authScreen(false);
  if (!s.logged_in) return authScreen(true);
  me = s.user;
  if (!visibleTabs().some(([id]) => id === ui.tab)) ui.tab = "dashboard";
  tabFromHash();
  await load();
  render();
}

start().catch((e) => root.replaceChildren(el("p", { class: "err" }, e.message)));

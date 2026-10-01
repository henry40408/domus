"use strict";
// All dynamic content goes through textContent / createTextNode; never innerHTML.

const app = document.getElementById("app");

function el(tag, props, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e[k] = v;
  }
  for (const c of children) e.append(c);
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

function flash(box, text, isError) {
  box.textContent = text;
  box.className = "msg " + (isError ? "err" : "ok");
}

async function guarded(box, fn) {
  try { await fn(); } catch (e) { flash(box, e.message, true); }
}

function render(...nodes) {
  app.replaceChildren(el("h1", {}, "domus"), ...nodes);
}

// ------------------------------------------------------------ auth screen

function authScreen(setupDone) {
  const pw = el("input", { type: "password", placeholder: "Password", autocomplete: setupDone ? "current-password" : "new-password" });
  const msg = el("p", { class: "msg" });
  const go = el("button", {
    onclick: () => guarded(msg, async () => {
      await api("POST", setupDone ? "/login" : "/setup", { password: pw.value });
      await start();
    }),
  }, setupDone ? "Log in" : "Set password");
  pw.addEventListener("keydown", (e) => { if (e.key === "Enter") go.click(); });
  render(
    el("section", {},
      el("h2", {}, setupDone ? "Log in" : "First-time setup"),
      el("p", { class: "muted" }, setupDone ? "" : "Choose the admin password (at least 8 characters)."),
      pw, go, msg),
  );
}

// -------------------------------------------------------------- dashboard

async function hueSection() {
  const info = await api("GET", "/hue");
  const ip = el("input", { type: "text", placeholder: "Bridge IP, e.g. 192.168.1.2", value: info.ip || "" });
  const msg = el("p", { class: "msg" });
  const btn = el("button", {
    onclick: () => guarded(msg, async () => {
      await api("POST", "/hue/pair", { ip: ip.value });
      flash(msg, "Paired.", false);
      await start();
    }),
  }, info.paired ? "Re-pair" : "Pair");
  return el("section", {},
    el("h2", {}, "Hue Bridge"),
    el("p", { class: "muted" }, info.paired ? "Paired with " + info.ip + "." : "Press the link button on the bridge, then click Pair."),
    ip, btn, msg);
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

async function tokenSection() {
  const tokens = await api("GET", "/tokens");
  const name = el("input", { type: "text", placeholder: "Token name, e.g. watch" });
  const msg = el("p", { class: "msg" });
  const shown = el("p", { class: "msg" });
  const list = el("ul", {}, ...tokens.map((t) =>
    el("li", {},
      el("span", {}, t.name + " "),
      el("button", {
        onclick: () => guarded(msg, async () => { await api("DELETE", "/tokens/" + t.id); await start(); }),
      }, "Revoke"))));
  const add = el("button", {
    onclick: () => guarded(msg, async () => {
      const t = await api("POST", "/tokens", { name: name.value });
      shown.className = "msg ok";
      const copied = el("span", {});
      shown.replaceChildren(
        "Copy this token now; it is shown only once. Click it to select all.",
        el("code", { class: "tok" }, t.token),
        el("button", {
          onclick: async () => { copied.textContent = (await copyText(t.token)) ? " Copied" : " Select the token and copy it manually"; },
        }, "Copy token"),
        copied);
      name.value = "";
    }),
  }, "Create token");
  return el("section", {},
    el("h2", {}, "Long-lived access tokens"),
    list, name, add, msg, shown);
}

async function groupSection() {
  const [groups, lights] = await Promise.all([api("GET", "/groups"), api("GET", "/lights")]);
  const msg = el("p", { class: "msg" });

  function editor(name, members, expose, lightId) {
    const nameInput = el("input", { type: "text", placeholder: "Group name", value: name || "" });
    if (name) nameInput.readOnly = true;
    const boxes = lights.map((l) => {
      const cb = el("input", { type: "checkbox", value: l.entity_id, checked: members.includes(l.entity_id) });
      return el("label", {}, cb, " " + (l.name || l.entity_id) + " (" + l.entity_id + ")");
    });
    const exposeBox = el("input", { type: "checkbox", checked: !!expose });
    const exposeLabel = el("label", {}, exposeBox,
      " Add an all-lights switch to this group" + (lightId ? " (" + lightId + ")" : " (light.domus_group_<name>)"));
    const save = el("button", {
      onclick: () => guarded(msg, async () => {
        const chosen = boxes.map((b) => b.firstChild).filter((c) => c.checked).map((c) => c.value);
        await api("PUT", "/groups/" + encodeURIComponent(nameInput.value), { members: chosen, expose_light: exposeBox.checked });
        await start();
      }),
    }, "Save");
    const parts = [nameInput, ...boxes, exposeLabel, save];
    if (name) {
      parts.push(el("button", {
        onclick: () => guarded(msg, async () => { await api("DELETE", "/groups/" + encodeURIComponent(name)); await start(); }),
      }, "Delete"));
    }
    return el("div", {}, ...parts);
  }

  const items = groups.map((g) => el("details", {}, el("summary", {}, g.name + " (" + g.members.length + ")"), editor(g.name, g.members, g.expose_light, g.light_entity_id)));
  return el("section", {},
    el("h2", {}, "Groups"),
    el("p", { class: "muted" }, "In the hasscontrol watch settings, enter the exact group name below. Keep groups small (about a dozen entries) so older watches can parse them. Members can be lights and Hue scenes."),
    lights.length === 0 ? el("p", { class: "muted" }, "No lights yet. Pair the Hue Bridge first.") : "",
    ...items,
    el("details", {}, el("summary", {}, "New group"), editor("", [], false, "")),
    msg);
}

async function dashboard() {
  const out = el("button", {
    onclick: async () => { await api("POST", "/logout"); await start(); },
  }, "Log out");
  render(await hueSection(), await groupSection(), await tokenSection(), out);
}

async function start() {
  const s = await api("GET", "/status");
  if (!s.setup_done) authScreen(false);
  else if (!s.logged_in) authScreen(true);
  else await dashboard();
}

start().catch((e) => render(el("p", { class: "err" }, e.message)));

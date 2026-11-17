// Bridge for the official Home Assistant Companion apps. They load the server's page with
// `?external_auth=1` and wait for it to report that it is connected; without that they show a
// connection error after a few seconds. Plain browsers never use it.
(() => {
  if (!new URLSearchParams(location.search).has("external_auth")) return;

  // The apps push commands (results, commands, ...) through this function; domus has no use for them.
  window.externalBus = () => {};

  const send = (message) => {
    const ios = window.webkit?.messageHandlers?.externalBus;
    if (ios) ios.postMessage(message); // iOS takes the object
    else if (window.externalApp?.externalBus) window.externalApp.externalBus(JSON.stringify(message)); // Android
  };
  send({ type: "connection-status", payload: { event: "connected" } });

  // Signs in with the app's own login, as the Home Assistant frontend does: the app hands over its
  // access token (`getExternalAuth`), the server trades it for a session cookie. The token only ever
  // goes to this same-origin path, in a header, and is not stored. Without this the page asks for
  // the password again whenever its own cookie has gone.
  const SEEN = "domus-autologin";
  const getToken = () => new Promise((resolve, reject) => {
    window.externalAuthSetToken = (ok, data) => (ok ? resolve(data) : reject(new Error("token unavailable")));
    const payload = { callback: "externalAuthSetToken" };
    const ios = window.webkit?.messageHandlers?.getExternalAuth;
    if (ios) ios.postMessage(payload);
    else if (window.externalApp?.getExternalAuth) window.externalApp.getExternalAuth(JSON.stringify(payload));
    else reject(new Error("no app bridge"));
    setTimeout(() => reject(new Error("timeout")), 10000);
  });
  let busy = false;
  const autoLogin = async () => {
    // One try per half minute, so a refused token cannot turn into a reload loop.
    if (busy || Date.now() - Number(sessionStorage.getItem(SEEN) || 0) < 30000) return;
    busy = true;
    try {
      const status = await (await fetch("/api/domus/status")).json();
      if (status.logged_in || !status.setup_done) return;
      sessionStorage.setItem(SEEN, String(Date.now()));
      const { access_token } = await getToken();
      const res = await fetch("/api/domus/session/from-token", {
        method: "POST",
        headers: { authorization: "Bearer " + access_token },
      });
      if (res.ok) location.reload();
    } catch (_) { /* the password login stays available */ } finally { busy = false; }
  };
  autoLogin();
  // The app ignores messages while it is in the background; try again when it comes back.
  document.addEventListener("visibilitychange", () => { if (!document.hidden) autoLogin(); });

  // The app is only the WebView; its own settings (server list, sensors, ...) are opened by the page
  // asking for them, as the Home Assistant frontend does from its sidebar. Without this entry the
  // user could not reach them, so it stays on screen on every page, including the login.
  const NS = "http://www.w3.org/2000/svg";
  const gear = document.createElementNS(NS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 24 24", fill: "none", stroke: "currentColor", "stroke-width": "2",
    "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true", class: "ico",
  })) gear.setAttribute(k, v);
  const hub = document.createElementNS(NS, "circle");
  for (const [k, v] of Object.entries({ cx: "12", cy: "12", r: "3" })) hub.setAttribute(k, v);
  const teeth = document.createElementNS(NS, "path");
  teeth.setAttribute("d", "M12 2v3M12 19v3M2 12h3M19 12h3M4.9 4.9l2.1 2.1M17 17l2.1 2.1M4.9 19.1L7 17M17 7l2.1-2.1");
  const ring = document.createElementNS(NS, "circle");
  for (const [k, v] of Object.entries({ cx: "12", cy: "12", r: "6.5" })) ring.setAttribute(k, v);
  gear.append(ring, hub, teeth);

  const settings = document.createElement("button");
  settings.type = "button";
  settings.className = "app-settings";
  settings.setAttribute("aria-label", "App settings");
  settings.title = "App settings";
  settings.append(gear);
  settings.addEventListener("click", () => send({ type: "config_screen/show" }));
  document.body.append(settings);
})();

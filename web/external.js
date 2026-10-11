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

  // The app is only the WebView; its own settings (server list, sensors, ...) are opened by the page
  // asking for them, as the Home Assistant frontend does from its sidebar. Without this entry the
  // user could not reach them, so it stays on screen on every page, including the login.
  const settings = document.createElement("button");
  settings.type = "button";
  settings.className = "app-settings";
  settings.textContent = "App settings";
  settings.addEventListener("click", () => send({ type: "config_screen/show" }));
  document.body.append(settings);
})();

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
})();

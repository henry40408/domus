// Login page of /auth/authorize: trades the credentials for a one-time code, then hands it to
// the app by navigating to its redirect URI (a plain form post could not leave `form-action 'self'`).
(function () {
  const q = new URLSearchParams(location.search);
  const form = document.getElementById("login");
  const error = document.getElementById("error");
  form.addEventListener("submit", async (ev) => {
    ev.preventDefault();
    error.textContent = "";
    const button = form.querySelector("button");
    button.disabled = true;
    try {
      const res = await fetch("/auth/login_flow", {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({
          username: document.getElementById("username").value,
          password: document.getElementById("password").value,
          client_id: q.get("client_id") || "",
          redirect_uri: q.get("redirect_uri") || "",
        }),
      });
      const body = await res.json().catch(() => ({}));
      if (!res.ok || !body.code) {
        error.textContent = body.message || "Sign in failed.";
        return;
      }
      const target = new URL(q.get("redirect_uri"));
      target.searchParams.set("code", body.code);
      if (q.get("state")) target.searchParams.set("state", q.get("state"));
      location.href = target.toString();
    } catch (e) {
      error.textContent = "Could not reach domus.";
    } finally {
      button.disabled = false;
    }
  });
})();

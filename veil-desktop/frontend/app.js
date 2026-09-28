"use strict";
const $ = (id) => document.getElementById(id);
const api = () => window.go.main.App;
let language =
  localStorage.getItem("language") ||
  (navigator.language.startsWith("zh") ? "zh" : "en");
if (!messages[language]) language = "en";
let theme =
  localStorage.getItem("theme") ||
  (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
let status = { state: "stopped", stats: {} },
  savedRevision = "",
  dirty = false,
  busy = false,
  epoch = 0;
const t = (key) => messages[language][key];
let systemProxy = { mode: "keep", supported: false, managed: false };

function confirmAction(
  title,
  message,
  accept,
  cancel = t("cancel"),
  detail = "",
) {
  const dialog = $("confirmation");
  if (dialog.open) return Promise.resolve(false);
  $("confirmation-title").textContent = title;
  $("confirmation-message").textContent = message;
  $("confirmation-detail").textContent = detail;
  $("confirmation-detail").hidden = !detail;
  $("confirmation-accept").textContent = accept;
  $("confirmation-cancel").textContent = cancel;
  dialog.returnValue = "cancel";
  dialog.showModal();
  $("confirmation-cancel").focus();
  return new Promise((resolve) => {
    dialog.addEventListener(
      "close",
      () => resolve(dialog.returnValue === "confirm"),
      { once: true },
    );
  });
}
const confirmDiscard = () =>
  confirmAction(t("discardTitle"), t("discardMessage"), t("discard"));
function requestQuit() {
  return perform(async () => {
    if (
      await confirmAction(
        t("quitTitle"),
        t("quitMessage"),
        t("quit"),
        t("keepRunning"),
        dirty ? t("quitDirty") : "",
      )
    ) {
      await api().Quit();
    }
  });
}
const config = () => {
  let value;
  try {
    value = JSON.parse($("config").value);
  } catch {
    throw new Error(t("invalidJSON"));
  }
  if (!value || Array.isArray(value) || typeof value !== "object")
    throw new Error(t("invalidJSON"));
  return value;
};
function feedback(message, error = false) {
  $("feedback").textContent = message;
  $("feedback").classList.toggle("error", error);
  $("feedback").hidden = !message;
}
function render() {
  const running = status.state === "running",
    saved = !!savedRevision;
  $("state").classList.toggle("running", running);
  $("state-text").textContent = t(status.state) || status.state;
  $("endpoint").textContent = status.listen || "—";
  $("accepted").textContent = status.stats?.accepted || 0;
  $("completed").textContent =
    `${status.stats?.completed || 0} / ${status.stats?.failed || 0}`;
  $("toggle").textContent = t(running ? "disconnect" : "connect");
  $("toggle").disabled = busy || !saved;
  $("apply").hidden = !status.restart_required;
  $("draft-state").textContent = t(
    dirty ? "dirty" : saved ? "saved" : "noProfile",
  );
  $("draft-state").classList.toggle("dirty", dirty);
  $("connect-title").textContent = t(
    status.error
      ? "failedTitle"
      : status.restart_required
        ? "pendingTitle"
        : running
          ? "runningTitle"
          : "readyTitle",
  );
  $("connect-hint").textContent =
    status.error ||
    t(
      status.restart_required
        ? "pendingHint"
        : running
          ? "runningHint"
          : saved
            ? "savedHint"
            : "readyHint",
    );
  $("diagnostic-data").textContent = JSON.stringify(status, null, 2);
  $("proxy-mode").value = systemProxy.mode;
  $("proxy-mode").disabled = busy || !systemProxy.supported;
  $("proxy-clear").disabled = busy || !systemProxy.supported;
  $("proxy-hint").textContent =
    systemProxy.error ||
    t(
      !systemProxy.supported
        ? "proxyUnsupported"
        : systemProxy.managed
          ? "proxyManaged"
          : systemProxy.mode === "auto"
            ? "proxyAutoHint"
            : "proxyKeepHint",
    );
  for (const id of [
    "import",
    "reload",
    "validate",
    "save",
    "apply",
    "protocol",
    "listen",
    "config",
    "quit",
  ])
    $(id).disabled = busy;
  $("reload").disabled = busy || !saved;
  $("validate").disabled = $("save").disabled =
    busy || !$("config").value.trim();
}
function showProfile(value) {
  $("config").value = value ? JSON.stringify(value, null, 2) : "";
  syncFields();
}
function syncFields() {
  let c = null;
  try {
    c = config();
  } catch {
    /* Keep invalid drafts editable. */
  }
  const proxy = c?.role === "client" && !c.target;
  $("fields").hidden = !proxy;
  $("empty").hidden = !!c;
  if (proxy) {
    $("protocol").value = c.inbound || "socks";
    $("listen").value = c.listen || "";
  }
  $("profile-hint").textContent =
    c?.server || (c ? c.listen || "" : t("importHint"));
}
function applyLanguage() {
  document.documentElement.lang = language === "zh" ? "zh-CN" : "en";
  $("language").value = language;
  document.querySelectorAll("[data-i18n]").forEach((el) => {
    el.textContent = t(el.dataset.i18n);
  });
  $("theme").textContent = t(theme === "dark" ? "lightTheme" : "theme");
  syncFields();
  render();
}
async function request(action, extra = {}) {
  const r = await api().Request({ version: 1, action, ...extra });
  if (r.status) status = r.status;
  if (r.error)
    throw new Error(
      r.error.code === "conflict"
        ? t("conflict")
        : `${t("error")}: ${r.error.message}`,
    );
  return r;
}
async function load() {
  const r = await request("config");
  savedRevision = status.saved_revision || "";
  showProfile(r.config);
  dirty = false;
}
async function perform(fn) {
  if (busy) return;
  busy = true;
  epoch++;
  render();
  feedback("");
  try {
    await fn();
  } catch (error) {
    feedback(String(error.message || error), true);
  } finally {
    try {
      systemProxy = await api().SystemProxy();
    } catch {
      /* Keep the action's original error visible. */
    }
    busy = false;
    render();
  }
}
$("language").onchange = async () => {
  language = $("language").value;
  localStorage.setItem("language", language);
  applyLanguage();
  await api().SetLanguage(language);
  feedback("");
};
$("theme").onclick = () => {
  theme = theme === "dark" ? "light" : "dark";
  localStorage.setItem("theme", theme);
  document.documentElement.dataset.theme = theme;
  applyLanguage();
};
$("config").oninput = () => {
  dirty = true;
  syncFields();
  render();
};
for (const id of ["protocol", "listen"])
  $(id).oninput = () => {
    try {
      const c = config();
      c.inbound = $("protocol").value;
      c.listen = $("listen").value;
      $("config").value = JSON.stringify(c, null, 2);
      dirty = true;
      render();
    } catch (e) {
      feedback(e.message, true);
    }
  };
$("import").onclick = () =>
  perform(async () => {
    if (dirty && !(await confirmDiscard())) return;
    const content = await api().Import();
    if (!content) return;
    // Parse before replacing the draft so a malformed file cannot erase it.
    const value = JSON.parse(content);
    if (!value || typeof value !== "object" || Array.isArray(value))
      throw new Error(t("invalidJSON"));
    showProfile(value);
    dirty = true;
    feedback(t("imported"));
  });
$("reload").onclick = () =>
  perform(async () => {
    if (dirty && !(await confirmDiscard())) return;
    await load();
    feedback(t("reloaded"));
  });
$("validate").onclick = () =>
  perform(async () => {
    await request("validate", { config: config() });
    feedback(t("valid"));
  });
$("save").onclick = () =>
  perform(async () => {
    await request("save", {
      config: config(),
      expected_revision: savedRevision,
    });
    await load();
    feedback(t("savedFeedback"));
  });
$("toggle").onclick = () =>
  perform(async () => {
    const action = status.state === "running" ? "stop" : "start";
    await request(action, { expected_revision: savedRevision });
    feedback(t(action === "start" ? "started" : "stoppedFeedback"));
  });
$("apply").onclick = () =>
  perform(async () => {
    const revision = savedRevision;
    if (!(await confirmAction(t("applyTitle"), t("applyMessage"), t("apply"))))
      return;
    await request("restart", { expected_revision: revision });
    feedback(t("applied"));
  });
$("proxy-mode").onchange = () => {
  const mode = $("proxy-mode").value;
  perform(async () => {
    if (
      mode === "auto" &&
      !(await confirmAction(
        t("proxyAutoTitle"),
        t("proxyAutoMessage"),
        t("proxyAuto"),
      ))
    )
      return;
    await api().SetSystemProxyMode(mode);
    feedback(t("proxyUpdated"));
  });
};
$("proxy-clear").onclick = () =>
  perform(async () => {
    if (
      !(await confirmAction(
        t("proxyClearTitle"),
        t("proxyClearMessage"),
        t("proxyClear"),
      ))
    )
      return;
    await api().ClearSystemProxy();
    feedback(t("proxyCleared"));
  });
document.documentElement.dataset.theme = theme;
// Let the window manager account for decorations and panels in small workspaces.
if (
  window.outerWidth + 32 > screen.availWidth ||
  window.outerHeight + 80 > screen.availHeight
)
  window.runtime.WindowMaximise();
$("quit").onclick = requestQuit;
window.runtime.EventsOn("quit-requested", requestQuit);
applyLanguage();
(async () => {
  try {
    await api().SetLanguage(language);
    await perform(load);
  } catch {
    feedback(t("unavailable"), true);
  }
  setInterval(async () => {
    if (busy) return;
    const current = epoch;
    try {
      const r = await api().Request({ version: 1, action: "status" });
      if (current !== epoch || busy) return;
      if (r.error) throw new Error(r.error.message);
      const proxy = await api().SystemProxy();
      if (current !== epoch || busy) return;
      status = r.status;
      systemProxy = proxy;
      render();
    } catch {
      if (!busy) feedback(t("unavailable"), true);
    }
  }, 3000);
})();

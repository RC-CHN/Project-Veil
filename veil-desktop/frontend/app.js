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
const t = (key) => messages[language][key];
let busy = false,
  rows = [],
  connections,
  systemProxy = {
    mode: "keep",
    supported: false,
    managed: false,
    connection: "",
  };
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

function feedback(text, error = false) {
  $("feedback").textContent = text;
  $("feedback").hidden = !text;
  $("feedback").classList.toggle("error", error);
}
function render() {
  const active = rows.filter(
    (r) => r.kind === "connection" && r.state === "running",
  );
  $("state").classList.toggle("running", !!active.length);
  $("state-text").textContent = active.length
    ? `${t("running")} · ${active.length}`
    : t("stopped");
  const selected = rows.find((r) => r.id === systemProxy.connection);
  $("endpoint").textContent =
    (selected?.inlets || []).map((i) => i.listen).join(", ") || "—";
  $("accepted").textContent = active.reduce(
    (n, r) => n + (r.stats?.accepted || 0),
    0,
  );
  $("completed").textContent =
    `${active.reduce((n, r) => n + (r.stats?.completed || 0), 0)} / ${active.reduce((n, r) => n + (r.stats?.failed || 0), 0)}`;
  $("diagnostic-data").textContent = JSON.stringify(rows, null, 2);
  const select = $("proxy-connection");
  select.replaceChildren(new Option(t("chooseProxyConnection"), ""));
  for (const row of rows.filter((r) => r.kind === "connection"))
    select.add(new Option(row.name, row.id));
  select.value = systemProxy.connection || "";
  select.disabled = busy || !systemProxy.supported;
  $("proxy-mode").value = systemProxy.mode;
  $("proxy-mode").disabled = $("proxy-clear").disabled =
    busy || !systemProxy.supported;
  $("quit").disabled = busy;
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
}
async function updateProxy() {
  systemProxy = await api().SystemProxy();
  render();
}
async function perform(operation) {
  if (busy) return;
  busy = true;
  render();
  feedback("");
  try {
    await operation();
  } catch (error) {
    feedback(error.message || String(error), true);
  } finally {
    try {
      await updateProxy();
    } catch {}
    busy = false;
    render();
  }
}
function applyLanguage() {
  document.documentElement.lang = language === "zh" ? "zh-CN" : "en";
  $("language").value = language;
  document.querySelectorAll("[data-i18n]").forEach((n) => {
    n.textContent = t(n.dataset.i18n);
  });
  $("theme").textContent = t(theme === "dark" ? "lightTheme" : "theme");
  $("proxy-connection").setAttribute("aria-label", t("chooseProxyConnection"));
  connections?.setLanguage(language);
  render();
}
$("language").onchange = async () => {
  language = $("language").value;
  localStorage.setItem("language", language);
  applyLanguage();
  await api().SetLanguage(language);
};
$("theme").onclick = () => {
  theme = theme === "dark" ? "light" : "dark";
  localStorage.setItem("theme", theme);
  document.documentElement.dataset.theme = theme;
  applyLanguage();
};
$("proxy-connection").onchange = () => {
  const id = $("proxy-connection").value;
  perform(() => api().SetSystemProxyConnection(id));
};
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
const requestQuit = () =>
  perform(async () => {
    if (
      await confirmAction(
        t("quitTitle"),
        t("quitMessage"),
        t("quit"),
        t("keepRunning"),
        connections?.hasDraft() ? t("quitDirty") : "",
      )
    )
      await api().Quit();
  });
$("quit").onclick = requestQuit;
window.runtime.EventsOn("quit-requested", requestQuit);
document.documentElement.dataset.theme = theme;
if (
  window.outerWidth + 32 > screen.availWidth ||
  window.outerHeight + 80 > screen.availHeight
)
  window.runtime.WindowMaximise();
applyLanguage();
(async () => {
  try {
    await api().SetLanguage(language);
    try {
      await api().PrepareConnections();
    } catch (e) {
      feedback(String(e), true);
    }
    await updateProxy();
    connections = window.VeilConnections({
      root: $("connection-list"),
      language,
      request: async (q) => {
        const r = await api().Request(q);
        await updateProxy();
        return r;
      },
      importFile: () => api().Import(),
      exportFile: (id) => api().Export(id),
      onChange: (next) => {
        rows = next;
        render();
      },
    });
  } catch (e) {
    feedback(e.message || t("unavailable"), true);
  }
  setInterval(() => {
    if (!busy) updateProxy().catch(() => feedback(t("unavailable"), true));
  }, 3000);
})();

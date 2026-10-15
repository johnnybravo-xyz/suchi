// SPDX-License-Identifier: AGPL-3.0-or-later

const invoke = window.__TAURI__.core.invoke;
const title = document.querySelector("#title");
const message = document.querySelector("#message");
const details = document.querySelector("#details");
const setup = document.querySelector("#setup");
const token = document.querySelector("#token");
const actions = document.querySelector("#actions");
const retry = document.querySelector("#retry");
const openApp = document.querySelector("#open-app");
const stop = document.querySelector("#stop");
const rollback = document.querySelector("#rollback");

let lastState = "";

function render(state) {
  if (JSON.stringify(state) === lastState) return;
  lastState = JSON.stringify(state);
  title.textContent = state.title;
  message.textContent = state.message;
  details.hidden = !state.detail;
  details.textContent = state.detail || "";
  setup.hidden = !state.setupRequired;
  actions.hidden = !(state.canRetry || state.canOpen || state.canStop || state.canRollback);
  retry.hidden = !state.canRetry;
  openApp.hidden = !state.canOpen;
  stop.hidden = !state.canStop;
  rollback.hidden = !state.canRollback;
  rollback.textContent = state.rollbackLabel || "Roll back";
}

async function refresh() {
  try {
    render(await invoke("launcher_state"));
  } catch {
    render({
      title: "Launcher unavailable",
      message: "Suchi could not read its local process state.",
      detail: "Restart the desktop application. Logs are available from the tray menu.",
      setupRequired: false,
      canRetry: false,
      canOpen: false,
      canStop: false,
      canRollback: false,
      rollbackLabel: null,
    });
  }
}

document.querySelector("#reveal-token").addEventListener("click", async () => {
  token.textContent = await invoke("reveal_setup_token");
  token.hidden = false;
});
document.querySelector("#open-setup").addEventListener("click", () => invoke("open_setup"));
retry.addEventListener("click", () => invoke("retry_processing"));
openApp.addEventListener("click", () => invoke("open_application"));
stop.addEventListener("click", () => invoke("stop_processing"));
rollback.addEventListener("click", () => invoke("rollback_operation"));

await refresh();
window.setInterval(refresh, 500);

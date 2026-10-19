// SPDX-License-Identifier: AGPL-3.0-or-later

import { expect, test } from "bun:test";

function element() {
  return {
    hidden: false,
    textContent: "",
    listeners: new Map(),
    addEventListener(name, listener) {
      this.listeners.set(name, listener);
    },
  };
}

test("starts polling and renders state as a classic script", async () => {
  const selectors = [
    "#title",
    "#message",
    "#details",
    "#progress",
    "#setup",
    "#token",
    "#actions",
    "#retry",
    "#open-logs",
    "#open-app",
    "#stop",
    "#rollback",
    "#reveal-token",
    "#open-setup",
  ];
  const elements = new Map(selectors.map((selector) => [selector, element()]));
  const commands = [];
  let poll;
  const state = {
    title: "Archive ready",
    message: "Open Suchi.",
    detail: "Ready on loopback.",
    busy: true,
    setupRequired: true,
    canRetry: false,
    canOpen: true,
    canStop: true,
    canRollback: false,
    canOpenLogs: true,
    rollbackLabel: null,
  };

  globalThis.document = {
    querySelector(selector) {
      return elements.get(selector);
    },
  };
  globalThis.window = {
    __TAURI__: {
      core: {
        async invoke(command) {
          commands.push(command);
          if (command === "launcher_state") return state;
          throw new Error(`unexpected command: ${command}`);
        },
      },
    },
    setInterval(callback, milliseconds) {
      expect(milliseconds).toBe(500);
      poll = callback;
      return 1;
    },
  };

  try {
    const source = await Bun.file(new URL("./launcher.js", import.meta.url)).text();
    expect(() => new Function(source)()).not.toThrow();
    await Bun.sleep(0);

    expect(commands).toEqual(["launcher_state"]);
    expect(typeof poll).toBe("function");
    expect(elements.get("#title").textContent).toBe(state.title);
    expect(elements.get("#message").textContent).toBe(state.message);
    expect(elements.get("#details").hidden).toBe(false);
    expect(elements.get("#progress").hidden).toBe(false);
    expect(elements.get("#setup").hidden).toBe(false);
    expect(elements.get("#actions").hidden).toBe(false);
    expect(elements.get("#open-logs").hidden).toBe(false);
    expect(elements.get("#open-app").hidden).toBe(false);
    expect(elements.get("#stop").hidden).toBe(false);

    await poll();
    expect(commands).toEqual(["launcher_state", "launcher_state"]);
  } finally {
    delete globalThis.document;
    delete globalThis.window;
  }
});

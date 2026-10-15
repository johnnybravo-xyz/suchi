// SPDX-License-Identifier: AGPL-3.0-or-later

const host = "127.0.0.1";
const port = 1420;
const root = new URL("../launcher/", import.meta.url);
const assets = new Map([
  ["/", ["index.html", "text/html; charset=utf-8"]],
  ["/index.html", ["index.html", "text/html; charset=utf-8"]],
  ["/launcher.css", ["launcher.css", "text/css; charset=utf-8"]],
  ["/launcher.js", ["launcher.js", "text/javascript; charset=utf-8"]],
]);

Bun.serve({
  hostname: host,
  port,
  async fetch(request) {
    const url = new URL(request.url);
    if (url.hostname !== host || !["GET", "HEAD"].includes(request.method)) {
      return new Response("Not found", { status: 404 });
    }
    const asset = assets.get(url.pathname);
    if (!asset) return new Response("Not found", { status: 404 });

    const [name, contentType] = asset;
    const file = Bun.file(new URL(name, root));
    if (!(await file.exists())) return new Response("Not found", { status: 404 });
    console.log(`[launcher-dev] ${request.method} ${url.pathname}`);
    return new Response(request.method === "HEAD" ? null : file, {
      headers: {
        "Cache-Control": "no-store",
        "Content-Security-Policy": "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src ipc: http://ipc.localhost",
        "Content-Type": contentType,
        "X-Content-Type-Options": "nosniff",
      },
    });
  },
});

console.log(`[launcher-dev] serving http://${host}:${port}`);

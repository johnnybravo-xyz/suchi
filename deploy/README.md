# deploy/

Boilerplate for the four common self-host shapes.

Everything here is a starting point — edit the hostname, TLS paths,
and storage locations before you paste them into production.

| Directory | What it is |
| --------- | ---------- |
| [`compose.yaml`](../compose.yaml) | Minimal single-container Compose deployment. Start here when you want editable mounts, networks, or image pins. |
| [`systemd/`](systemd/suchi.service) | Unit file for a bare-binary install on a Linux host. Runs suchi as an unprivileged user with the usual defense-in-depth sandboxing. |
| [`caddy/`](caddy/Caddyfile) | Reverse-proxy snippet for Caddy. Auto-TLS via Let's Encrypt when the site block uses a real hostname. |
| [`nginx/`](nginx/suchi.conf) | Server block for nginx. Assumes certificates already exist at the paths shown — provision them however you already do. |
| [`traefik/`](traefik/) | Dynamic-config snippet for Traefik. Assumes an existing `websecure` entrypoint and cert resolver. |
| [`k8s/`](k8s/) | Single-replica Deployment + PVC + ClusterIP Service. **SQLite is single-writer** — do not scale replicas up. |
| [`mail-mbsync/`](mail-mbsync/) | The mail-intake sidecar reference deployment (Phase 2). Docker-compose flavor. |

All shapes assume the same two env vars are set on suchi:

- `DATA_DIR` — where the SQLite database, CAS blobs, and rendered
  views live. Must be writable by the suchi process.
- `LISTEN_ADDR` — usually `127.0.0.1:8000` behind a reverse proxy, or
  `0.0.0.0:8000` inside a container.

Full env-var and config-file reference: [docs/config.mdx](../docs/config.mdx).

## Not covered here

- **NAS templates** (Synology / unRAID / TrueNAS): come after
  Phase 4.5 lands the public repo. The Docker image works on all
  three today; the templates just wrap the same image.
- **Backups**: suchi ships nothing backup-specific — restic/borg over
  `DATA_DIR` while suchi is stopped, or an atomic snapshot of the
  underlying volume. Do not copy the SQLite file while suchi is
  running; use `.backup` or a WAL-aware tool.
- **Log shipping**: suchi writes JSON on stderr. Anything that can
  read stderr (journald, docker log driver, kubectl logs) works.

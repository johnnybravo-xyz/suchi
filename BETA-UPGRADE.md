# Beta archive upgrade runbook

This is the shareable, agent-executable cutover checklist for moving a real
Suchi archive from a v0.1.0 beta or an untagged pre-stable build to the current
stable-v1 source line. It assumes one SQLite writer and either Docker Compose or
an equivalent single-process deployment.

The short rule: **ordinary beta archives are upgraded by the final binary. Run
compatibility SQL manually only for the exact already-adopted pre-identity state
described below.** Never change `PRAGMA user_version` or `schema_lineage` to make
an unknown database look supported.

## Agent execution contract

An automated agent following this file MUST obey these rules:

1. Execute gates in order. Do not cross a gate until every stated check passes.
2. Treat every **STOP** condition as terminal. Preserve the stopped service and
   rollback artifacts; do not improvise repair SQL, edit lineage/version values,
   skip a migration, or delete conflicting data.
3. Use the exact target checkout for the image, fingerprint allowlist, and
   compatibility SQL. Record its full Git revision.
4. Keep the rollback directory outside `DATA_DIR`. Never delete or overwrite it
   during the upgrade.
5. Stop every writer before backup or migration. A stopped web container is not
   sufficient when an import, watcher, cron job, or CLI can open the archive.
6. Never print or share `.env` contents, email addresses, cookies, tokens,
   passwords, `.decrypt-key`, OIDC issuer/subject values, or document names.
   Record only the non-secret evidence named in this document.
7. Preserve unexpected files and operator changes. Ask the operator before
   deleting anything not created by this runbook.
8. When a provider requires a password, passkey, or other interactive login,
   ask the operator to complete it in the preserved browser tab. Never request
   or handle their credential.
9. A failed command is not permission to continue. Capture the command, exit
   status, and redacted error, then stop at that gate.
10. Report `[PASS]`, `[STOP]`, or `[N/A]` for every gate in the evidence template
    at the end. A deployment is complete only after the post-start and OIDC
    checks pass.

All shell snippets are intended for one administrative shell on the Suchi host.
If an agent opens a new SSH command for each snippet, it MUST source a saved
non-secret coordinate file or restate the variables explicitly; it must not
assume shell state survives between calls.

## Exact compatibility identities

The target source currently admits these core states. The fingerprint covers
the normalized core `sqlite_schema`, not document data.

| Source | `user_version` | Lineage | Core fingerprint | Action |
| --- | ---: | --- | --- | --- |
| beta.1 | 1 | absent | `68089660de648a4fcc136bcefc105edc5d29dc4de59dad482124914ea626fb2b` | Automatic adoption |
| beta.2 | 2 | absent | `a341b731c93a7270e3440a18f58911df80b2289bf44cd3baeecff4a2b2b0071c` | Automatic adoption |
| beta.3 | 3 | `final-beta-schema-3` | `de0f8f20cfd5b2858051236a045cf67177ad4f32652d3bd646fe177ba462687b` | Automatic adoption |
| Historical schema 3 | 3 | `final-beta-schema-3` | `8cbc483c04442b2995eb0a8a76ca30430b15a63b01cdd0395b76031b690b20b1` | Automatic only when `agent_webhooks` is empty |
| Pre-identity schema 4 | 4 | `final-beta-schema-3` | `5d6ac98308eb644b030092178792a46f36e6f8f5041411f020ed2dcf216f46c2` | Automatic adoption |
| Already-adopted pre-identity state | 1 | `stable-v1` | `5d6ac98308eb644b030092178792a46f36e6f8f5041411f020ed2dcf216f46c2` | Reviewed manual 0005 path only |
| Final stable-v1 | 1 | `stable-v1` | `9d2a6320eae1582b0f294caa6ed518b5a73ab3dbe6597c9ca1a36cc563e03240` | No-op |

Any other combination is **STOP**. Empty databases are fresh installs, not beta
archive upgrades.

The helper below prints `<fingerprint> <core-object-count>`. Final stable-v1
must print the final fingerprint above followed by `209`.

The manual path uses
[`core/db/compatibility/0005_account_identity.sql`](core/db/compatibility/0005_account_identity.sql)
with exact SHA-256:

```text
0fd3d629aa77d2a6c6f6a94c2785d500e7a0dc581511a30184d73a66e1a8ad9a
```

## Gate 0 — declare coordinates

Replace every example path. The rollback root MUST be outside `DATA_DIR`.
`PUBLIC_URL` is the externally reachable HTTPS origin, without a trailing path.
For a locally built old deployment, `OLD_SOURCE_DIR` is its untouched checkout;
`SOURCE_DIR` is a separate, clean checkout of the exact target. They may share
Git objects through a worktree, but they MUST NOT be the same working directory.

```sh
export DEPLOY_DIR=/opt/suchi
export DATA_DIR=/var/lib/suchi
export DB="$DATA_DIR/suchi.db"
export OLD_SOURCE_DIR=/opt/suchi-current-src
export SOURCE_DIR=/opt/suchi-target-src
export COMPOSE_FILE="$DEPLOY_DIR/docker-compose.yml"
export ENV_FILE="$DEPLOY_DIR/.env"
export SERVICE=suchi
export PUBLIC_URL=https://suchi.example.com
export TARGET_REVISION="$(git -C "$SOURCE_DIR" rev-parse HEAD)"
export SHORT_REVISION="$(git -C "$SOURCE_DIR" rev-parse --short=12 HEAD)"
export CANDIDATE_IMAGE="suchi:candidate-$SHORT_REVISION"
export STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
export ROLLBACK_DIR="/srv/suchi-rollback/$STAMP"

for value in \
  "$DEPLOY_DIR" "$DATA_DIR" "$DB" "$OLD_SOURCE_DIR" "$SOURCE_DIR" \
  "$COMPOSE_FILE" "$ENV_FILE" "$PUBLIC_URL" "$TARGET_REVISION"
do
  test -n "$value" || { echo '[STOP] missing coordinate'; exit 1; }
done

test -f "$DB"
export DATA_UID="$(stat -c '%u' "$DB")"
export DATA_GID="$(stat -c '%g' "$DB")"
test -f "$COMPOSE_FILE"
test -f "$ENV_FILE"
test -z "$(git -C "$SOURCE_DIR" status --short)"
test -z "$(git -C "$OLD_SOURCE_DIR" status --short)"
test "$(realpath "$OLD_SOURCE_DIR")" != "$(realpath "$SOURCE_DIR")"

sudo install -d -m 700 -o "$(id -u)" -g "$(id -g)" "$ROLLBACK_DIR"
printf '%s\n' "$TARGET_REVISION" > "$ROLLBACK_DIR/target-revision.txt"
printf '[PASS] gate-0 coordinates declared\n'
```

**STOP** if either checkout has local changes, both coordinates resolve to the
same working directory, the database path is ambiguous, or the rollback path is
inside `DATA_DIR`.

## Gate 1 — preserve identity and baseline evidence

When OIDC is enabled, do this before stopping the old build:

1. Sign in as the archive owner in a normal browser window.
2. Confirm **My account** shows the expected owner and role.
3. Leave that tab open. Do not sign out or clear site data.

The preserved session is required to bind an existing email-occupied row to its
OIDC issuer/subject. Ordinary OIDC login cannot adopt that row by email, by
design. If no session survives, rollback is required; do not delete, merge, or
recreate the account.

Record non-secret baseline facts:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$DB" '
  SELECT id, role, disabled FROM users ORDER BY id;
  SELECT count(*) AS users FROM users;
  SELECT count(*) AS documents FROM documents;
  SELECT count(*) AS group_members FROM group_members;
  SELECT count(*) AS system_members FROM jd_system_members;
  SELECT count(*) AS sessions FROM sessions;
  SELECT count(*) AS audit_events FROM audit_events;
' | tee "$ROLLBACK_DIR/baseline-counts.txt"

printf '[PASS] gate-1 owner session and baseline recorded\n'
```

Do not add email columns to the shared output.

## Gate 2 — stop every writer

First identify deployment-specific writers: Suchi, CLI imports, mail intake
sidecars, scheduled jobs, and operator scripts. Stop them all. The commands
below stop only the named Compose service; they do not discover external jobs.

```sh
cd "$DEPLOY_DIR"
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" stop "$SERVICE"

CONTAINER_ID="$(docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" ps -q "$SERVICE")"
test -n "$CONTAINER_ID"
docker inspect --format='status={{.State.Status}} exit={{.State.ExitCode}}' "$CONTAINER_ID"

command -v fuser >/dev/null
if sudo fuser "$DB" "$DB-wal" "$DB-shm" >"$ROLLBACK_DIR/fuser.txt" 2>&1; then
  cat "$ROLLBACK_DIR/fuser.txt"
  echo '[STOP] a process still holds the database'
  exit 1
fi

sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$DB" '
  PRAGMA user_version;
  SELECT count(*) AS lineage_tables
  FROM sqlite_schema WHERE type="table" AND name="schema_lineage";
  PRAGMA integrity_check;
  PRAGMA foreign_key_check;
' | tee "$ROLLBACK_DIR/preflight-database.txt"

printf '[PASS] gate-2 all writers stopped; integrity ok; foreign keys clean\n'
```

Expected integrity output is exactly `ok`; foreign-key check emits no rows.
`fuser` should emit no process IDs. **STOP** on any holder, integrity result other
than `ok`, foreign-key row, or unexplained service exit.

## Gate 3 — create and verify a complete rollback set

The rollback unit is the whole instance: database, blobs, `.decrypt-key`,
configuration, old image or binary, and old source when locally built. A
SQLite-only snapshot is not a complete archive backup.

```sh
OLD_IMAGE_REF="$(docker inspect --format='{{.Config.Image}}' "$CONTAINER_ID")"
OLD_IMAGE_ID="$(docker inspect --format='{{.Image}}' "$CONTAINER_ID")"
printf '%s\n' "$OLD_IMAGE_REF" > "$ROLLBACK_DIR/old-image-ref.txt"
printf '%s\n' "$OLD_IMAGE_ID" > "$ROLLBACK_DIR/old-image-id.txt"

sudo tar --xattrs --acls --numeric-owner -cpf - -C "$DATA_DIR" . \
  > "$ROLLBACK_DIR/data.tar"

sudo install -m 600 -o "$(id -u)" -g "$(id -g)" \
  "$ENV_FILE" "$ROLLBACK_DIR/.env"
sudo install -m 600 -o "$(id -u)" -g "$(id -g)" \
  "$COMPOSE_FILE" "$ROLLBACK_DIR/docker-compose.yml"

docker image save "$OLD_IMAGE_REF" -o "$ROLLBACK_DIR/old-image.tar"
OLD_SOURCE_REVISION="$(git -C "$OLD_SOURCE_DIR" rev-parse HEAD)"
printf '%s\n' "$OLD_SOURCE_REVISION" > "$ROLLBACK_DIR/old-source-revision.txt"
git -C "$OLD_SOURCE_DIR" bundle create "$ROLLBACK_DIR/old-source.bundle" HEAD
sudo sha256sum "$DB" > "$ROLLBACK_DIR/live-db.sha256"

(
  cd "$ROLLBACK_DIR"
  sha256sum \
    data.tar .env docker-compose.yml old-image.tar old-source.bundle \
    > SHA256SUMS
  sha256sum -c SHA256SUMS
)

tar -tf "$ROLLBACK_DIR/data.tar" | grep -Fx './suchi.db'
tar -tf "$ROLLBACK_DIR/data.tar" | grep -Fx './.decrypt-key'
docker image load -i "$ROLLBACK_DIR/old-image.tar"
git bundle verify "$ROLLBACK_DIR/old-source.bundle"
git bundle list-heads "$ROLLBACK_DIR/old-source.bundle" \
  | grep -F "$OLD_SOURCE_REVISION"

printf '[PASS] gate-3 complete rollback set verified\n'
```

For a published-image deployment without an old source checkout, replace the
old-source commands and checksum entry with the exact release tag, image digest,
signature/provenance result, and release checksum. The exact target source is
still required for schema classification. For a bare binary, copy the old
executable instead of an image.

Protect the rollback directory like the live archive: it contains documents,
credentials, configuration, and session state. **STOP** if any checksum, image
load, bundle verification, database member, or key member check fails.

## Gate 4 — build or fetch the exact target

Choose exactly one branch.

### Source build

Docker build contexts do not include `.git`; pass `REVISION` explicitly so
`suchi version` proves the source identity.

```sh
cd "$SOURCE_DIR"
test -z "$(git status --short)"
test "$(git rev-parse HEAD)" = "$TARGET_REVISION"

docker build \
  --target standard \
  --build-arg REVISION="$TARGET_REVISION" \
  --label "org.opencontainers.image.revision=$TARGET_REVISION" \
  --tag "$CANDIDATE_IMAGE" \
  .

docker run --rm "$CANDIDATE_IMAGE" version
docker image inspect \
  --format='id={{.Id}} revision={{index .Config.Labels "org.opencontainers.image.revision"}}' \
  "$CANDIDATE_IMAGE" \
  | tee "$ROLLBACK_DIR/candidate-image.txt"
```

The version output must contain `$SHORT_REVISION`, and the label must equal the
full target revision.

### Published image

Use an immutable digest, never a moving `latest` or `beta` tag:

```sh
export CANDIDATE_IMAGE='ghcr.io/johnnybravo-xyz/suchi@sha256:<verified-digest>'
docker pull "$CANDIDATE_IMAGE"
docker image inspect --format='id={{.Id}} repo={{join .RepoDigests ","}}' \
  "$CANDIDATE_IMAGE" | tee "$ROLLBACK_DIR/candidate-image.txt"
docker run --rm "$CANDIDATE_IMAGE" version
```

Verify the published signature, provenance, and checksum according to the
release notes. Record the immutable image ID:

```sh
CANDIDATE_IMAGE_ID="$(docker image inspect --format='{{.Id}}' "$CANDIDATE_IMAGE")"
printf '%s\n' "$CANDIDATE_IMAGE_ID" > "$ROLLBACK_DIR/candidate-image-id.txt"
printf '[PASS] gate-4 exact candidate acquired\n'
```

**STOP** if revision, digest, signature, provenance, or version does not match.

## Gate 5 — classify the live schema exactly

This helper implements the same length-prefixed, ordered core-manifest algorithm
as `core/db/adopt.go`. It reads the post-extension core-object allowlist from the
same target source.

```sh
cat > /tmp/suchi-schema-fingerprint.py <<'PY'
import hashlib
import re
import sqlite3
import struct
import sys

if len(sys.argv) != 3:
    raise SystemExit('usage: suchi-schema-fingerprint.py DB SOURCE_DIR')

db_path, source_dir = sys.argv[1:]
source = open(f'{source_dir}/core/db/migrate.go', encoding='utf-8').read()
match = re.search(
    r'var coreAfterExtensionObjects = \[\]string\{(.*?)\n\}',
    source,
    re.S,
)
if not match:
    raise SystemExit('cannot parse coreAfterExtensionObjects')
allow = set(re.findall(r'"([^"]+)"', match.group(1)))

connection = sqlite3.connect(f'file:{db_path}?mode=ro', uri=True)
rows = list(connection.execute(
    "SELECT rowid,type,name,tbl_name,COALESCE(sql,'') "
    "FROM sqlite_schema WHERE name NOT GLOB 'sqlite_*'"
))
boundary = next((row[0] for row in rows if row[2] == '_suchi_extension_migrations'), None)
manifest = [
    row[1:]
    for row in rows
    if boundary is None or row[0] < boundary or row[2] in allow
]
manifest.sort()

digest = hashlib.sha256()
for row in manifest:
    for field in row:
        encoded = field.encode('utf-8')
        digest.update(struct.pack('>I', len(encoded)))
        digest.update(encoded)
print(f'{digest.hexdigest()} {len(manifest)}')
PY

sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -list "$DB" \
  'PRAGMA user_version; SELECT count(*) FROM sqlite_schema WHERE type="table" AND name="schema_lineage";' \
  | tee "$ROLLBACK_DIR/source-version-lineage-presence.txt"

if test "$(sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -noheader "$DB" \
  'SELECT count(*) FROM sqlite_schema WHERE type="table" AND name="schema_lineage";')" = 1
then
  sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -list "$DB" \
    'SELECT singleton,name FROM schema_lineage;' \
    | tee "$ROLLBACK_DIR/source-lineage.txt"
fi

sudo -u "#$DATA_UID" -g "#$DATA_GID" python3 /tmp/suchi-schema-fingerprint.py "$DB" "$SOURCE_DIR" \
  | tee "$ROLLBACK_DIR/source-fingerprint.txt"
```

Match version, lineage, and fingerprint to one row in the compatibility table.
For the historical schema-3 row, also require:

```sh
test "$(sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -noheader "$DB" \
  'SELECT count(*) FROM agent_webhooks;')" = 0
```

Choose one branch and record it:

```sh
printf '%s\n' automatic > "$ROLLBACK_DIR/upgrade-path.txt"
# or, only for version=1 + stable-v1 + pre-identity fingerprint:
# printf '%s\n' manual-0005 > "$ROLLBACK_DIR/upgrade-path.txt"
# or, only for the final fingerprint:
# printf '%s\n' stable-noop > "$ROLLBACK_DIR/upgrade-path.txt"

printf '[PASS] gate-5 source state classified\n'
```

**STOP** when no row matches exactly. Do not use the manual path for canonical
beta.1–beta.3, historical schema 3, schema 4, or final stable-v1.

## Gate 6 — rehearse on the backup

Extract two complete copies: one immutable source comparison and one writable
rehearsal. This preserves WAL/key/blob relationships and avoids treating a
SQLite-only copy as the backup.

```sh
export SOURCE_SNAPSHOT_DIR="$ROLLBACK_DIR/source-snapshot"
export REHEARSAL_DIR="$ROLLBACK_DIR/rehearsal"
export SOURCE_SNAPSHOT_DB="$SOURCE_SNAPSHOT_DIR/suchi.db"
export REHEARSAL_DB="$REHEARSAL_DIR/suchi.db"

install -d -m 700 "$SOURCE_SNAPSHOT_DIR" "$REHEARSAL_DIR"
sudo tar --numeric-owner -xf "$ROLLBACK_DIR/data.tar" -C "$SOURCE_SNAPSHOT_DIR"
sudo tar --numeric-owner -xf "$ROLLBACK_DIR/data.tar" -C "$REHEARSAL_DIR"
sudo chown "$DATA_UID:$DATA_GID" "$SOURCE_SNAPSHOT_DIR" "$REHEARSAL_DIR"
sudo chmod 700 "$SOURCE_SNAPSHOT_DIR" "$REHEARSAL_DIR"
```

### Automatic or stable-noop branch

Run the target binary against the rehearsal. `gc` is a dry-run unless `--apply`
is supplied; startup preparation still performs and verifies automatic schema
adoption.

```sh
docker run --rm \
  --user "$DATA_UID:$DATA_GID" \
  -e DATA_DIR=/data \
  -e PUBLIC_URL=http://127.0.0.1:18080 \
  -v "$REHEARSAL_DIR:/data" \
  "$CANDIDATE_IMAGE" gc

sudo -u "#$DATA_UID" -g "#$DATA_GID" python3 /tmp/suchi-schema-fingerprint.py "$REHEARSAL_DB" "$SOURCE_DIR" \
  | tee "$ROLLBACK_DIR/rehearsal-fingerprint.txt"
```

Require final fingerprint
`9d2a6320eae1582b0f294caa6ed518b5a73ab3dbe6597c9ca1a36cc563e03240`,
integrity `ok`, and no foreign-key rows:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$REHEARSAL_DB" '
  PRAGMA user_version;
  SELECT singleton,name FROM schema_lineage;
  PRAGMA integrity_check;
  PRAGMA foreign_key_check;
'
```

Automatic adoption should also create its exclusive pre-adoption database
snapshot in the rehearsal. Final stable-noop should not create one.

### Manual-0005 branch

Use this branch only when all three source facts match:

```text
user_version = 1
schema_lineage = stable-v1
fingerprint = 5d6ac98308eb644b030092178792a46f36e6f8f5041411f020ed2dcf216f46c2
```

Verify guards without printing an owner email:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$DB" '
  SELECT count(*) AS plugin_kv_rows FROM plugin_kv;
  SELECT count(*) AS plugin_kv_dependents
  FROM sqlite_schema
  WHERE name != "plugin_kv" AND sql IS NOT NULL
    AND (tbl_name = "plugin_kv" OR instr(lower(sql), "plugin_kv") > 0);
  SELECT CASE
    WHEN NOT EXISTS (
      SELECT 1 FROM settings WHERE key = "ingest.fs_watch_owner"
    ) THEN "absent"
    WHEN (
      SELECT json_valid(value_json)
        AND json_type(value_json) = "text"
        AND trim(json_extract(value_json, "$")) = ""
      FROM settings WHERE key = "ingest.fs_watch_owner"
    ) THEN "blank"
    WHEN (
      SELECT count(*) FROM users
      WHERE lower(trim(email)) = lower(trim(json_extract(
        (SELECT value_json FROM settings WHERE key = "ingest.fs_watch_owner"), "$"
      )))
    ) = 1 THEN "resolves_once"
    ELSE "STOP"
  END AS watched_owner_state;
'
```

Both plugin counts must be zero. Watched owner must be `absent`, `blank`, or
`resolves_once`. A non-empty environment `INGEST_FS_OWNER_EMAIL` must also match
the existing owner email before restart; compare it locally without printing
the value into a shared log.

Copy and verify the exact migration:

```sh
sudo install -m 644 \
  "$SOURCE_DIR/core/db/compatibility/0005_account_identity.sql" \
  /tmp/0005_account_identity.sql

printf '%s  %s\n' \
  0fd3d629aa77d2a6c6f6a94c2785d500e7a0dc581511a30184d73a66e1a8ad9a \
  /tmp/0005_account_identity.sql \
  | sudo sha256sum -c -
```

Apply the exact transaction only to the rehearsal:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 "$REHEARSAL_DB" <<'SQL'
.bail on
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
.read /tmp/0005_account_identity.sql
UPDATE schema_lineage SET name='stable-v1' WHERE singleton=1;
PRAGMA user_version=1;
COMMIT;
PRAGMA foreign_keys=ON;
PRAGMA wal_checkpoint(TRUNCATE);
PRAGMA foreign_key_check;
PRAGMA integrity_check;
SQL

sudo -u "#$DATA_UID" -g "#$DATA_GID" python3 /tmp/suchi-schema-fingerprint.py "$REHEARSAL_DB" "$SOURCE_DIR" \
  | tee "$ROLLBACK_DIR/rehearsal-fingerprint.txt"

docker run --rm \
  --user "$DATA_UID:$DATA_GID" \
  -e DATA_DIR=/data \
  -e PUBLIC_URL=http://127.0.0.1:18080 \
  -v "$REHEARSAL_DIR:/data" \
  "$CANDIDATE_IMAGE" gc
```

The transaction must print `ok`, no foreign-key rows, and the exact final
fingerprint. The candidate command must accept the schema.

For the manual path, compare source and rehearsal IDs plus ownership directly:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$REHEARSAL_DB" <<SQL
.bail on
ATTACH DATABASE 'file:$SOURCE_SNAPSHOT_DB?mode=ro' AS old;
SELECT
  (SELECT count(*) FROM (SELECT id FROM old.users EXCEPT SELECT id FROM main.users))
  + (SELECT count(*) FROM (SELECT id FROM main.users EXCEPT SELECT id FROM old.users))
    AS user_id_differences,
  (SELECT count(*) FROM (SELECT id,owner_id FROM old.documents EXCEPT SELECT id,owner_id FROM main.documents))
  + (SELECT count(*) FROM (SELECT id,owner_id FROM main.documents EXCEPT SELECT id,owner_id FROM old.documents))
    AS document_owner_differences,
  (SELECT count(*) FROM (SELECT group_id,user_id FROM old.group_members EXCEPT SELECT group_id,user_id FROM main.group_members))
  + (SELECT count(*) FROM (SELECT group_id,user_id FROM main.group_members EXCEPT SELECT group_id,user_id FROM old.group_members))
    AS group_membership_differences,
  (SELECT count(*) FROM (SELECT system_id,user_id FROM old.jd_system_members EXCEPT SELECT system_id,user_id FROM main.jd_system_members))
  + (SELECT count(*) FROM (SELECT system_id,user_id FROM main.jd_system_members EXCEPT SELECT system_id,user_id FROM old.jd_system_members))
    AS system_membership_differences,
  (SELECT count(*) FROM (SELECT id,user_id FROM old.sessions EXCEPT SELECT id,user_id FROM main.sessions))
  + (SELECT count(*) FROM (SELECT id,user_id FROM main.sessions EXCEPT SELECT id,user_id FROM old.sessions))
    AS session_differences,
  (SELECT count(*) FROM (SELECT id FROM old.audit_events EXCEPT SELECT id FROM main.audit_events))
  + (SELECT count(*) FROM (SELECT id FROM main.audit_events EXCEPT SELECT id FROM old.audit_events))
    AS audit_id_differences;
SELECT count(*) AS missing_legacy_senders
FROM old.documents AS d
WHERE d.correspondent_id IS NOT NULL
  AND NOT EXISTS (
    SELECT 1 FROM main.document_correspondents AS dc
    WHERE dc.document_id=d.id
      AND dc.correspondent_id=d.correspondent_id
      AND dc.role='sender'
  );
DETACH DATABASE old;
SQL
```

Every difference count must be zero. Record expected migration-only count
changes separately: legacy correspondent relations may increase, and successful
jobs older than seven days may be pruned.

For every branch, run final rehearsal checks:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$REHEARSAL_DB" '
  PRAGMA integrity_check;
  PRAGMA foreign_key_check;
  SELECT count(*) AS users FROM users;
  SELECT count(*) AS documents FROM documents;
  SELECT count(*) AS sessions FROM sessions;
  SELECT count(*) AS audit_events FROM audit_events;
' | tee "$ROLLBACK_DIR/rehearsal-checks.txt"

printf '[PASS] gate-6 rehearsal reached final stable-v1\n'
```

**STOP** on any migration error, wrong fingerprint, integrity/FK failure,
unexpected ID/count change, or candidate rejection.

## Gate 7 — apply the chosen live path

Reconfirm quiescence and prove the live database has not changed since backup:

```sh
if sudo fuser "$DB" "$DB-wal" "$DB-shm" >"$ROLLBACK_DIR/fuser-before-live.txt" 2>&1; then
  cat "$ROLLBACK_DIR/fuser-before-live.txt"
  echo '[STOP] a process holds the live database'
  exit 1
fi

sha256sum -c "$ROLLBACK_DIR/live-db.sha256"
```

### Automatic or stable-noop branch

Do not run SQL. The final binary performs the automatic adoption during start.
Continue to Gate 8.

### Manual-0005 branch

Recheck the migration checksum, then apply the same rehearsed transaction:

```sh
printf '%s  %s\n' \
  0fd3d629aa77d2a6c6f6a94c2785d500e7a0dc581511a30184d73a66e1a8ad9a \
  /tmp/0005_account_identity.sql \
  | sudo sha256sum -c -

sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 "$DB" <<'SQL'
.bail on
PRAGMA foreign_keys=OFF;
BEGIN IMMEDIATE;
.read /tmp/0005_account_identity.sql
UPDATE schema_lineage SET name='stable-v1' WHERE singleton=1;
PRAGMA user_version=1;
COMMIT;
PRAGMA foreign_keys=ON;
PRAGMA wal_checkpoint(TRUNCATE);
PRAGMA foreign_key_check;
PRAGMA integrity_check;
SQL

sudo -u "#$DATA_UID" -g "#$DATA_GID" python3 /tmp/suchi-schema-fingerprint.py "$DB" "$SOURCE_DIR" \
  | tee "$ROLLBACK_DIR/live-fingerprint.txt"

sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$DB" '
  PRAGMA user_version;
  SELECT singleton,name FROM schema_lineage;
  PRAGMA integrity_check;
  PRAGMA foreign_key_check;
  SELECT id,role,disabled,dev_seeded,
         oidc_issuer IS NOT NULL AS has_oidc_issuer,
         oidc_subject IS NOT NULL AS has_oidc_subject
  FROM users ORDER BY id;
  SELECT count(*) AS documents FROM documents;
  SELECT count(*) AS sessions FROM sessions;
  SELECT count(*) AS audit_events FROM audit_events;
' | tee "$ROLLBACK_DIR/live-post-migration.txt"
```

Require version 1, lineage `stable-v1`, exact final fingerprint, integrity `ok`,
no foreign-key rows, unchanged numeric IDs/counts, and an unbound existing user
until the browser-binding step.

```sh
printf '[PASS] gate-7 live path applied or delegated to final binary\n'
```

## Gate 8 — install, start, and verify the exact image

Make Compose resolve to the candidate. If the deployment uses a local fixed tag,
retag the verified candidate. If it uses `SUCHI_IMAGE`, set that variable to the
verified immutable digest without printing the rest of `.env`.

```sh
# Example only for a Compose file whose image is suchi:local:
docker image tag "$CANDIDATE_IMAGE" suchi:local

docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" config --quiet
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" config --images \
  | tee "$ROLLBACK_DIR/resolved-images.txt"
```

Confirm the resolved Suchi image is the intended candidate, then start exactly
one instance:

```sh
cd "$DEPLOY_DIR"
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
  up -d --no-build --force-recreate "$SERVICE"

NEW_CONTAINER_ID="$(docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" ps -q "$SERVICE")"
test -n "$NEW_CONTAINER_ID"
test "$(docker inspect --format='{{.Image}}' "$NEW_CONTAINER_ID")" = "$CANDIDATE_IMAGE_ID"

docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
  exec -T "$SERVICE" suchi version
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
  exec -T "$SERVICE" suchi healthcheck

curl -fsS "$PUBLIC_URL/healthz"
curl -fsS "$PUBLIC_URL/readyz"
```

For an automatic branch, now calculate the live fingerprint and verify the
binary completed adoption:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" python3 /tmp/suchi-schema-fingerprint.py "$DB" "$SOURCE_DIR" \
  | tee "$ROLLBACK_DIR/live-fingerprint.txt"

sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$DB" '
  PRAGMA user_version;
  SELECT singleton,name FROM schema_lineage;
  PRAGMA integrity_check;
  PRAGMA foreign_key_check;
' | tee "$ROLLBACK_DIR/live-post-start-schema.txt"
```

Require fingerprint
`9d2a6320eae1582b0f294caa6ed518b5a73ab3dbe6597c9ca1a36cc563e03240`,
version 1, lineage `stable-v1`, integrity `ok`, and no foreign-key rows.

Review startup logs. Require `main.serve`, the expected backup loop and enabled
watchers, and no schema, integrity, authentication, or restart loop:

```sh
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
  logs --since 10m "$SERVICE" \
  | tee "$ROLLBACK_DIR/startup.log"

docker inspect --format='status={{.State.Status}} health={{.State.Health.Status}} restart={{.RestartCount}}' \
  "$NEW_CONTAINER_ID"

printf '[PASS] gate-8 exact target is ready\n'
```

Redact hostnames or account identifiers before sharing logs. **STOP and rollback**
on a wrong image ID, failed probe, wrong fingerprint, repeated restart, schema
error, integrity/FK failure, or unexpected integration failure.

## Gate 9 — bind the existing OIDC owner

This gate is required only when OIDC is configured and the existing account was
unbound. Otherwise record `[N/A]`.

1. Return to the preserved signed-in browser tab.
2. Open **My account** and choose **Sync from identity provider**.
3. Complete the forced provider login. It must return the same verified email.
4. Confirm the same numeric Suchi user remains signed in with the same role,
   document visibility, memberships, and permissions.
5. Sign out normally, sign in again through OIDC, and repeat the checks.

When the provider asks for a password or passkey, the agent must pause this gate
and ask the operator to complete the provider UI. The agent must not receive the
credential.

Verify only non-secret database evidence:

```sh
sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$DB" '
  SELECT id,role,disabled,
         oidc_issuer IS NOT NULL AS has_oidc_issuer,
         oidc_subject IS NOT NULL AS has_oidc_subject
  FROM users ORDER BY id;
  SELECT id,action,object_kind,object_id,retained
  FROM audit_events
  WHERE action="user.oidc_bound"
  ORDER BY id DESC LIMIT 5;
'
```

Expected: the original user ID has both OIDC fields, one retained
`user.oidc_bound` event identifies that user, the callback replaced the current
cookie, and old sessions invalidated by the flow no longer authenticate. Never
share raw cookies, issuer/subject pairs, tokens, or emails.

```text
[PASS] gate-9 same-user OIDC binding, sign-out, and fresh sign-in verified
```

If no live session survives or the provider returns a different email, **STOP
and rollback**. Do not bypass the conflict.

## Gate 10 — verify archive behavior and retain rollback

Complete all applicable checks:

- Open known documents and download one original.
- Search for a known term and confirm expected results.
- Confirm owner/document/membership IDs and permissions are unchanged.
- Confirm filesystem and mail watchers have their expected enabled/disabled
  state and owner. Account for documents legitimately ingested after restart.
- Confirm a post-upgrade audit event can be written and viewed.
- Confirm the built-in backup loop is active, then verify a new snapshot through
  its manifest/checksum path.
- Run `suchi doctor`; review every warning rather than treating exit zero as the
  only signal.
- Keep the complete rollback directory and old image until a restore drill has
  succeeded.

```sh
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
  exec -T "$SERVICE" suchi doctor \
  | tee "$ROLLBACK_DIR/doctor.txt"

sudo -u "#$DATA_UID" -g "#$DATA_GID" sqlite3 -readonly -header -column "$DB" '
  SELECT count(*) AS users FROM users;
  SELECT count(*) AS documents FROM documents;
  SELECT count(*) AS group_members FROM group_members;
  SELECT count(*) AS system_members FROM jd_system_members;
  SELECT count(*) AS sessions FROM sessions;
  SELECT count(*) AS audit_events FROM audit_events;
' | tee "$ROLLBACK_DIR/final-counts.txt"

printf '[PASS] gate-10 archive, watchers, audit, backup, and health verified\n'
```

Only after every gate passes may the agent remove its temporary scripts and the
two extracted rehearsal directories. It MUST retain `data.tar`, old image or
binary, old configuration, checksums, evidence, and failed-state material for
the operator's retention period.

```sh
rm -f /tmp/suchi-schema-fingerprint.py
sudo rm -f /tmp/0005_account_identity.sql
# Optional after operator-approved success:
# sudo rm -rf "$SOURCE_SNAPSHOT_DIR" "$REHEARSAL_DIR"
```

## Rollback

Migrations are one-way. Never start the old binary against the migrated live
directory. Restore the complete backup and old image/config together.

```sh
cd "$DEPLOY_DIR"
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" stop "$SERVICE"
# Stop every external writer again here.

sudo mv "$DATA_DIR" "$DATA_DIR.failed-$STAMP"
sudo install -d -m 700 "$DATA_DIR"
sudo tar --numeric-owner -xf "$ROLLBACK_DIR/data.tar" -C "$DATA_DIR"
sudo chmod 600 "$DATA_DIR/.decrypt-key"

docker image load -i "$ROLLBACK_DIR/old-image.tar"
sudo install -m 600 "$ROLLBACK_DIR/.env" "$ENV_FILE"
sudo install -m 600 "$ROLLBACK_DIR/docker-compose.yml" "$COMPOSE_FILE"

docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
  up -d --no-build --force-recreate "$SERVICE"
docker compose --env-file "$ENV_FILE" -f "$COMPOSE_FILE" \
  exec -T "$SERVICE" suchi healthcheck
```

Verify the old owner, a known document, search, and one sealed integration. Keep
the failed migrated directory until the incident is understood. If the archive
layout differs from the `tar -C "$DATA_DIR" .` convention used above, adjust the
restore destination before extracting—not afterward.

## Machine-readable evidence template

Fill every field with `PASS`, `STOP`, or `N/A`; use hashes/IDs only where named.
Do not add secret values.

```json
{
  "target_revision": "<full git revision or release digest>",
  "source": {
    "user_version": 0,
    "lineage": "absent|final-beta-schema-3|stable-v1",
    "fingerprint": "<sha256>",
    "upgrade_path": "automatic|manual-0005|stable-noop"
  },
  "gates": {
    "coordinates": "PASS|STOP",
    "preserved_owner_session": "PASS|STOP|N/A",
    "writers_quiesced": "PASS|STOP",
    "complete_backup_verified": "PASS|STOP",
    "candidate_provenance": "PASS|STOP",
    "source_classified": "PASS|STOP",
    "rehearsal": "PASS|STOP",
    "live_schema": "PASS|STOP",
    "readiness": "PASS|STOP",
    "oidc_same_user_binding": "PASS|STOP|N/A",
    "documents_search_permissions": "PASS|STOP",
    "watchers_audit_backup_doctor": "PASS|STOP"
  },
  "target": {
    "user_version": 1,
    "lineage": "stable-v1",
    "fingerprint": "9d2a6320eae1582b0f294caa6ed518b5a73ab3dbe6597c9ca1a36cc563e03240"
  },
  "rollback_artifact": "<protected path or backup ID>",
  "rollback_retain_until": "<UTC date>"
}
```

Related detail: [Backup and restore](docs/backup-restore.mdx), [release database
compatibility](docs/release-process.mdx#stable-v1-database-compatibility), and
[OIDC configuration](docs/config.mdx#oidc).

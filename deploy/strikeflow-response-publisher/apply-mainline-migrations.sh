#!/bin/sh
set -eu

if [ "$#" -ne 7 ] || [ "$7" != "--confirm-migrate" ]; then
  echo "usage: $0 RELEASE_DIR IMAGE_DIGEST PREFLIGHT_DIR ENCRYPTED_BACKUP BACKUP_SHA256 EVIDENCE_DIR --confirm-migrate" >&2
  exit 64
fi

release_dir=$(readlink -f "$1")
image_digest=$2
preflight_dir=$(readlink -f "$3")
backup_file=$(readlink -f "$4")
backup_sha=$(readlink -f "$5")
evidence_dir=$6
base_compose=/opt/multica/docker-compose.selfhost.yml
pin_compose=/opt/multica/docker-compose.pin.yml
base_env=/opt/multica/.env
lock_file=/run/lock/multica-response-publisher-deploy.lock
artifact_image_digest=$(sed -n 's/^image_digest=//p' "$release_dir/ARTIFACTS")
artifact_image_id=$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")
source_commit=$(sed -n 's/^source_commit=//p' "$release_dir/ARTIFACTS")

# This is the only migration set this gate may execute. 253–257 are already
# represented by sealed predecessor aliases; the legacy 235 row is retained.
migration_allowlist='224_agent_task_session_rollout_missing,225_chat_message_channel_media_pending,226_chat_message_channel_ingested,227_channel_media_pending_object,228_channel_media_pending_object_key_index,229_channel_media_pending_object_primary_key,230_channel_media_pending_object_claim_index,231_agent_task_queue_terminal_completed_at_index,232_channel_media_pending_object_due_index,233_agent_task_queue_agent_terminal_latest_index,234_agent_task_queue_retired_session_id,235_chat_message_quick_actions,236_agent_task_quick_actions_disabled,237_quick_action,238_quick_action_workspace_index,239_comment_quick_action,240_agent_task_regenerate_quick_actions,241_comment_parent_lookup_index,242_runtime_profile_add_qoderclicn,243_workspace_teardown_dirty_trigger_guard,244_issue_dependency_issue_index,245_issue_dependency_depends_on_index,246_inbox_item_issue_index,247_comment_parent_index,248_agent_task_trigger_comment_index,249_issue_subscriber_delegated,250_issue_subscriber_opt_out_scope,251_agent_runtime_unbind,252_strikeflow_connector_principal,258_strikeflow_content_reply_connector,259_strikeflow_content_reply_receipt_unique,260_strikeflow_response_outbox_identity_immutable'
canonical_aliases='253_strikeflow_response_outbox 254_strikeflow_connector_reply_command_unique 255_strikeflow_response_outbox_event_unique 256_strikeflow_response_outbox_due_index 257_strikeflow_response_outbox_event_id_unique'
predecessor_extras='235_strikeflow_connector_principal 900001_strikeflow_response_outbox 900002_strikeflow_connector_reply_command_unique 900003_strikeflow_response_outbox_event_unique 900004_strikeflow_response_outbox_due_index 900005_strikeflow_response_outbox_event_id_unique'

case "$release_dir" in /opt/multica-response-publisher/releases/*) ;; *) exit 1;; esac
case "$preflight_dir" in /var/backups/multica-response-publisher/*) ;; *) exit 1;; esac
case "$backup_file" in /var/backups/multica/*.dump.age) ;; *) exit 1;; esac
case "$backup_sha" in /var/backups/multica/*.sha256) ;; *) exit 1;; esac
evidence_parent=$(readlink -f "$(dirname "$evidence_dir")")
evidence_name=$(basename "$evidence_dir")
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in gate-a-mainline-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
case "$evidence_name" in *[!A-Za-z0-9._-]*) echo "invalid evidence path" >&2; exit 1;; esac
evidence_dir=$evidence_parent/$evidence_name
test ! -e "$evidence_dir"
test "$(stat -c '%U:%G %a' "$backup_file")" = root:root\ 600
test "$(stat -c '%U:%G %a' "$backup_sha")" = root:root\ 600
test "$(wc -l <"$backup_sha" | tr -d ' ')" -eq 1
expected_backup_hash=$(awk 'NF == 2 {print $1}' "$backup_sha")
expected_backup_name=$(awk 'NF == 2 {print $2}' "$backup_sha" | sed 's/^\*//')
test "$expected_backup_name" = "$(basename "$backup_file")"
test "$expected_backup_hash" = "$(sha256sum "$backup_file" | awk '{print $1}')"

exec 9>"$lock_file"
flock -n 9 || { echo "another response deployment is running" >&2; exit 1; }
test "$(readlink -f /opt/multica-response-publisher/current)" = "$release_dir"
test "$(stat -c '%U:%G %a' "$preflight_dir")" = root:root\ 700
(cd / && sha256sum -c "$preflight_dir/active-compose.sha256" >/dev/null)
(cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null)
test "$image_digest" = "$artifact_image_digest"
test "$(docker image inspect "$image_digest" --format '{{.Id}}')" = "$artifact_image_id"
docker image inspect "$image_digest" --format '{{range .RepoDigests}}{{println .}}{{end}}' | grep -Fqx "$artifact_image_digest"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.source"}}')" = "$source_commit"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.state"}}')" = dormant

for unit in strikeflow-multica-content-dispatch.service strikeflow-multica-content-dispatch.timer \
  strikeflow-multica-content-ongoing.service strikeflow-multica-content-ongoing.timer \
  multica-response-publisher.service multica-response-publisher.timer; do
  if systemctl is-active --quiet "$unit" 2>/dev/null; then
    echo "$unit must be inactive during the migration gate" >&2
    exit 1
  fi
  pid=$(systemctl show -p MainPID --value "$unit" 2>/dev/null || true)
  test -z "$pid" || test "$pid" = 0
done
for timer in strikeflow-multica-content-dispatch.timer strikeflow-multica-content-ongoing.timer multica-response-publisher.timer; do
  enabled=$(systemctl is-enabled "$timer" 2>/dev/null || true)
  case "$enabled" in enabled|enabled-runtime) echo "$timer must not be enabled" >&2; exit 1;; esac
done

"$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
  --before-start --allow-delivered-outbox "$release_dir" "$image_digest" "$preflight_dir"

install -d -o root -g root -m 0700 "$evidence_dir"
install -o root -g root -m 0600 "$backup_sha" "$evidence_dir/backup.sha256"
printf '%s\n' "$backup_file" >"$evidence_dir/encrypted-backup.path"
chmod 0600 "$evidence_dir/encrypted-backup.path"
printf '%s\n' "$migration_allowlist" | tr ',' '\n' | LC_ALL=C sort >"$evidence_dir/migration-allowlist"
printf '%s\n' "$canonical_aliases" | tr ' ' '\n' | LC_ALL=C sort >"$evidence_dir/canonical-aliases"
printf '%s\n' "$predecessor_extras" | tr ' ' '\n' | LC_ALL=C sort >"$evidence_dir/predecessor-extras"

find "$release_dir/server/migrations" -maxdepth 1 -type f -name '*.up.sql' -printf '%f\n' |
  sed 's/\.up\.sql$//' | LC_ALL=C sort >"$evidence_dir/migration-ledger.source"
awk -v allow="$migration_allowlist" -v aliases="$(printf '%s' "$canonical_aliases" | tr ' ' ',')" '
  BEGIN { n=split(allow,a,","); for (i=1;i<=n;i++) skip[a[i]]=1; n=split(aliases,a,","); for (i=1;i<=n;i++) skip[a[i]]=1 }
  !($0 in skip) { print }
' "$evidence_dir/migration-ledger.source" >"$evidence_dir/migration-ledger.expected-before"
for version in $(cat "$evidence_dir/migration-allowlist"); do grep -Fx "$version" "$evidence_dir/migration-ledger.source" >/dev/null; done

docker exec -i multica-postgres-1 sh -c \
  'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version FROM schema_migrations ORDER BY version"' \
  >"$evidence_dir/migration-ledger.before"
awk -v extras="$predecessor_extras" '
  BEGIN { n=split(extras,a," "); for (i=1;i<=n;i++) skip[a[i]]=1 }
  !($0 in skip) { print }
' "$evidence_dir/migration-ledger.before" | LC_ALL=C sort >"$evidence_dir/migration-ledger.before.normalized"
cmp "$evidence_dir/migration-ledger.expected-before" "$evidence_dir/migration-ledger.before.normalized"
for version in $(cat "$evidence_dir/predecessor-extras"); do test "$(grep -Fc "$version" "$evidence_dir/migration-ledger.before")" -eq 1; done

for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.before"
done
docker exec -i multica-postgres-1 sh -c \
  'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  >"$evidence_dir/database.before" <<'SQL'
SELECT 'outbox_identity|' || md5(COALESCE(string_agg(to_jsonb(o)::text, '|' ORDER BY o.event_id), '')) FROM strikeflow_response_outbox o;
SELECT 'reply_identity|' || md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.token_id,r.idempotency_key), '')) FROM strikeflow_connector_reply_receipt r;
SELECT 'content_identity|' || md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.workspace_id,r.recipient_id,r.agent_id,r.idempotency_key), '')) FROM strikeflow_connector_content_reply_receipt r;
SELECT 'outbox_state|' || count(*) || '|' || count(*) FILTER (WHERE delivered_at IS NULL OR needs_attention_at IS NOT NULL) || '|' || count(*) FILTER (WHERE strikeflow_command_id IS NOT NULL) FROM strikeflow_response_outbox;
SQL

freeze_fifo=$evidence_dir/producer-freeze.fifo
mkfifo "$freeze_fifo"
chmod 0600 "$freeze_fifo"
docker exec -i multica-postgres-1 sh -c 'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <"$freeze_fifo" >"$evidence_dir/producer-freeze.log" 2>&1 &
freeze_pid=$!
exec 8>"$freeze_fifo"
printf "SELECT pg_advisory_lock(hashtextextended('multica.strikeflow.response.producer.freeze', 0)); SELECT 'producer_lock_acquired';\n" >&8
locked=false
attempt=0
while [ "$attempt" -lt 30 ]; do
  if grep -Fxq producer_lock_acquired "$evidence_dir/producer-freeze.log"; then locked=true; break; fi
  sleep 1
  attempt=$((attempt + 1))
done
test "$locked" = true

release_freeze() {
  if [ -n "${freeze_pid:-}" ]; then
    exec 8>&- || true
    wait "$freeze_pid" 2>/dev/null || true
    freeze_pid=
  fi
  if [ -p "$freeze_fifo" ]; then rm -f "$freeze_fifo"; fi
}

migrate_overlay=$evidence_dir/migrate-only.yml
printf '%s\n' 'services:' '  backend:' "    image: $image_digest" '    entrypoint: ["./migrate"]' >"$migrate_overlay"
chmod 0600 "$migrate_overlay"
docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" -f "$migrate_overlay" config --format json |
  python3 -c '
import json, sys
backend = json.load(sys.stdin)["services"]["backend"]
if backend.get("image") != sys.argv[1] or backend.get("entrypoint") != ["./migrate"]:
    raise SystemExit("migrate-only render mismatch")
' "$image_digest"

migrator_name=sf-response-migrate-mainline-$evidence_name
record_failure() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  docker stop -t 10 "$migrator_name" >"$evidence_dir/failure-migrator-stop.log" 2>&1 || true
  docker rm -f "$migrator_name" >"$evidence_dir/failure-migrator-remove.log" 2>&1 || true
  release_freeze
  printf 'migration_status=%s\n' "$status" >"$evidence_dir/status.txt"
  docker exec -i multica-postgres-1 sh -c 'psql -X -A -t -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version FROM schema_migrations ORDER BY version"' >"$evidence_dir/migration-ledger.failure-final" 2>&1 || true
  docker exec -i multica-postgres-1 sh -c 'psql -X -A -t -U "$POSTGRES_USER" -d "$POSTGRES_DB"' >"$evidence_dir/database.failure-final" 2>&1 <<'SQL' || true
SELECT 'outbox_state|' || count(*) || '|' || count(*) FILTER (WHERE delivered_at IS NULL OR needs_attention_at IS NOT NULL) FROM strikeflow_response_outbox;
SQL
  for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.failure-final" 2>&1 || true; done
  chmod 0600 "$evidence_dir"/* 2>/dev/null || true
  (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS ! -name producer-freeze.fifo -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
  chmod 0600 "$evidence_dir/SHA256SUMS" 2>/dev/null || true
  exit "$status"
}
trap record_failure EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" -f "$migrate_overlay" \
  run --name "$migrator_name" --rm --no-deps -T -e "MULTICA_MIGRATION_ALLOWLIST=$migration_allowlist" backend up \
  >"$evidence_dir/migrate.log" 2>&1
test -z "$(docker ps -a --filter "name=^/\${migrator_name}$" --format '{{.ID}}')"

"$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
  --before-start --allow-delivered-outbox "$release_dir" "$image_digest" "$preflight_dir" \
  >"$evidence_dir/catalog-and-host-verifier.log"
docker exec -i multica-postgres-1 sh -c \
  'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version FROM schema_migrations ORDER BY version"' \
  >"$evidence_dir/migration-ledger.after"
awk -v extras="$predecessor_extras" -v aliases="900001_strikeflow_response_outbox=253_strikeflow_response_outbox 900002_strikeflow_connector_reply_command_unique=254_strikeflow_connector_reply_command_unique 900003_strikeflow_response_outbox_event_unique=255_strikeflow_response_outbox_event_unique 900004_strikeflow_response_outbox_due_index=256_strikeflow_response_outbox_due_index 900005_strikeflow_response_outbox_event_id_unique=257_strikeflow_response_outbox_event_id_unique" '
  BEGIN { n=split(extras,a," "); for (i=1;i<=n;i++) skip[a[i]]=1; n=split(aliases,a," "); for (i=1;i<=n;i++){ split(a[i],p,"="); map[p[1]]=p[2] } }
  !($0 in skip) { if ($0 in map) $0=map[$0]; print }
' "$evidence_dir/migration-ledger.after" | LC_ALL=C sort >"$evidence_dir/migration-ledger.after.normalized"
cmp "$evidence_dir/migration-ledger.source" "$evidence_dir/migration-ledger.after.normalized"
for version in $(cat "$evidence_dir/predecessor-extras"); do test "$(grep -Fc "$version" "$evidence_dir/migration-ledger.after")" -eq 1; done

docker exec -i multica-postgres-1 sh -c \
  'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  >"$evidence_dir/database.after" <<'SQL'
SELECT 'outbox_identity|' || md5(COALESCE(string_agg(to_jsonb(o)::text, '|' ORDER BY o.event_id), '')) FROM strikeflow_response_outbox o;
SELECT 'reply_identity|' || md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.token_id,r.idempotency_key), '')) FROM strikeflow_connector_reply_receipt r;
SELECT 'content_identity|' || md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.workspace_id,r.recipient_id,r.agent_id,r.idempotency_key), '')) FROM strikeflow_connector_content_reply_receipt r;
SELECT 'outbox_state|' || count(*) || '|' || count(*) FILTER (WHERE delivered_at IS NULL OR needs_attention_at IS NOT NULL) || '|' || count(*) FILTER (WHERE strikeflow_command_id IS NOT NULL) FROM strikeflow_response_outbox;
SQL
cmp "$evidence_dir/database.before" "$evidence_dir/database.after"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container")
  test "$current" = "$(cat "$evidence_dir/$container.before")"
done
curl -fsS http://127.0.0.1:8080/readyz >/dev/null
release_freeze
date -u +%FT%TZ >"$evidence_dir/completed-at.txt"
printf 'migration_status=0\n' >"$evidence_dir/status.txt"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS ! -name producer-freeze.fifo -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
chmod 0600 "$evidence_dir"/*
trap - EXIT HUP INT TERM
echo "multica_mainline_migrations_applied evidence=$evidence_dir"

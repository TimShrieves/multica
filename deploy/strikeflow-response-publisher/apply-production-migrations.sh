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
image_tag=$(sed -n 's/^image_tag=//p' "$release_dir/ARTIFACTS")
artifact_image_digest=$(sed -n 's/^image_digest=//p' "$release_dir/ARTIFACTS")
artifact_image_id=$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")
source_commit=$(sed -n 's/^source_commit=//p' "$release_dir/ARTIFACTS")

case "$backup_file" in /var/backups/multica/*.dump.age) ;; *) echo "invalid encrypted backup path" >&2; exit 1;; esac
case "$backup_sha" in /var/backups/multica/*.sha256) ;; *) echo "invalid backup checksum path" >&2; exit 1;; esac
evidence_parent=$(readlink -f "$(dirname "$evidence_dir")")
evidence_name=$(basename "$evidence_dir")
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in gate-a-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
case "$evidence_name" in *[!A-Za-z0-9._-]*) echo "invalid evidence path" >&2; exit 1;; esac
evidence_dir=$evidence_parent/$evidence_name
test ! -e "$evidence_dir"
test "$(stat -c '%U:%G %a' "$backup_file")" = "root:root 600"
test "$(stat -c '%U:%G %a' "$backup_sha")" = "root:root 600"
test "$(wc -l <"$backup_sha" | tr -d ' ')" -eq 1
expected_backup_hash=$(awk 'NF == 2 { print $1 }' "$backup_sha")
expected_backup_name=$(awk 'NF == 2 { print $2 }' "$backup_sha" | sed 's/^\*//')
case "$expected_backup_hash" in *[!0-9a-fA-F]*|'') echo "invalid backup checksum" >&2; exit 1;; esac
test "${#expected_backup_hash}" -eq 64
test "$expected_backup_name" = "$(basename "$backup_file")"
test "$expected_backup_hash" = "$(sha256sum "$backup_file" | awk '{print $1}')"
exec 9>"$lock_file"
flock -n 9 || { echo "another response deployment is running" >&2; exit 1; }

"$release_dir/deploy/strikeflow-response-publisher/verify-disabled-install.sh" \
  "$release_dir" "$image_tag" "$preflight_dir"
test "$image_digest" = "$artifact_image_digest"
test "$(docker image inspect "$image_digest" --format '{{.Id}}')" = "$artifact_image_id"
docker image inspect "$image_digest" --format '{{range .RepoDigests}}{{println .}}{{end}}' |
  grep -Fqx "$artifact_image_digest"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.source"}}')" = "$source_commit"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.state"}}')" = dormant

install -d -o root -g root -m 0700 "$evidence_dir"
install -o root -g root -m 0600 "$backup_sha" "$evidence_dir/backup.sha256"
printf '%s\n' "$backup_file" >"$evidence_dir/encrypted-backup.path"
chmod 0600 "$evidence_dir/encrypted-backup.path"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.before"
done

docker exec -i multica-postgres-1 sh -c \
  'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  >"$evidence_dir/database.before" <<'SQL'
SELECT 'response_migrations|' || count(*) FROM schema_migrations WHERE version LIKE '90000%';
SELECT 'outbox_exists|' || (to_regclass('public.strikeflow_response_outbox') IS NOT NULL);
SELECT 'reply_receipts|' || count(*) || '|' || md5(string_agg((to_jsonb(r)-'strikeflow_command_id')::text,'|' ORDER BY r.token_id,r.idempotency_key)) FROM strikeflow_connector_reply_receipt r;
SELECT 'command_column_exists|' || EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_connector_reply_receipt' AND column_name='strikeflow_command_id');
SQL
grep -Fx 'response_migrations|0' "$evidence_dir/database.before"
grep -Fx 'outbox_exists|false' "$evidence_dir/database.before"
grep -Fx 'command_column_exists|false' "$evidence_dir/database.before"

migrate_overlay=$evidence_dir/migrate-only.yml
printf '%s\n' \
  'services:' \
  '  backend:' \
  "    image: $image_digest" \
  '    entrypoint: ["./migrate"]' \
  >"$migrate_overlay"
chmod 0600 "$migrate_overlay"
docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" -f "$migrate_overlay" config --format json |
python3 -c '
import json, sys
backend = json.load(sys.stdin)["services"]["backend"]
if backend.get("image") != sys.argv[1] or backend.get("entrypoint") != ["./migrate"]:
    raise SystemExit("migrate-only render mismatch")
' "$image_digest"

migrator_name=sf-response-migrate-gate-a-$evidence_name
record_failure() {
  status=$?
  trap - EXIT HUP INT TERM
  docker stop -t 10 "$migrator_name" >"$evidence_dir/failure-migrator-stop.log" 2>&1 || true
  docker rm -f "$migrator_name" >"$evidence_dir/failure-migrator-remove.log" 2>&1 || true
  printf 'migration_status=%s\n' "$status" >"$evidence_dir/status.txt"
  docker ps -a --filter "name=^/${migrator_name}$" --format '{{.ID}}|{{.Status}}' >"$evidence_dir/failure-residual-migrator.txt" 2>&1 || true
  for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
    docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
      "$container" >"$evidence_dir/$container.failure-final" 2>&1 || true
  done
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
    >"$evidence_dir/database.failure-final" 2>&1 <<'SQL' || true
SELECT 'response_migrations|' || count(*) FROM schema_migrations WHERE version LIKE '90000%';
SELECT 'outbox_exists|' || (to_regclass('public.strikeflow_response_outbox') IS NOT NULL);
SELECT 'reply_receipts|' || count(*) || '|' || md5(string_agg((to_jsonb(r)-'strikeflow_command_id')::text,'|' ORDER BY r.token_id,r.idempotency_key)) FROM strikeflow_connector_reply_receipt r;
SQL
  chmod 0600 "$evidence_dir"/* 2>/dev/null || true
  (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
  chmod 0600 "$evidence_dir"/SHA256SUMS 2>/dev/null || true
  exit "$status"
}
trap record_failure EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" -f "$migrate_overlay" \
  run --name "$migrator_name" --rm --no-deps -T backend up >"$evidence_dir/migrate.log" 2>&1

"$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
  --before-start "$release_dir" "$image_digest" "$preflight_dir" \
  >"$evidence_dir/catalog-and-host-verifier.log"
docker exec -i multica-postgres-1 sh -c \
  'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  >"$evidence_dir/database.after" <<'SQL'
SELECT 'response_migrations|' || count(*) FROM schema_migrations WHERE version LIKE '90000%';
SELECT 'outbox_rows|' || count(*) FROM strikeflow_response_outbox;
SELECT 'needs_attention|' || count(*) FROM strikeflow_response_outbox WHERE needs_attention_at IS NOT NULL;
SELECT 'reply_receipts|' || count(*) || '|' || md5(string_agg((to_jsonb(r)-'strikeflow_command_id')::text,'|' ORDER BY r.token_id,r.idempotency_key)) FROM strikeflow_connector_reply_receipt r;
SELECT 'non_null_command_bindings|' || count(*) FROM strikeflow_connector_reply_receipt WHERE strikeflow_command_id IS NOT NULL;
SQL
grep -Fx 'response_migrations|5' "$evidence_dir/database.after"
grep -Fx 'outbox_rows|0' "$evidence_dir/database.after"
grep -Fx 'needs_attention|0' "$evidence_dir/database.after"
grep -Fx 'non_null_command_bindings|0' "$evidence_dir/database.after"
test "$(grep '^reply_receipts|' "$evidence_dir/database.before")" = "$(grep '^reply_receipts|' "$evidence_dir/database.after")"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container")
  test "$current" = "$(cat "$evidence_dir/$container.before")"
done
test -z "$(docker ps -a --filter "name=^/${migrator_name}$" --format '{{.ID}}')"

date -u +%FT%TZ >"$evidence_dir/completed-at.txt"
printf 'migration_status=0\n' >"$evidence_dir/status.txt"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
chmod 0600 "$evidence_dir"/*
trap - EXIT HUP INT TERM
echo "multica_response_migrations_applied evidence=$evidence_dir"

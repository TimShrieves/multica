#!/bin/sh
set -eu

if [ "$#" -ne 5 ] || [ "$5" != "--confirm-deploy-disabled" ]; then
  echo "usage: $0 RELEASE_DIR IMAGE_DIGEST ORIGINAL_PREFLIGHT EVIDENCE_DIR --confirm-deploy-disabled" >&2
  exit 64
fi

release_dir=$(readlink -f "$1")
image_digest=$2
preflight_dir=$(readlink -f "$3")
evidence_dir=$4
base_compose=/opt/multica/docker-compose.selfhost.yml
pin_compose=/opt/multica/docker-compose.pin.yml
overlay=$release_dir/docker-compose.strikeflow-response-candidate-disabled.yml
base_env=/opt/multica/.env
disabled_env=$release_dir/deploy/strikeflow-response-publisher/publisher.env.disabled
lock_file=/run/lock/multica-response-publisher-deploy.lock

evidence_parent=$(readlink -f "$(dirname "$evidence_dir")")
evidence_name=$(basename "$evidence_dir")
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in candidate-disabled-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
case "$evidence_name" in *[!A-Za-z0-9._-]*) echo "invalid evidence path" >&2; exit 1;; esac
evidence_dir=$evidence_parent/$evidence_name
test ! -e "$evidence_dir"
exec 9>"$lock_file"
flock -n 9 || { echo "another response deployment is running" >&2; exit 1; }

wait_for_ready() {
  attempt=0
  while [ "$attempt" -lt 30 ]; do
    if wget -q -O /dev/null http://127.0.0.1:8080/readyz; then return 0; fi
    attempt=$((attempt + 1))
    sleep 2
  done
  return 1
}

expected_original_image=$(cut -d'|' -f2 "$preflight_dir/multica-backend-1.identity")
expected_original_ports=$(cut -d'|' -f4 "$preflight_dir/multica-backend-1.identity")
assert_original_backend() {
  test "$(docker inspect multica-backend-1 --format '{{.Image}}')" = "$expected_original_image"
  test "$(docker inspect multica-backend-1 --format '{{json .NetworkSettings.Ports}}')" = "$expected_original_ports"
  test "$(docker inspect multica-backend-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')" = "$base_compose,$pin_compose"
  docker inspect multica-backend-1 --format '{{json .Mounts}}' |
    python3 -c 'import json,sys; raise SystemExit(any(m.get("Destination")=="/run/secrets/strikeflow_response_hmac" for m in json.load(sys.stdin)))'
  wait_for_ready
}

restore_original_backend() {
  docker compose --project-directory /opt/multica --env-file "$base_env" \
    -f "$base_compose" -f "$pin_compose" \
    up -d --no-deps --force-recreate backend || return 1
  assert_original_backend
}

restore_disabled_candidate() {
  STRIKEFLOW_RESPONSE_BACKEND_IMAGE=$image_digest \
  docker compose --project-directory /opt/multica --env-file "$base_env" \
    --env-file "$disabled_env" \
    -f "$base_compose" -f "$pin_compose" -f "$overlay" \
    up -d --no-deps --force-recreate backend || return 1
  wait_for_ready || return 1
  "$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
    --allow-delivered-outbox "$release_dir" "$image_digest" "$preflight_dir"
}

"$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
  --migration-preflight --allow-delivered-outbox "$release_dir" "$image_digest" "$preflight_dir"
restored_image_ref=$(docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" config --format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["backend"]["image"])')
test -n "$restored_image_ref"
test "$(docker image inspect "$restored_image_ref" --format '{{.Id}}')" = "$expected_original_image"

install -d -o root -g root -m 0700 "$evidence_dir"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.before"
done
STRIKEFLOW_RESPONSE_BACKEND_IMAGE=$image_digest \
docker compose --project-directory /opt/multica --env-file "$base_env" \
  --env-file "$disabled_env" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" config --format json |
  sha256sum >"$evidence_dir/rendered-compose.sha256"

database_fingerprint() {
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
SELECT 'migration_ledger|' || md5(string_agg(version,'|' ORDER BY version)) FROM schema_migrations;
SELECT 'reply_receipts|' || count(*) || '|' || md5(string_agg(to_jsonb(r)::text,'|' ORDER BY r.token_id,r.idempotency_key)) FROM strikeflow_connector_reply_receipt r;
SELECT 'outbox|' || count(*) || '|' || count(*) FILTER (WHERE delivered_at IS NULL) || '|' || count(*) FILTER (WHERE needs_attention_at IS NOT NULL) FROM strikeflow_response_outbox;
SQL
}
database_fingerprint >"$evidence_dir/database.before"

backend_changed=false
deployment_verified=false
fail_closed() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  if [ "$backend_changed" = true ] && [ "$deployment_verified" != true ]; then
    set +e
    restore_original_backend >"$evidence_dir/failure-restore-original.log" 2>&1
    restore_status=$?
    disabled_status=not_attempted
    if [ "$restore_status" -ne 0 ]; then
      restore_disabled_candidate >"$evidence_dir/failure-restore-disabled.log" 2>&1
      disabled_status=$?
    fi
    printf 'restore_original_status=%s\nrestore_disabled_status=%s\n' \
      "$restore_status" "$disabled_status" >"$evidence_dir/failure-status.txt"
    docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
      multica-backend-1 >"$evidence_dir/multica-backend-1.failure-final" 2>&1
    database_fingerprint >"$evidence_dir/database.failure-final" 2>&1 || true
    chmod 0600 "$evidence_dir"/* 2>/dev/null || true
    (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
    chmod 0600 "$evidence_dir"/SHA256SUMS 2>/dev/null || true
  fi
  exit "$status"
}
trap fail_closed EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

backend_changed=true
STRIKEFLOW_RESPONSE_BACKEND_IMAGE=$image_digest \
docker compose --project-directory /opt/multica --env-file "$base_env" \
  --env-file "$disabled_env" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" \
  up -d --no-deps --force-recreate backend

wait_for_ready
"$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
  --allow-delivered-outbox "$release_dir" "$image_digest" "$preflight_dir" >"$evidence_dir/verifier.after.log"
database_fingerprint >"$evidence_dir/database.after"
cmp -s "$evidence_dir/database.before" "$evidence_dir/database.after"
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}|{{json .Config.Entrypoint}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.after"
date -u +%FT%TZ >"$evidence_dir/deployed-at.txt"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
chmod 0600 "$evidence_dir"/*
deployment_verified=true
trap - EXIT HUP INT TERM
echo "multica_response_candidate_deployed_disabled evidence=$evidence_dir"

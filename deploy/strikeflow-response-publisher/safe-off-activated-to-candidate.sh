#!/bin/sh
set -eu

if [ "$#" -ne 6 ]; then
  echo "usage: $0 RELEASE_DIR IMAGE_DIGEST ORIGINAL_PREFLIGHT STARTING_PREFLIGHT EVIDENCE_DIR (--confirm-safe-off-to-candidate|--confirm-emergency-safe-off-to-candidate)" >&2
  exit 64
fi
case "$6" in
  --confirm-safe-off-to-candidate) safe_off_mode=drained; disabled_verify_mode=--allow-delivered-outbox ;;
  --confirm-emergency-safe-off-to-candidate) safe_off_mode=preserve; disabled_verify_mode=--preserve-outbox ;;
  *) echo "invalid safe-off confirmation" >&2; exit 64 ;;
esac

release_dir=$(readlink -f "$1")
image_digest=$2
original_preflight=$(readlink -f "$3")
starting_preflight=$(readlink -f "$4")
evidence_dir=$5
config_file=/etc/multica-response-publisher/publisher.env
base_compose=/opt/multica/docker-compose.selfhost.yml
pin_compose=/opt/multica/docker-compose.pin.yml
disabled_overlay=$release_dir/docker-compose.strikeflow-response-candidate-disabled.yml
disabled_env=$release_dir/deploy/strikeflow-response-publisher/publisher.env.disabled
base_env=/opt/multica/.env
lock_file=/run/lock/multica-response-publisher-deploy.lock
reconciliation_timer=strikeflow-multica-content-dispatch.timer
reconciliation_service=strikeflow-multica-content-dispatch.service
ongoing_timer=strikeflow-multica-content-ongoing.timer
ongoing_service=strikeflow-multica-content-ongoing.service

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

database_fingerprint() {
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
SELECT 'migration_ledger|' || md5(string_agg(version,'|' ORDER BY version)) FROM schema_migrations;
SELECT 'reply_receipts|' || count(*) || '|' || md5(string_agg(to_jsonb(r)::text,'|' ORDER BY r.token_id,r.idempotency_key)) FROM strikeflow_connector_reply_receipt r;
SELECT 'outbox|' || count(*) || '|' || count(*) FILTER (WHERE delivered_at IS NULL) || '|' || count(*) FILTER (WHERE needs_attention_at IS NOT NULL) || '|' || md5(COALESCE(string_agg(to_jsonb(o)::text,'|' ORDER BY o.event_id),'')) FROM strikeflow_response_outbox o;
SQL
}

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
  docker compose --project-directory /opt/multica \
    --env-file "$base_env" --env-file "$disabled_env" \
    -f "$base_compose" -f "$pin_compose" -f "$disabled_overlay" \
    up -d --no-deps --force-recreate backend || return 1
  wait_for_ready || return 1
  "$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
    "$disabled_verify_mode" "$release_dir" "$image_digest" "$starting_preflight"
}

install_disabled_config() {
  disabled_tmp=$(mktemp /etc/multica-response-publisher/publisher.env.disabled.XXXXXX)
  install -o root -g root -m 0600 "$disabled_env" "$disabled_tmp"
  mv "$disabled_tmp" "$config_file"
}

capture_reconciliation_state() {
  output=$1
  systemctl show "$reconciliation_timer" "$reconciliation_service" "$ongoing_timer" "$ongoing_service" \
    --property=Id,LoadState,ActiveState,SubState,UnitFileState,Result,MainPID >"$output"
}

# systemctl is-enabled exits successfully for static units even though they
# cannot be enabled. Treat only explicit enablement states as enabled; unknown
# states fail closed so safe-off never reports quiescence prematurely.
systemd_unit_is_enabled() {
  unit=$1
  enabled_state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  case "$enabled_state" in
    enabled|enabled-runtime|linked|linked-runtime|alias|generated|transient)
      return 0
      ;;
    disabled|static|indirect|masked)
      return 1
      ;;
    *)
      return 0
      ;;
  esac
}

systemd_unit_main_pid_is_zero() {
  unit=$1
  main_pid=$(systemctl show "$unit" --property=MainPID --value 2>/dev/null || true)
  case "$main_pid" in
    ""|0) return 0 ;;
    *) return 1 ;;
  esac
}

stop_response_reconciliation() {
  # New response receipts or fallback reconciliation must not race publisher
  # safe-off. This operation is intentionally one-way; rollback never restarts
  # continuous scheduling without a separate activation approval.
  stop_attempt=0
  while [ "$stop_attempt" -lt 3 ]; do
    systemctl disable --now "$reconciliation_timer" "$ongoing_timer" >/dev/null 2>&1 || true
    systemctl stop "$reconciliation_service" "$ongoing_service" >/dev/null 2>&1 || true
    if ! systemctl is-active --quiet "$reconciliation_timer" \
       && ! systemctl is-active --quiet "$reconciliation_service" \
       && ! systemctl is-active --quiet "$ongoing_timer" \
       && ! systemctl is-active --quiet "$ongoing_service" \
       && ! systemd_unit_is_enabled "$reconciliation_timer" \
       && ! systemd_unit_is_enabled "$ongoing_timer" \
       && systemd_unit_main_pid_is_zero "$reconciliation_timer" \
       && systemd_unit_main_pid_is_zero "$reconciliation_service" \
       && systemd_unit_main_pid_is_zero "$ongoing_timer" \
       && systemd_unit_main_pid_is_zero "$ongoing_service"; then
      return 0
    fi
    stop_attempt=$((stop_attempt + 1))
  done
  return 1
}

evidence_parent=$(readlink -f "$(dirname "$evidence_dir")")
evidence_name=$(basename "$evidence_dir")
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in safe-off-activated-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
case "$evidence_name" in *[!A-Za-z0-9._-]*) echo "invalid evidence path" >&2; exit 1;; esac
evidence_dir=$evidence_parent/$evidence_name
test ! -e "$evidence_dir"
case "$original_preflight" in /var/backups/multica-response-publisher/*) ;; *) echo "invalid original preflight" >&2; exit 1;; esac
case "$starting_preflight" in /var/backups/multica-response-publisher/*) ;; *) echo "invalid starting preflight" >&2; exit 1;; esac
test "$original_preflight" != "$starting_preflight"
test "$(stat -c '%U:%G %a' "$original_preflight")" = "root:root 700"
test "$(stat -c '%U:%G %a' "$starting_preflight")" = "root:root 700"
test -z "$(find "$original_preflight" "$starting_preflight" -xdev -type l -print -quit)"
test "$(readlink -f /opt/multica-response-publisher/current)" = "$release_dir"
test "$(stat -c '%U:%G %a' "$config_file")" = "root:root 600"
test "$(sed -n 's/^image_digest=//p' "$release_dir/ARTIFACTS")" = "$image_digest"

# Refuse to safe-off anything except the exact enabled, single-command canary.
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  --rollback-preflight "$release_dir" "$image_digest" "$starting_preflight"
python3 - "$config_file" <<'PY'
import pathlib, uuid, sys
values = dict(line.split("=", 1) for line in pathlib.Path(sys.argv[1]).read_text().splitlines())
mode = values.get("STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE")
commands = [value.strip() for value in values.get("STRIKEFLOW_RESPONSE_COMMAND_IDS", "").split(",") if value.strip()]
if mode == "explicit_commands":
    if len(commands) != 1:
        raise SystemExit("explicit_commands safe-off requires exactly one canary command")
    uuid.UUID(commands[0])
elif mode == "receipt_lineage":
    if commands:
        raise SystemExit("receipt_lineage safe-off requires an empty command list")
else:
    raise SystemExit("safe-off authorization mode is invalid")
PY
unsafe_outbox_count=$(docker exec -i multica-postgres-1 sh -c \
  'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM strikeflow_response_outbox WHERE delivered_at IS NULL OR needs_attention_at IS NOT NULL"')
if [ "$safe_off_mode" = drained ]; then
  test "$unsafe_outbox_count" = 0
fi

expected_original_image=$(cut -d'|' -f2 "$original_preflight/multica-backend-1.identity")
expected_original_ports=$(cut -d'|' -f4 "$original_preflight/multica-backend-1.identity")
starting_image=$(cut -d'|' -f2 "$starting_preflight/multica-backend-1.identity")
candidate_image=$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")
test "$starting_image" = "$candidate_image"
test "$expected_original_image" != "$starting_image"
for container in multica-frontend-1 multica-postgres-1; do
  cmp -s "$original_preflight/$container.identity" "$starting_preflight/$container.identity"
done
(cd / && sha256sum -c "$original_preflight/active-compose.sha256" >/dev/null)
(cd / && sha256sum -c "$starting_preflight/active-compose.sha256" >/dev/null)
restored_image_ref=$(docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" config --format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["backend"]["image"])')
test -n "$restored_image_ref"
test "$(docker image inspect "$restored_image_ref" --format '{{.Id}}')" = "$expected_original_image"

install -d -o root -g root -m 0700 "$evidence_dir"
printf '%s\n' "$original_preflight" >"$evidence_dir/original-preflight.txt"
printf '%s\n' "$starting_preflight" >"$evidence_dir/starting-preflight.txt"
printf '%s\n' "$safe_off_mode" >"$evidence_dir/safe-off-mode.txt"
install -o root -g root -m 0600 "$config_file" "$evidence_dir/publisher.env.enabled"
sed -n 's/^STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE=//p' "$config_file" >"$evidence_dir/authorization-mode.txt"
capture_reconciliation_state "$evidence_dir/response-reconciliation.before"
database_fingerprint >"$evidence_dir/database.before"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}|{{json .Config.Entrypoint}}' \
    "$container" >"$evidence_dir/$container.before"
done

backend_changed=false
safe_off_started=false
safe_off_verified=false
record_failure() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  if { [ "$backend_changed" = true ] || [ "$safe_off_started" = true ]; } && [ "$safe_off_verified" != true ]; then
    set +e
    stop_response_reconciliation >"$evidence_dir/failure-stop-reconciliation.log" 2>&1
    reconciliation_stop_status=$?
    disabled_verify_mode=--preserve-outbox
    restore_disabled_candidate >"$evidence_dir/failure-restore-disabled.log" 2>&1
    disabled_status=$?
    original_status=not_attempted
    if [ "$disabled_status" -ne 0 ]; then
      restore_original_backend >"$evidence_dir/failure-restore-original.log" 2>&1
      original_status=$?
    fi
    install_disabled_config >"$evidence_dir/failure-install-disabled-config.log" 2>&1
    config_status=$?
    printf 'safe_off_status=%s\nreconciliation_stop_status=%s\nrestore_disabled_status=%s\nrestore_original_status=%s\nconfig_status=%s\n' \
      "$status" "$reconciliation_stop_status" "$disabled_status" "$original_status" "$config_status" >"$evidence_dir/failure-status.txt"
    database_fingerprint >"$evidence_dir/database.failure-final" 2>&1 || true
    docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}|{{json .Mounts}}' \
      multica-backend-1 >"$evidence_dir/multica-backend-1.failure-final" 2>&1 || true
  fi
  capture_reconciliation_state "$evidence_dir/response-reconciliation.failure-final" 2>/dev/null || true
  chmod 0600 "$evidence_dir"/* 2>/dev/null || true
  (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
  chmod 0600 "$evidence_dir"/SHA256SUMS 2>/dev/null || true
  exit "$status"
}
trap record_failure EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

safe_off_started=true
stop_response_reconciliation
capture_reconciliation_state "$evidence_dir/response-reconciliation.after-stop"

backend_changed=true
restore_disabled_candidate >"$evidence_dir/safe-off-compose-and-verify.log" 2>&1
database_fingerprint >"$evidence_dir/database.after"
cmp -s "$evidence_dir/database.before" "$evidence_dir/database.after"
for container in multica-frontend-1 multica-postgres-1; do
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container")
  test "$current" = "$(cat "$starting_preflight/$container.identity")"
done
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}|{{json .Config.Entrypoint}}|{{json .Mounts}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.disabled"

# Restore the tracked blank environment only after the running backend is
# proven disabled and secret-free. Preserve the HMAC credential for later gates.
install_disabled_config
test "$(sed -n 's/^STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=//p' "$config_file")" = false
test -z "$(sed -n '/^STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=/!s/^[^=]*=\(.*\)$/\1/p' "$config_file" | sed '/^$/d')"
date -u +%FT%TZ >"$evidence_dir/safe-off-at.txt"
capture_reconciliation_state "$evidence_dir/response-reconciliation.final"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
chmod 0600 "$evidence_dir"/*
safe_off_verified=true
trap - EXIT HUP INT TERM
echo "multica_response_publisher_safe_off_candidate evidence=$evidence_dir"

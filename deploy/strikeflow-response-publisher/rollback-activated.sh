#!/bin/sh
set -eu

if [ "$#" -ne 5 ] || [ "$5" != "--confirm-rollback" ]; then
  echo "usage: $0 RELEASE_DIR ORIGINAL_PREFLIGHT STARTING_PREFLIGHT EVIDENCE_DIR --confirm-rollback" >&2
  exit 64
fi
release_dir=$(readlink -f "$1")
original_preflight=$(readlink -f "$2")
starting_preflight=$(readlink -f "$3")
evidence_dir=$4
config_file=/etc/multica-response-publisher/publisher.env
base_compose=/opt/multica/docker-compose.selfhost.yml
pin_compose=/opt/multica/docker-compose.pin.yml
overlay=$release_dir/docker-compose.strikeflow-response-publisher.yml
disabled_overlay=$release_dir/docker-compose.strikeflow-response-candidate-disabled.yml
disabled_env=$release_dir/deploy/strikeflow-response-publisher/publisher.env.disabled
base_env=/opt/multica/.env
lock_file=/run/lock/multica-response-publisher-deploy.lock

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

assert_original_backend() {
  test "$(docker inspect multica-backend-1 --format '{{.Image}}')" = "$expected_image"
  test "$(docker inspect multica-backend-1 --format '{{json .NetworkSettings.Ports}}')" = "$expected_ports"
  test "$(docker inspect multica-backend-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')" = "$base_compose,$pin_compose"
  docker inspect multica-backend-1 --format '{{json .Mounts}}' |
    python3 -c 'import json,sys; mounts=json.load(sys.stdin); raise SystemExit(any(m.get("Destination")=="/run/secrets/strikeflow_response_hmac" for m in mounts))'
  wait_for_ready
}

assert_activation_overlay() {
  docker compose --project-directory /opt/multica \
    --env-file "$base_env" --env-file "$config_file" \
    -f "$base_compose" -f "$pin_compose" -f "$overlay" config --format json |
    python3 -c '
import json, sys
backend = json.load(sys.stdin)["services"]["backend"]
if backend.get("image") != sys.argv[1]:
    raise SystemExit("activation image is not the sealed digest")
if backend.get("entrypoint") != ["./server"]:
    raise SystemExit("activation must bypass the migration entrypoint")
' "$image_digest"
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
    --preserve-outbox "$release_dir" "$image_digest" "$starting_preflight"
}

install_disabled_config() {
  disabled_tmp=$(mktemp /etc/multica-response-publisher/publisher.env.disabled.XXXXXX)
  install -o root -g root -m 0600 "$disabled_env" "$disabled_tmp"
  mv "$disabled_tmp" "$config_file"
}

evidence_parent=$(readlink -f "$(dirname "$evidence_dir")")
evidence_name=$(basename "$evidence_dir")
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in rollback-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
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
# Refuse to mutate an unrecognized or drifted runtime. Emergency recovery from
# drift requires a separate operator-reviewed procedure and evidence set.
image_digest=$(sed -n 's/^STRIKEFLOW_RESPONSE_BACKEND_IMAGE=//p' "$config_file")
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  --rollback-preflight "$release_dir" "$image_digest" "$starting_preflight"
expected_image=$(cut -d'|' -f2 "$original_preflight/multica-backend-1.identity")
expected_ports=$(cut -d'|' -f4 "$original_preflight/multica-backend-1.identity")
starting_image=$(cut -d'|' -f2 "$starting_preflight/multica-backend-1.identity")
candidate_image=$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")
test "$starting_image" = "$candidate_image"
test "$expected_image" != "$starting_image"
for container in multica-frontend-1 multica-postgres-1; do
  cmp -s "$original_preflight/$container.identity" "$starting_preflight/$container.identity"
done
(cd / && sha256sum -c "$original_preflight/active-compose.sha256" >/dev/null)
(cd / && sha256sum -c "$starting_preflight/active-compose.sha256" >/dev/null)
assert_activation_overlay
restored_image_ref=$(docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" config --format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["backend"]["image"])')
test -n "$restored_image_ref"
test "$(docker image inspect "$restored_image_ref" --format '{{.Id}}')" = "$expected_image"
install -d -o root -g root -m 0700 "$evidence_dir"
printf '%s\n' "$original_preflight" >"$evidence_dir/original-preflight.txt"
printf '%s\n' "$starting_preflight" >"$evidence_dir/starting-preflight.txt"
install -o root -g root -m 0600 "$config_file" "$evidence_dir/publisher.env.before-rollback"

# First recreate the candidate with the publisher false. Environment changes
# are not live until recreate, so editing the file alone is never considered
# safe-off evidence.
safe_off=$(mktemp /etc/multica-response-publisher/publisher.env.safe-off.XXXXXX)
chmod 0600 "$safe_off"
sed 's/^STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=.*/STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=false/' \
  "$config_file" >"$safe_off"
rollback_complete=false
rollback_failure() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  if [ "$rollback_complete" != true ]; then
    set +e
    restore_disabled_candidate >"$evidence_dir/failure-restore-disabled.log" 2>&1
    disabled_status=$?
    restore_status=not_attempted
    if [ "$disabled_status" -ne 0 ]; then
      restore_original_backend >"$evidence_dir/failure-restore-original.log" 2>&1
      restore_status=$?
    fi
    install_disabled_config >"$evidence_dir/failure-install-disabled-config.log" 2>&1
    config_status=$?
    printf 'restore_disabled_status=%s\nrestore_original_status=%s\nconfig_status=%s\n' \
      "$disabled_status" "$restore_status" "$config_status" >"$evidence_dir/failure-status.txt"
    if [ -e "$safe_off" ]; then
      mv "$safe_off" "$evidence_dir/publisher.env.safe-off"
    fi
    chmod 0600 "$evidence_dir"/* 2>/dev/null || true
    (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
    chmod 0600 "$evidence_dir"/SHA256SUMS 2>/dev/null || true
  fi
  exit "$status"
}
trap rollback_failure EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
STRIKEFLOW_RESPONSE_BACKEND_IMAGE=$image_digest \
docker compose --project-directory /opt/multica \
  --env-file "$base_env" --env-file "$disabled_env" \
  -f "$base_compose" -f "$pin_compose" -f "$disabled_overlay" \
  up -d --no-deps --force-recreate backend
"$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
  --preserve-outbox "$release_dir" "$image_digest" "$starting_preflight"
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.safe-off"

# Then restore and prove the exact original base+pin backend image and original
# two-file Compose project. Migrations/outbox/audit evidence are retained.
restore_original_backend

install_disabled_config
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.restored"
date -u +%FT%TZ >"$evidence_dir/rolled-back-at.txt"
mv "$safe_off" "$evidence_dir/publisher.env.safe-off"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
rollback_complete=true
trap - EXIT HUP INT TERM
chmod 0600 "$evidence_dir"/*
echo "multica_response_publisher_rolled_back evidence=$evidence_dir"

#!/bin/sh
set -eu

if [ "$#" -ne 4 ] || [ "$4" != "--confirm-rollback" ]; then
  echo "usage: $0 RELEASE_DIR PREFLIGHT_DIR EVIDENCE_DIR --confirm-rollback" >&2
  exit 64
fi
release_dir=$(readlink -f "$1")
preflight_dir=$(readlink -f "$2")
evidence_dir=$3
config_file=/etc/multica-response-publisher/publisher.env
base_compose=/opt/multica/docker-compose.selfhost.yml
pin_compose=/opt/multica/docker-compose.pin.yml
overlay=$release_dir/docker-compose.strikeflow-response-publisher.yml
base_env=/opt/multica/.env

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
  test "$(docker inspect multica-backend-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')" = "$base_compose,$pin_compose"
  docker inspect multica-backend-1 --format '{{json .Mounts}}' |
    python3 -c 'import json,sys; mounts=json.load(sys.stdin); raise SystemExit(any(m.get("Destination")=="/run/secrets/strikeflow_response_hmac" for m in mounts))'
  wait_for_ready
}

restore_original_backend() {
  docker compose --project-directory /opt/multica --env-file "$base_env" \
    -f "$base_compose" -f "$pin_compose" \
    up -d --no-deps --force-recreate backend || return 1
  assert_original_backend
}

case "$evidence_dir" in /var/backups/multica-response-publisher/rollback-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
test ! -e "$evidence_dir"
test "$(readlink -f /opt/multica-response-publisher/current)" = "$release_dir"
test "$(stat -c '%U:%G %a' "$config_file")" = "root:root 600"
# Refuse to mutate an unrecognized or drifted runtime. Emergency recovery from
# drift requires a separate operator-reviewed procedure and evidence set.
image_digest=$(sed -n 's/^STRIKEFLOW_RESPONSE_BACKEND_IMAGE=//p' "$config_file")
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  --rollback-preflight "$release_dir" "$image_digest" "$preflight_dir"
expected_image=$(cut -d'|' -f2 "$preflight_dir/multica-backend-1.identity")
(cd / && sha256sum -c "$preflight_dir/active-compose.sha256" >/dev/null)
restored_image_ref=$(docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" config --format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["backend"]["image"])')
test -n "$restored_image_ref"
test "$(docker image inspect "$restored_image_ref" --format '{{.Id}}')" = "$expected_image"
install -d -o root -g root -m 0700 "$evidence_dir"
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
  trap - EXIT HUP INT TERM
  if [ "$rollback_complete" != true ]; then
    set +e
    restore_original_backend >"$evidence_dir/failure-restore-original.log" 2>&1
    restore_status=$?
    printf 'restore_original_status=%s\n' "$restore_status" >"$evidence_dir/failure-status.txt"
    if [ -e "$safe_off" ]; then
      mv "$safe_off" "$evidence_dir/publisher.env.safe-off"
    fi
    chmod 0600 "$evidence_dir"/* 2>/dev/null || true
  fi
  exit "$status"
}
trap rollback_failure EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
docker compose --project-directory /opt/multica \
  --env-file "$base_env" --env-file "$safe_off" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" \
  up -d --no-deps --force-recreate backend
test "$(docker inspect multica-backend-1 --format '{{.State.Running}}')" = true
docker inspect multica-backend-1 --format '{{json .Config.Env}}' |
python3 -c '
import json, sys
env = dict(item.split("=", 1) for item in json.load(sys.stdin) if "=" in item)
if env.get("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED") != "false":
    raise SystemExit("candidate was not recreated fail-closed")
'
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.safe-off"

# Then restore and prove the exact pre-activation backend image and original
# two-file Compose project. Migrations/outbox/audit evidence are retained.
restore_original_backend

install -o root -g root -m 0600 \
  "$release_dir/deploy/strikeflow-response-publisher/publisher.env.disabled" "$config_file"
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.restored"
date -u +%FT%TZ >"$evidence_dir/rolled-back-at.txt"
mv "$safe_off" "$evidence_dir/publisher.env.safe-off"
rollback_complete=true
trap - EXIT HUP INT TERM
chmod 0600 "$evidence_dir"/*
echo "multica_response_publisher_rolled_back evidence=$evidence_dir"

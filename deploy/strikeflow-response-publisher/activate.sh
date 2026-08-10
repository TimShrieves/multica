#!/bin/sh
set -eu

if [ "$#" -ne 5 ] || [ "$5" != "--confirm-activate" ]; then
  echo "usage: $0 RELEASE_DIR IMAGE_DIGEST PREFLIGHT_DIR EVIDENCE_DIR --confirm-activate" >&2
  exit 64
fi
release_dir=$(readlink -f "$1")
image_digest=$2
preflight_dir=$(readlink -f "$3")
evidence_dir=$4
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

case "$evidence_dir" in /var/backups/multica-response-publisher/activation-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
test ! -e "$evidence_dir"
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  --before-start "$release_dir" "$image_digest" "$preflight_dir"
expected_image=$(cut -d'|' -f2 "$preflight_dir/multica-backend-1.identity")
(cd / && sha256sum -c "$preflight_dir/active-compose.sha256" >/dev/null)
restored_image_ref=$(docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" config --format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["backend"]["image"])')
test -n "$restored_image_ref"
test "$(docker image inspect "$restored_image_ref" --format '{{.Id}}')" = "$expected_image"

install -d -o root -g root -m 0700 "$evidence_dir"
install -o root -g root -m 0600 "$config_file" "$evidence_dir/publisher.env.enabled"
sha256sum "$evidence_dir/publisher.env.enabled" >"$evidence_dir/publisher.env.enabled.sha256"
safe_off=$evidence_dir/publisher.env.safe-off
sed 's/^STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=.*/STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=false/' \
  "$config_file" >"$safe_off"
chmod 0600 "$safe_off"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.before"
done
docker compose --project-directory /opt/multica \
  --env-file "$base_env" --env-file "$config_file" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" config --format json |
  sha256sum >"$evidence_dir/rendered-compose.sha256"

backend_changed=false
activation_verified=false
fail_closed() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$backend_changed" = true ] && [ "$activation_verified" != true ]; then
    set +e
    docker compose --project-directory /opt/multica \
      --env-file "$base_env" --env-file "$safe_off" \
      -f "$base_compose" -f "$pin_compose" -f "$overlay" \
      up -d --no-deps --force-recreate backend \
      >"$evidence_dir/fail-closed-compose.log" 2>&1
    safe_status=$?
    docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .Config.Env}}' \
      multica-backend-1 >"$evidence_dir/multica-backend-1.fail-closed" 2>&1
    inspect_status=$?
    docker inspect multica-backend-1 --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null |
      grep -Fx 'STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=false' \
      >"$evidence_dir/fail-closed-disabled.txt"
    disabled_status=$?
    # Always finish on the original two-file backend. This removes the HMAC
    # bind mount as well as disabling the publisher; the additive response
    # schema and all outbox/audit evidence remain intact.
    restore_original_backend >"$evidence_dir/fail-closed-fallback.log" 2>&1
    fallback_status=$?
    assert_original_backend >"$evidence_dir/fail-closed-original-verify.log" 2>&1
    original_verify_status=$?
    printf 'compose_status=%s\ninspect_status=%s\ndisabled_status=%s\nfallback_status=%s\n' \
      "$safe_status" "$inspect_status" "$disabled_status" "$fallback_status" \
      >"$evidence_dir/fail-closed-status.txt"
    printf 'original_verify_status=%s\n' "$original_verify_status" >>"$evidence_dir/fail-closed-status.txt"
    chmod 0600 "$evidence_dir"/*
  fi
  exit "$status"
}
trap fail_closed EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Migrations must already pass the semantic catalog gate above. This command
# therefore recreates only the backend and cannot become an implicit SQL gate.
backend_changed=true
docker compose --project-directory /opt/multica \
  --env-file "$base_env" --env-file "$config_file" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" \
  up -d --no-deps --force-recreate backend

wait_for_ready
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  "$release_dir" "$image_digest" "$preflight_dir"
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.after"
date -u +%FT%TZ >"$evidence_dir/activated-at.txt"
chmod 0600 "$evidence_dir"/*
activation_verified=true
trap - EXIT HUP INT TERM
echo "multica_response_publisher_activated evidence=$evidence_dir"

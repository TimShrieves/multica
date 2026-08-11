#!/bin/sh
set -eu

if [ "$#" -ne 6 ] || [ "$6" != "--confirm-activate" ]; then
  echo "usage: $0 RELEASE_DIR IMAGE_DIGEST ORIGINAL_PREFLIGHT STARTING_PREFLIGHT EVIDENCE_DIR --confirm-activate" >&2
  exit 64
fi
release_dir=$(readlink -f "$1")
image_digest=$2
original_preflight=$(readlink -f "$3")
starting_preflight=$(readlink -f "$4")
evidence_dir=$5
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
config = json.load(sys.stdin)
backend = config["services"]["backend"]
if backend.get("image") != sys.argv[1]:
    raise SystemExit("activation image is not the sealed digest")
if backend.get("entrypoint") != ["./server"]:
    raise SystemExit("activation must bypass the migration entrypoint")
print(json.dumps(config, sort_keys=True, separators=(",", ":")))
' "$image_digest"
}

restore_original_backend() {
  docker compose --project-directory /opt/multica --env-file "$base_env" \
    -f "$base_compose" -f "$pin_compose" \
    up -d --no-deps --force-recreate backend || return 1
  assert_original_backend
}

evidence_parent=$(readlink -f "$(dirname "$evidence_dir")")
evidence_name=$(basename "$evidence_dir")
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in activation-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
case "$evidence_name" in *[!A-Za-z0-9._-]*) echo "invalid evidence path" >&2; exit 1;; esac
evidence_dir=$evidence_parent/$evidence_name
test ! -e "$evidence_dir"
case "$original_preflight" in /var/backups/multica-response-publisher/*) ;; *) echo "invalid original preflight" >&2; exit 1;; esac
case "$starting_preflight" in /var/backups/multica-response-publisher/*) ;; *) echo "invalid starting preflight" >&2; exit 1;; esac
test "$original_preflight" != "$starting_preflight"
test "$(stat -c '%U:%G %a' "$original_preflight")" = "root:root 700"
test "$(stat -c '%U:%G %a' "$starting_preflight")" = "root:root 700"
test -z "$(find "$original_preflight" "$starting_preflight" -xdev -type l -print -quit)"
"$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
  --allow-delivered-outbox "$release_dir" "$image_digest" "$starting_preflight"
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  --before-start "$release_dir" "$image_digest" "$starting_preflight"
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
restored_image_ref=$(docker compose --project-directory /opt/multica --env-file "$base_env" \
  -f "$base_compose" -f "$pin_compose" config --format json |
  python3 -c 'import json,sys; print(json.load(sys.stdin)["services"]["backend"]["image"])')
test -n "$restored_image_ref"
test "$(docker image inspect "$restored_image_ref" --format '{{.Id}}')" = "$expected_image"

install -d -o root -g root -m 0700 "$evidence_dir"
printf '%s\n' "$original_preflight" >"$evidence_dir/original-preflight.txt"
printf '%s\n' "$starting_preflight" >"$evidence_dir/starting-preflight.txt"
install -o root -g root -m 0600 "$config_file" "$evidence_dir/publisher.env.enabled"
sha256sum "$evidence_dir/publisher.env.enabled" >"$evidence_dir/publisher.env.enabled.sha256"
safe_off=$evidence_dir/publisher.env.safe-off
sed 's/^STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=.*/STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=false/' \
  "$config_file" >"$safe_off"
chmod 0600 "$safe_off"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.before"
done
assert_activation_overlay | sha256sum >"$evidence_dir/rendered-compose.sha256"

backend_changed=false
activation_verified=false
fail_closed() {
  status=$?
  trap - EXIT HUP INT TERM
  if [ "$backend_changed" = true ] && [ "$activation_verified" != true ]; then
    set +e
    # First return to the sealed disabled candidate without the HMAC mount.
    # Only fall back to the original base+pin image if that safe state cannot
    # be proven.
    STRIKEFLOW_RESPONSE_BACKEND_IMAGE=$image_digest \
    docker compose --project-directory /opt/multica \
      --env-file "$base_env" --env-file "$disabled_env" \
      -f "$base_compose" -f "$pin_compose" -f "$disabled_overlay" \
      up -d --no-deps --force-recreate backend \
      >"$evidence_dir/fail-closed-compose.log" 2>&1
    safe_status=$?
    "$release_dir/deploy/strikeflow-response-publisher/verify-candidate-disabled-install.sh" \
      --allow-delivered-outbox "$release_dir" "$image_digest" "$starting_preflight" \
      >"$evidence_dir/fail-closed-disabled-verify.log" 2>&1
    disabled_status=$?
    fallback_status=not_attempted
    original_verify_status=not_attempted
    if [ "$safe_status" -ne 0 ] || [ "$disabled_status" -ne 0 ]; then
      restore_original_backend >"$evidence_dir/fail-closed-fallback.log" 2>&1
      fallback_status=$?
      assert_original_backend >"$evidence_dir/fail-closed-original-verify.log" 2>&1
      original_verify_status=$?
    fi
    docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}|{{json .Mounts}}' \
      multica-backend-1 >"$evidence_dir/multica-backend-1.fail-closed" 2>&1
    printf 'compose_status=%s\ndisabled_status=%s\nfallback_status=%s\noriginal_verify_status=%s\n' \
      "$safe_status" "$disabled_status" "$fallback_status" "$original_verify_status" \
      >"$evidence_dir/fail-closed-status.txt"
    chmod 0600 "$evidence_dir"/*
    (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
    chmod 0600 "$evidence_dir"/SHA256SUMS 2>/dev/null || true
  fi
  exit "$status"
}
trap fail_closed EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

# Migrations must already pass the semantic catalog gate above. The overlay's
# direct ./server entrypoint makes this backend-only recreate incapable of
# becoming an implicit SQL gate.
backend_changed=true
docker compose --project-directory /opt/multica \
  --env-file "$base_env" --env-file "$config_file" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" \
  up -d --no-deps --force-recreate backend

wait_for_ready
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  "$release_dir" "$image_digest" "$starting_preflight"
docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' \
  multica-backend-1 >"$evidence_dir/multica-backend-1.after"
date -u +%FT%TZ >"$evidence_dir/activated-at.txt"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
chmod 0600 "$evidence_dir"/*
activation_verified=true
trap - EXIT HUP INT TERM
echo "multica_response_publisher_activated evidence=$evidence_dir"

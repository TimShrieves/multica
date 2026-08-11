#!/bin/sh
set -eu

mode=running
outbox_policy=empty
adoption_manifest=
case "${1:-}" in
  --before-start) mode=before-start; shift ;;
  --allow-delivered-outbox) outbox_policy=delivered; shift ;;
  --preserve-outbox) outbox_policy=preserve; shift ;;
  --allow-reconciled-pending-outbox)
    outbox_policy=adoption
    adoption_manifest=${2:-}
    shift 2
    ;;
esac
if [ "$#" -ne 3 ]; then
  echo "usage: $0 [--before-start|--allow-delivered-outbox|--preserve-outbox|--allow-reconciled-pending-outbox MANIFEST] RELEASE_DIR IMAGE_DIGEST PREFLIGHT_DIR" >&2
  exit 64
fi

release_dir=$(readlink -f "$1")
image_digest=$2
preflight_dir=$(readlink -f "$3")
base_compose=/opt/multica/docker-compose.selfhost.yml
pin_compose=/opt/multica/docker-compose.pin.yml
overlay=$release_dir/docker-compose.strikeflow-response-candidate-disabled.yml
base_env=/opt/multica/.env
disabled_env=$release_dir/deploy/strikeflow-response-publisher/publisher.env.disabled
catalog_source=$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh
adoption_contract=$release_dir/deploy/strikeflow-response-publisher/adoption-contract.sh

case "$release_dir" in /opt/multica-response-publisher/releases/*) ;; *) exit 1;; esac
case "$preflight_dir" in /var/backups/multica-response-publisher/*) ;; *) exit 1;; esac
test "$(readlink -f /opt/multica-response-publisher/current)" = "$release_dir"
test -f "$overlay" -a -f "$base_compose" -a -f "$pin_compose" -a -f "$base_env" -a -f "$disabled_env" -a -f "$adoption_contract"
test -f "$catalog_source"
test "$(stat -c '%U:%G' "$release_dir")" = root:root
test -z "$(find "$release_dir" -xdev \( ! -user root -o ! -group root \) -print -quit)"
test -z "$(find "$release_dir" -xdev -type l -print -quit)"
test -z "$(find "$preflight_dir" -xdev -type l -print -quit)"
test -z "$(find "$release_dir" -xdev -perm /022 -print -quit)"
test "$(stat -c '%U:%G %a' "$preflight_dir")" = "root:root 700"
(cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null)
(cd / && sha256sum -c "$preflight_dir/active-compose.sha256" >/dev/null)
test "$(sed -n 's/^image_digest=//p' "$release_dir/ARTIFACTS")" = "$image_digest"

image_id=$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")
source_commit=$(sed -n 's/^source_commit=//p' "$release_dir/ARTIFACTS")
test "$(docker image inspect "$image_digest" --format '{{.Id}}')" = "$image_id"
docker image inspect "$image_digest" --format '{{range .RepoDigests}}{{println .}}{{end}}' |
  grep -Fqx "$image_digest"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.source"}}')" = "$source_commit"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.state"}}')" = dormant

# Reuse the exact sealed catalog DO block from the enabled verifier so the
# disabled candidate gate cannot drift to a weaker schema definition.
test "$(grep -c '^DO \$\$$' "$catalog_source")" -eq 1
test "$(grep -c '^SQL$' "$catalog_source")" -eq 1
sed -n '/^DO \$\$$/,/^SQL$/p' "$catalog_source" | sed '$d' |
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
if [ "$outbox_policy" = empty ]; then
  outbox_count=$(docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM strikeflow_response_outbox"')
  test "$outbox_count" = 0
elif [ "$outbox_policy" = delivered ]; then
  unsafe_outbox_count=$(docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM strikeflow_response_outbox WHERE delivered_at IS NULL OR needs_attention_at IS NOT NULL"')
  test "$unsafe_outbox_count" = 0
elif [ "$outbox_policy" = adoption ]; then
  # The manifest is operator-created evidence that the exact natural responses
  # already converged through StrikeFlow's cross-mode reconciliation fix. This
  # verifier only permits that exact ordered pair and never edits either ledger.
  # shellcheck source=/dev/null
  . "$adoption_contract"
  validate_adoption_manifest "$adoption_manifest"
  verify_adoption_config
  verify_response_reconciliation_stopped
  verify_adoption_source_catalog
  verify_adoption_outbox initial
fi

# Render only the non-secret image override and an exact false publisher flag.
STRIKEFLOW_RESPONSE_BACKEND_IMAGE=$image_digest \
docker compose --project-directory /opt/multica --env-file "$base_env" \
  --env-file "$disabled_env" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" config --format json |
python3 -c '
import json, sys
config = json.load(sys.stdin)
backend = config["services"]["backend"]
if backend.get("image") != sys.argv[1]:
    raise SystemExit("rendered backend image is not the sealed digest")
if backend.get("entrypoint") != ["./server"]:
    raise SystemExit("disabled candidate must bypass the migration entrypoint")
response_env = {k: v for k, v in backend.get("environment", {}).items() if k.startswith("STRIKEFLOW_RESPONSE_")}
if response_env.pop("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", None) != "false" or any(response_env.values()):
    raise SystemExit("rendered candidate publisher is not exactly false")
if any(v.get("target") == "/run/secrets/strikeflow_response_hmac" for v in backend.get("volumes", [])):
    raise SystemExit("disabled candidate must not mount the response HMAC secret")
' "$image_digest"

for container in multica-frontend-1 multica-postgres-1; do
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container")
  test "$current" = "$(cat "$preflight_dir/$container.identity")"
done

if [ "$mode" = before-start ]; then
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' multica-backend-1)
  test "$current" = "$(cat "$preflight_dir/multica-backend-1.identity")"
  test "$(docker inspect multica-backend-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')" = "$base_compose,$pin_compose"
else
  test "$(docker inspect multica-backend-1 --format '{{.Image}}')" = "$image_id"
  test "$(docker inspect multica-backend-1 --format '{{.State.Running}}')" = true
  test "$(docker inspect multica-backend-1 --format '{{.HostConfig.RestartPolicy.Name}}')" = unless-stopped
  test "$(docker inspect multica-backend-1 --format '{{json .Config.Entrypoint}}')" = '["./server"]'
  test "$(docker inspect multica-backend-1 --format '{{json .NetworkSettings.Ports}}')" = "$(cut -d'|' -f4 "$preflight_dir/multica-backend-1.identity")"
  test "$(docker inspect multica-backend-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')" = "$base_compose,$pin_compose,$overlay"
  docker inspect multica-backend-1 --format '{{json .Mounts}}' |
    python3 -c 'import json,sys; raise SystemExit(any(m.get("Destination")=="/run/secrets/strikeflow_response_hmac" for m in json.load(sys.stdin)))'
  docker inspect multica-backend-1 --format '{{json .Config.Env}}' |
    python3 -c '
import json, sys
env = dict(item.split("=", 1) for item in json.load(sys.stdin) if "=" in item)
response_env = {k: v for k, v in env.items() if k.startswith("STRIKEFLOW_RESPONSE_")}
if response_env.pop("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", None) != "false" or any(response_env.values()):
    raise SystemExit("running candidate publisher is not exactly false")
'
  wget -q -O /dev/null http://127.0.0.1:8080/health
  wget -q -O /dev/null http://127.0.0.1:8080/readyz
fi

echo "disabled_multica_response_candidate_ok mode=$mode image=$image_digest release=$(basename "$release_dir")"

#!/bin/sh
set -eu

if [ "$#" -ne 3 ]; then
  echo "usage: $0 RELEASE_DIR IMAGE_TAG PREFLIGHT_DIR" >&2
  exit 64
fi

release_dir=$1
image_tag=$2
preflight_dir=$3
install_root=/opt/multica-response-publisher
config_file=/etc/multica-response-publisher/publisher.env
hmac_file=/etc/multica-response-publisher/strikeflow-response-hmac

resolved_release_dir=$(readlink -f "$release_dir")
resolved_preflight_dir=$(readlink -f "$preflight_dir")
case "$resolved_release_dir" in
  /opt/multica-response-publisher/releases/*) ;;
  *) echo "release path escaped install root" >&2; exit 1 ;;
esac
case "$resolved_preflight_dir" in
  /var/backups/multica-response-publisher/*) ;;
  *) echo "preflight path escaped backup root" >&2; exit 1 ;;
esac
test "$(readlink -f "$install_root/current")" = "$resolved_release_dir"
release_dir=$resolved_release_dir
preflight_dir=$resolved_preflight_dir
artifacts_file=$release_dir/ARTIFACTS

test -d "$release_dir"
test -f "$release_dir/SHA256SUMS"
test "$(grep -c '  deploy/strikeflow-response-publisher/adoption-contract.sh$' "$release_dir/SHA256SUMS")" -eq 1
test -f "$artifacts_file"
test "$(stat -c '%U:%G' "$release_dir")" = root:root
test -z "$(find "$release_dir" -xdev -type l -print -quit)"
test -z "$(find "$preflight_dir" -xdev -type l -print -quit)"
test -z "$(find "$release_dir" -xdev -name '._*' -print -quit)"
test -z "$(find "$release_dir" -xdev -perm /022 -print -quit)"
test "$(stat -c '%U:%G %a' "$preflight_dir")" = "root:root 700"
(cd "$release_dir" && sha256sum -c SHA256SUMS)

expected_artifact_keys='source_commit source_archive_sha256 image_tag image_id image_digest preflight publisher_enabled migrations_applied_to_production deployment_tooling_commit'
actual_artifact_keys=$(sed -n 's/=.*//p' "$artifacts_file" | sort | tr '\n' ' ' | sed 's/ $//')
test "$actual_artifact_keys" = "$(printf '%s\n' $expected_artifact_keys | sort | tr '\n' ' ' | sed 's/ $//')"
for key in $expected_artifact_keys; do
  test "$(grep -c "^${key}=" "$artifacts_file")" -eq 1
done
artifact() { sed -n "s/^$1=//p" "$artifacts_file"; }
test "$(artifact image_tag)" = "$image_tag"
test "$(artifact preflight)" = "$preflight_dir"
test "$(artifact publisher_enabled)" = false
test "$(artifact migrations_applied_to_production)" = false

image_id=$(artifact image_id)
image_digest=$(artifact image_digest)
source_commit=$(artifact source_commit)
test "$(docker image inspect "$image_tag" --format '{{.Id}}')" = "$image_id"
docker image inspect "$image_tag" --format '{{range .RepoDigests}}{{println .}}{{end}}' | grep -Fqx "$image_digest"
test "$(docker image inspect "$image_tag" --format '{{index .Config.Labels "co.strikeflow.response-publisher.source"}}')" = "$source_commit"
test "$(docker image inspect "$image_tag" --format '{{index .Config.Labels "co.strikeflow.response-publisher.state"}}')" = dormant
test -z "$(docker ps -a --no-trunc --filter "ancestor=$image_id" --format '{{.ID}}')"

test "$(stat -c '%U:%G %a' "$config_file")" = "root:root 600"
test "$(stat -c '%U:%G %a' "$(dirname "$config_file")")" = "root:root 700"
cr=$(printf '\r')
test -z "$(LC_ALL=C grep -n "$cr" "$config_file" || true)"
expected_config_keys='STRIKEFLOW_RESPONSE_AGENT_ID STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE STRIKEFLOW_RESPONSE_BACKEND_IMAGE STRIKEFLOW_RESPONSE_COMMAND_IDS STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS STRIKEFLOW_RESPONSE_HMAC_HOST_FILE STRIKEFLOW_RESPONSE_HMAC_KEY_ID STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE STRIKEFLOW_RESPONSE_NOT_BEFORE STRIKEFLOW_RESPONSE_PROJECT_IDS STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED STRIKEFLOW_RESPONSE_RECIPIENT_ID STRIKEFLOW_RESPONSE_STR94_ISSUE_ID STRIKEFLOW_RESPONSE_WEBHOOK_URL STRIKEFLOW_RESPONSE_WORKSPACE_ID STRIKEFLOW_RESPONSE_WORKSPACE_KEY'
actual_config_keys=$(sed -n 's/=.*//p' "$config_file" | sort | tr '\n' ' ' | sed 's/ $//')
test "$actual_config_keys" = "$(printf '%s\n' $expected_config_keys | sort | tr '\n' ' ' | sed 's/ $//')"
for key in $expected_config_keys; do
  test "$(grep -c "^${key}=" "$config_file")" -eq 1
done
test "$(grep -c '^STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=false$' "$config_file")" -eq 1
if grep -Eq '^STRIKEFLOW_RESPONSE_(PUBLISHER_ENABLED=true|BACKEND_IMAGE=.+|WEBHOOK_URL=.+|HMAC_SECRET=.+|HMAC_SECRET_FILE=.+|HMAC_HOST_FILE=.+|HMAC_KEY_ID=.+|WORKSPACE_ID=.+|WORKSPACE_KEY=.+|PROJECT_IDS=.+|AUTHORIZATION_MODE=.+|COMMAND_IDS=.+|RECIPIENT_ID=.+|AGENT_ID=.+|STR94_ISSUE_ID=.+|EXCLUDED_ISSUE_IDS=.+|NOT_BEFORE=.+)' "$config_file"; then
  echo "publisher configuration is not dormant" >&2
  exit 1
fi

test "$(find /etc/multica-response-publisher -mindepth 1 -maxdepth 1 -printf '%f\n' | sort | tr '\n' ' ')" = "publisher.env strikeflow-response-hmac "
test ! -L "$hmac_file"
test "$(stat -c '%U:%G %a %s' "$hmac_file")" = "root:root 600 64"
if [ -f "$preflight_dir/hmac-file.sha256" ]; then
  test "$(sha256sum "$hmac_file" | cut -d' ' -f1)" = "$(tr -d '\r\n' <"$preflight_dir/hmac-file.sha256")"
fi

for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container")
  expected=$(cat "$preflight_dir/$container.identity")
  test "$current" = "$expected"
done

(cd / && sha256sum -c "$preflight_dir/active-compose.sha256")

if systemctl is-active --quiet multica-response-publisher.service 2>/dev/null; then
  echo "publisher service unexpectedly active" >&2
  exit 1
fi
if systemctl is-enabled --quiet multica-response-publisher.service 2>/dev/null; then
  echo "publisher service unexpectedly enabled" >&2
  exit 1
fi
if systemctl is-active --quiet multica-response-publisher.timer 2>/dev/null || systemctl is-enabled --quiet multica-response-publisher.timer 2>/dev/null; then
  echo "publisher timer unexpectedly installed or active" >&2
  exit 1
fi

active_config_files=$(docker inspect multica-backend-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')
test "$active_config_files" = '/opt/multica/docker-compose.selfhost.yml,/opt/multica/docker-compose.pin.yml'

echo "disabled_multica_response_publisher_ok image=$image_tag release=$(basename "$release_dir")"

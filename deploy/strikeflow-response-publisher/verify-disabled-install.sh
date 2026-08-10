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

test "$(readlink -f "$install_root/current")" = "$(readlink -f "$release_dir")"
test -d "$release_dir"
test -f "$release_dir/SHA256SUMS"
(cd "$release_dir" && sha256sum -c SHA256SUMS)

test "$(stat -c '%U:%G %a' "$config_file")" = "root:root 600"
test "$(grep -c '^STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED=false$' "$config_file")" -eq 1
if grep -Eq '^STRIKEFLOW_RESPONSE_(PUBLISHER_ENABLED=true|WEBHOOK_URL=.+|HMAC_SECRET|HMAC_SECRET_FILE=.+|HMAC_KEY_ID=.+|WORKSPACE_ID=.+|WORKSPACE_KEY=.+|PROJECT_IDS=.+|COMMAND_IDS=.+|RECIPIENT_ID=.+|AGENT_ID=.+|STR94_ISSUE_ID=.+|NOT_BEFORE=.+)' "$config_file"; then
  echo "publisher configuration is not dormant" >&2
  exit 1
fi

test ! -e /etc/multica-response-publisher/response-hmac-secret
test -z "$(docker ps --filter "ancestor=$image_tag" --format '{{.ID}}')"

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

echo "disabled_multica_response_publisher_ok image=$image_tag release=$(basename "$release_dir")"

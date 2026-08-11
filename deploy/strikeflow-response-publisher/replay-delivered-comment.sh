#!/bin/sh
set -eu

if [ "$#" -ne 9 ] || [ "$9" != "--confirm-replay" ]; then
  echo "usage: $0 RELEASE_DIR IMAGE_DIGEST STARTING_PREFLIGHT COMMAND_ID EVENT_ID PAYLOAD_SHA256 RECORDED_AT EVIDENCE_DIR --confirm-replay" >&2
  exit 64
fi
release_dir=$(readlink -f "$1")
image_digest=$2
starting_preflight=$(readlink -f "$3")
command_id=$4
event_id=$5
payload_sha=$6
recorded_at=$7
evidence_dir=$8
lock_file=/run/lock/multica-response-publisher-deploy.lock

exec 9>"$lock_file"
flock -n 9 || { echo "another response deployment is running" >&2; exit 1; }
evidence_parent=$(readlink -f "$(dirname "$evidence_dir")")
evidence_name=$(basename "$evidence_dir")
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in replay-comment-*) ;; *) echo "invalid evidence path" >&2; exit 1;; esac
case "$evidence_name" in *[!A-Za-z0-9._-]*) echo "invalid evidence path" >&2; exit 1;; esac
evidence_dir=$evidence_parent/$evidence_name
test ! -e "$evidence_dir"
test "$(readlink -f /opt/multica-response-publisher/current)" = "$release_dir"
"$release_dir/deploy/strikeflow-response-publisher/verify-enabled-install.sh" \
  --rollback-preflight "$release_dir" "$image_digest" "$starting_preflight"
test "$(docker inspect multica-backend-1 --format '{{.Image}}')" = "$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")"
test "$(docker inspect multica-backend-1 --format '{{json .Config.Entrypoint}}')" = '["./server"]'

install -d -o root -g root -m 0700 "$evidence_dir"
printf '%s\n' "$command_id" >"$evidence_dir/command-id.txt"
printf '%s\n' "$event_id" >"$evidence_dir/event-id.txt"
printf '%s\n' "$payload_sha" >"$evidence_dir/payload-sha256.txt"
printf '%s\n' "$recorded_at" >"$evidence_dir/recorded-at.txt"
database_fingerprint() {
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
SELECT 'reply_receipts|' || count(*) || '|' || md5(string_agg(to_jsonb(r)::text,'|' ORDER BY r.token_id,r.idempotency_key)) FROM strikeflow_connector_reply_receipt r;
SELECT 'outbox|' || count(*) || '|' || count(*) FILTER (WHERE delivered_at IS NULL) || '|' || count(*) FILTER (WHERE needs_attention_at IS NOT NULL) || '|' || md5(COALESCE(string_agg(to_jsonb(o)::text,'|' ORDER BY o.event_id),'')) FROM strikeflow_response_outbox o;
SQL
}
database_fingerprint >"$evidence_dir/database.before"
complete=false
seal_evidence() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  printf 'replay_status=%s\n' "$status" >"$evidence_dir/status.txt"
  database_fingerprint >"$evidence_dir/database.final" 2>&1 || true
  chmod 0600 "$evidence_dir"/* 2>/dev/null || true
  (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
  chmod 0600 "$evidence_dir"/SHA256SUMS 2>/dev/null || true
  if [ "$complete" = true ]; then exit 0; fi
  exit "$status"
}
trap seal_evidence EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker exec multica-backend-1 ./strikeflow-response-replay \
  --command-id "$command_id" --event-id "$event_id" \
  --payload-sha256 "$payload_sha" --recorded-at "$recorded_at" \
  >"$evidence_dir/replay-result.json"
python3 - "$evidence_dir/replay-result.json" "$event_id" "$payload_sha" "$recorded_at" <<'PY'
import datetime, json, pathlib, sys
data = json.loads(pathlib.Path(sys.argv[1]).read_text())
if data.get("event_id") != sys.argv[2] or data.get("payload_sha256") != sys.argv[3]:
    raise SystemExit("replay result identity mismatch")
if data.get("replay") is not True or data.get("response_state") != "responding":
    raise SystemExit("receiver did not acknowledge an exact responding replay")
want = datetime.datetime.fromisoformat(sys.argv[4].replace("Z", "+00:00"))
got = datetime.datetime.fromisoformat(data.get("recorded_at", "").replace("Z", "+00:00"))
if got != want:
    raise SystemExit("replay changed recorded_at")
PY
database_fingerprint >"$evidence_dir/database.after"
cmp -s "$evidence_dir/database.before" "$evidence_dir/database.after"
printf 'replay_status=0\n' >"$evidence_dir/status.txt"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
chmod 0600 "$evidence_dir"/*
complete=true
trap - EXIT HUP INT TERM
echo "multica_response_comment_replayed evidence=$evidence_dir"

#!/bin/sh
set -eu

mode=running
adoption_manifest=
case "${1:-}" in
  --before-start) mode=before-start; shift ;;
  --rollback-preflight) mode=rollback-preflight; shift ;;
  --adoption-before-start)
    mode=adoption-before-start
    adoption_manifest=${2:-}
    shift 2
    ;;
  --adoption-after-start)
    mode=adoption-after-start
    adoption_manifest=${2:-}
    shift 2
    ;;
esac
if [ "$#" -ne 3 ]; then
  echo "usage: $0 [--before-start|--rollback-preflight|--adoption-before-start MANIFEST|--adoption-after-start MANIFEST] RELEASE_DIR IMAGE_DIGEST PREFLIGHT_DIR" >&2
  exit 64
fi

release_dir=$(readlink -f "$1")
image_digest=$2
preflight_dir=$(readlink -f "$3")
config_file=/etc/multica-response-publisher/publisher.env
base_compose=/opt/multica/docker-compose.selfhost.yml
pin_compose=/opt/multica/docker-compose.pin.yml
overlay=$release_dir/docker-compose.strikeflow-response-publisher.yml
base_env=/opt/multica/.env
adoption_contract=$release_dir/deploy/strikeflow-response-publisher/adoption-contract.sh

case "$release_dir" in /opt/multica-response-publisher/releases/*) ;; *) exit 1;; esac
case "$preflight_dir" in /var/backups/multica-response-publisher/*) ;; *) exit 1;; esac
test "$(readlink -f /opt/multica-response-publisher/current)" = "$release_dir"
test "$(stat -c '%U:%G %a' "$config_file")" = "root:root 600"
test "$(stat -c '%U:%G %a' "$(dirname "$config_file")")" = "root:root 700"
test "$(stat -c '%U:%G %a' "$preflight_dir")" = "root:root 700"
test -f "$overlay" -a -f "$base_compose" -a -f "$pin_compose" -a -f "$base_env" -a -f "$adoption_contract"
test -z "$(find "$release_dir" -xdev -type l -print -quit)"
(cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null)
(cd / && sha256sum -c "$preflight_dir/active-compose.sha256" >/dev/null)
test "$(sed -n 's/^image_digest=//p' "$release_dir/ARTIFACTS")" = "$image_digest"

expected_keys='STRIKEFLOW_RESPONSE_AGENT_ID STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE STRIKEFLOW_RESPONSE_BACKEND_IMAGE STRIKEFLOW_RESPONSE_COMMAND_IDS STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS STRIKEFLOW_RESPONSE_HMAC_HOST_FILE STRIKEFLOW_RESPONSE_HMAC_KEY_ID STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE STRIKEFLOW_RESPONSE_NOT_BEFORE STRIKEFLOW_RESPONSE_PROJECT_IDS STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED STRIKEFLOW_RESPONSE_RECIPIENT_ID STRIKEFLOW_RESPONSE_STR94_ISSUE_ID STRIKEFLOW_RESPONSE_WEBHOOK_URL STRIKEFLOW_RESPONSE_WORKSPACE_ID STRIKEFLOW_RESPONSE_WORKSPACE_KEY'
actual_keys=$(sed -n 's/=.*//p' "$config_file" | sort | tr '\n' ' ' | sed 's/ $//')
test "$actual_keys" = "$(printf '%s\n' $expected_keys | sort | tr '\n' ' ' | sed 's/ $//')"
for key in $expected_keys; do test "$(grep -c "^${key}=" "$config_file")" -eq 1; done

python3 - "$config_file" "$image_digest" "$mode" <<'PY'
import datetime, pathlib, re, sys, urllib.parse, uuid

path, expected_image, verify_mode = sys.argv[1:]
values = {}
for line in pathlib.Path(path).read_text(encoding="utf-8").splitlines():
    key, sep, value = line.partition("=")
    if not sep or key in values:
        raise SystemExit("invalid publisher environment shape")
    values[key] = value
if values["STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED"] != "true":
    raise SystemExit("publisher is not exactly enabled")
if values["STRIKEFLOW_RESPONSE_BACKEND_IMAGE"] != expected_image:
    raise SystemExit("candidate image digest does not match config")
url = urllib.parse.urlsplit(values["STRIKEFLOW_RESPONSE_WEBHOOK_URL"])
if (url.scheme, url.netloc, url.path, url.query, url.fragment) != (
    "https", "strikeflow.strikemedia.co",
    "/api/integrations/multica/content-delivery/responses", "", ""):
    raise SystemExit("webhook URL is not the exact production receiver")
if values["STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE"] != "/run/secrets/strikeflow_response_hmac":
    raise SystemExit("in-container HMAC path is invalid")
if values["STRIKEFLOW_RESPONSE_HMAC_HOST_FILE"] != "/etc/multica-response-publisher/strikeflow-response-hmac":
    raise SystemExit("host HMAC path is invalid")
if not re.fullmatch(r"[A-Za-z0-9._-]{1,64}", values["STRIKEFLOW_RESPONSE_HMAC_KEY_ID"]):
    raise SystemExit("HMAC key id is invalid")
exact = {
    "STRIKEFLOW_RESPONSE_WORKSPACE_ID": "ca4961ec-70fa-4738-8167-1331d82ebb21",
    "STRIKEFLOW_RESPONSE_WORKSPACE_KEY": "strike",
    "STRIKEFLOW_RESPONSE_PROJECT_IDS": "d98f5700-8946-4054-b763-001d85767036",
    "STRIKEFLOW_RESPONSE_RECIPIENT_ID": "92008a79-f6ce-438d-b60f-4dd6580f94e4",
    "STRIKEFLOW_RESPONSE_AGENT_ID": "eb361a09-be12-4626-9d03-faadc99a3933",
    "STRIKEFLOW_RESPONSE_STR94_ISSUE_ID": "b41bcb97-8b63-43f6-9d6c-4ee9e9ada891",
}
for key, want in exact.items():
    if values[key] != want:
        raise SystemExit(f"{key} does not match the protected scope")
protected_issues = values["STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS"].split(",")
if protected_issues != [
    "b41bcb97-8b63-43f6-9d6c-4ee9e9ada891",
    "39dcf540-bedf-4449-bc71-2e9e15fa0573",
    "b1839f3d-97e5-449a-9059-21b3b393d096",
]:
    raise SystemExit("historical issue exclusion ledger must exactly protect STR-94, STR-166, and STR-172")
not_before = values["STRIKEFLOW_RESPONSE_NOT_BEFORE"]
if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", not_before):
    raise SystemExit("not-before is not exact RFC3339")
try:
    parsed = datetime.datetime.fromisoformat(not_before.replace("Z", "+00:00"))
except ValueError as exc:
    raise SystemExit("not-before is not RFC3339") from exc
if parsed.tzinfo is None or parsed.utcoffset() is None:
    raise SystemExit("not-before must contain a timezone")
if verify_mode in {"before-start", "adoption-before-start"}:
    age = datetime.datetime.now(datetime.timezone.utc) - parsed.astimezone(datetime.timezone.utc)
    if age < datetime.timedelta(0) or age > datetime.timedelta(hours=24):
        raise SystemExit("not-before must be within the previous 24 hours at activation")
mode = values["STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE"]
commands = [part.strip() for part in values["STRIKEFLOW_RESPONSE_COMMAND_IDS"].split(",") if part.strip()]
if mode == "explicit_commands":
    if not 1 <= len(commands) <= 32 or len(commands) != len(set(commands)):
        raise SystemExit("explicit command authorization is invalid")
    for command in commands:
        uuid.UUID(command)
elif mode == "receipt_lineage":
    if commands:
        raise SystemExit("receipt_lineage must not include command ids")
else:
    raise SystemExit("authorization mode is invalid")
PY

secret_file=$(sed -n 's/^STRIKEFLOW_RESPONSE_HMAC_HOST_FILE=//p' "$config_file")
test -f "$secret_file" -a ! -L "$secret_file"
test "$(stat -c '%U:%G %a' "$secret_file")" = "root:root 600"
python3 - "$secret_file" <<'PY'
import pathlib, sys
raw = pathlib.Path(sys.argv[1]).read_bytes()
if len(raw) < 32 or len(raw) > 4096 or raw != raw.strip():
    raise SystemExit("HMAC secret must be 32-4096 bytes without surrounding whitespace")
PY

image_id=$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")
test "$(docker image inspect "$image_digest" --format '{{.Id}}')" = "$image_id"
docker image inspect "$image_digest" --format '{{range .RepoDigests}}{{println .}}{{end}}' |
  grep -Fqx "$image_digest"
source_commit=$(sed -n 's/^source_commit=//p' "$release_dir/ARTIFACTS")
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.source"}}')" = "$source_commit"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.state"}}')" = dormant

# Render with the production env first and the purpose-scoped publisher env
# second. The overlay must be last so the pinned legacy image cannot override
# the sealed candidate. Output is consumed in-memory and never printed.
docker compose --project-directory /opt/multica \
  --env-file "$base_env" --env-file "$config_file" \
  -f "$base_compose" -f "$pin_compose" -f "$overlay" config --format json |
python3 -c '
import json, sys
config = json.load(sys.stdin)
image, secret = sys.argv[1:]
backend = config["services"]["backend"]
if backend.get("image") != image:
    raise SystemExit("rendered backend image is not the sealed digest")
if backend.get("entrypoint") != ["./server"]:
    raise SystemExit("enabled publisher must bypass the migration entrypoint")
env = backend.get("environment", {})
if env.get("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED") != "true":
    raise SystemExit("rendered publisher is not enabled")
volumes = backend.get("volumes", [])
matches = [v for v in volumes if v.get("target") == "/run/secrets/strikeflow_response_hmac"]
if len(matches) != 1 or matches[0].get("source") != secret or matches[0].get("read_only") is not True:
    raise SystemExit("rendered HMAC mount is not exact and read-only")
' "$image_digest" "$secret_file"

if [ "$mode" = before-start ] || [ "$mode" = adoption-before-start ]; then
  expected=$(cat "$preflight_dir/multica-backend-1.identity")
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' multica-backend-1)
  test "$current" = "$expected"
else
  test "$(docker inspect multica-backend-1 --format '{{.Image}}')" = "$image_id"
  test "$(docker inspect multica-backend-1 --format '{{.State.Running}}')" = true
  test "$(docker inspect multica-backend-1 --format '{{.HostConfig.RestartPolicy.Name}}')" = unless-stopped
  test "$(docker inspect multica-backend-1 --format '{{json .Config.Entrypoint}}')" = '["./server"]'
  expected_ports=$(cut -d'|' -f4 "$preflight_dir/multica-backend-1.identity")
  test "$(docker inspect multica-backend-1 --format '{{json .NetworkSettings.Ports}}')" = "$expected_ports"
  wget -q -O /dev/null http://127.0.0.1:8080/health
  wget -q -O /dev/null http://127.0.0.1:8080/readyz
  config_files=$(docker inspect multica-backend-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}')
  test "$config_files" = "$base_compose,$pin_compose,$overlay"
  docker inspect multica-backend-1 --format '{{json .Mounts}}' |
  python3 -c '
import json, sys
mounts = json.load(sys.stdin)
matches = [m for m in mounts if m.get("Destination") == "/run/secrets/strikeflow_response_hmac"]
if len(matches) != 1 or matches[0].get("Source") != sys.argv[1] or matches[0].get("RW") is not False:
    raise SystemExit("running HMAC mount is invalid")
' "$secret_file"
  docker inspect multica-backend-1 --format '{{json .Config.Env}}' |
  python3 -c '
import json, pathlib, sys
actual = dict(item.split("=", 1) for item in json.load(sys.stdin) if "=" in item)
configured = dict(line.split("=", 1) for line in pathlib.Path(sys.argv[1]).read_text().splitlines())
runtime_keys = {
    "STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", "STRIKEFLOW_RESPONSE_WEBHOOK_URL",
    "STRIKEFLOW_RESPONSE_HMAC_SECRET_FILE", "STRIKEFLOW_RESPONSE_HMAC_KEY_ID",
    "STRIKEFLOW_RESPONSE_WORKSPACE_ID", "STRIKEFLOW_RESPONSE_WORKSPACE_KEY",
    "STRIKEFLOW_RESPONSE_PROJECT_IDS", "STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE",
    "STRIKEFLOW_RESPONSE_COMMAND_IDS", "STRIKEFLOW_RESPONSE_RECIPIENT_ID",
    "STRIKEFLOW_RESPONSE_AGENT_ID", "STRIKEFLOW_RESPONSE_STR94_ISSUE_ID",
    "STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS",
    "STRIKEFLOW_RESPONSE_NOT_BEFORE",
}
if {key: actual.get(key) for key in runtime_keys} != {key: configured[key] for key in runtime_keys}:
    raise SystemExit("running response environment differs from sealed activation config")
' "$config_file"
fi

for container in multica-frontend-1 multica-postgres-1; do
  current=$(docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container")
  test "$current" = "$(cat "$preflight_dir/$container.identity")"
done

# Semantic production catalog gate. This is read-only and intentionally checks
# meanings instead of migration names alone.
docker exec -i multica-postgres-1 sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
DO $$
DECLARE versions text[];
BEGIN
  SELECT array_agg(version ORDER BY version) INTO versions
  FROM schema_migrations WHERE version LIKE '90000%';
  IF versions IS DISTINCT FROM ARRAY[
    '900001_strikeflow_response_outbox',
    '900002_strikeflow_connector_reply_command_unique',
    '900003_strikeflow_response_outbox_event_unique',
    '900004_strikeflow_response_outbox_due_index',
    '900005_strikeflow_response_outbox_event_id_unique'
  ]::text[] THEN RAISE EXCEPTION 'response migration ledger mismatch'; END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_connector_reply_receipt' AND column_name='strikeflow_command_id' AND data_type='uuid' AND is_nullable='YES') THEN
    RAISE EXCEPTION 'receipt command column mismatch';
  END IF;
  IF to_regclass('public.strikeflow_response_outbox') IS NULL THEN RAISE EXCEPTION 'response outbox absent'; END IF;
  IF (SELECT array_agg(format('%s:%s:%s',column_name,data_type,is_nullable) ORDER BY ordinal_position) FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_response_outbox') IS DISTINCT FROM ARRAY[
    'event_id:uuid:NO','event_type:text:NO','strikeflow_command_id:uuid:NO','workspace_key:text:NO',
    'workspace_id:uuid:NO','project_id:uuid:NO','issue_id:uuid:NO','issue_identifier:text:NO',
    'inbox_item_id:uuid:NO','root_comment_id:uuid:NO','member_comment_id:uuid:NO','continuation_task_id:uuid:NO',
    'recipient_id:uuid:NO','agent_id:uuid:NO','agent_comment_id:uuid:YES','agent_comment_parent_id:uuid:YES',
    'agent_comment_content:text:YES','agent_comment_type:text:YES','occurred_at:timestamp with time zone:NO',
    'attempt_count:integer:NO','next_attempt_at:timestamp with time zone:NO','lease_until:timestamp with time zone:YES',
    'delivered_at:timestamp with time zone:YES','needs_attention_at:timestamp with time zone:YES','last_error:text:YES',
    'created_at:timestamp with time zone:NO'
  ]::text[] THEN RAISE EXCEPTION 'response outbox column ledger mismatch'; END IF;
  IF (SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_response_outbox' AND column_default IS NOT NULL) <> 4
     OR NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_response_outbox' AND column_name='event_id' AND column_default='gen_random_uuid()')
     OR NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_response_outbox' AND column_name='attempt_count' AND column_default='0')
     OR NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_response_outbox' AND column_name='next_attempt_at' AND column_default='now()')
     OR NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_response_outbox' AND column_name='created_at' AND column_default='now()')
  THEN RAISE EXCEPTION 'response outbox default ledger mismatch'; END IF;
  IF (SELECT count(*) FROM pg_constraint WHERE conrelid='public.strikeflow_response_outbox'::regclass AND contype='c' AND convalidated) <> 3
     OR EXISTS (
       SELECT 1 FROM pg_constraint
       WHERE conrelid='public.strikeflow_response_outbox'::regclass AND contype='c' AND convalidated
         AND regexp_replace(pg_get_constraintdef(oid,false),'[[:space:]()]','','g') <> ALL(ARRAY[
           'CHECKevent_type=ANYARRAY[''agent_comment.created''::text,''task.completed''::text]',
           'CHECKattempt_count>=0',
           'CHECKevent_type=''agent_comment.created''::textANDagent_comment_idISNOTNULLANDagent_comment_contentISNOTNULLANDagent_comment_typeISNOTNULLORevent_type=''task.completed''::textANDagent_comment_idISNULLANDagent_comment_contentISNULLANDagent_comment_typeISNULL'
         ]::text[])
     )
     OR (SELECT count(DISTINCT regexp_replace(pg_get_constraintdef(oid,false),'[[:space:]()]','','g')) FROM pg_constraint WHERE conrelid='public.strikeflow_response_outbox'::regclass AND contype='c' AND convalidated) <> 3
  THEN RAISE EXCEPTION 'response outbox exact check constraints mismatch'; END IF;
  IF EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='public.strikeflow_response_outbox'::regclass AND contype='f') THEN RAISE EXCEPTION 'response outbox must not contain foreign keys'; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='strikeflow_reply_command_binding_immutable' AND tgrelid='public.strikeflow_connector_reply_receipt'::regclass AND tgfoid='public.reject_strikeflow_reply_command_binding_change()'::regprocedure AND tgtype=19 AND tgenabled='O' AND NOT tgisinternal)
     OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname='strikeflow_response_outbox_identity_immutable' AND tgrelid='public.strikeflow_response_outbox'::regclass AND tgfoid='public.reject_strikeflow_response_outbox_identity_change()'::regprocedure AND tgtype=19 AND tgenabled='O' AND NOT tgisinternal)
     OR (SELECT count(*) FROM pg_trigger WHERE tgrelid IN ('public.strikeflow_connector_reply_receipt'::regclass,'public.strikeflow_response_outbox'::regclass) AND NOT tgisinternal) <> 2
  THEN RAISE EXCEPTION 'exact immutability trigger binding mismatch'; END IF;
  IF NOT EXISTS (
       SELECT 1 FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang
       WHERE p.oid='public.reject_strikeflow_reply_command_binding_change()'::regprocedure
         AND l.lanname='plpgsql' AND p.prorettype='trigger'::regtype AND p.pronargs=0
         AND p.provolatile='v' AND NOT p.prosecdef AND NOT p.proleakproof
         AND btrim(regexp_replace(p.prosrc,'[[:space:]]+',' ','g')) =
           'BEGIN IF NEW.strikeflow_command_id IS DISTINCT FROM OLD.strikeflow_command_id THEN RAISE EXCEPTION ''strikeflow command binding is immutable''; END IF; RETURN NEW; END;'
     )
     OR NOT EXISTS (
       SELECT 1 FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang
       WHERE p.oid='public.reject_strikeflow_response_outbox_identity_change()'::regprocedure
         AND l.lanname='plpgsql' AND p.prorettype='trigger'::regtype AND p.pronargs=0
         AND p.provolatile='v' AND NOT p.prosecdef AND NOT p.proleakproof
         AND btrim(regexp_replace(p.prosrc,'[[:space:]]+',' ','g')) =
           'BEGIN IF ROW( NEW.event_id,NEW.event_type,NEW.strikeflow_command_id,NEW.workspace_key, NEW.workspace_id,NEW.project_id,NEW.issue_id,NEW.issue_identifier, NEW.inbox_item_id,NEW.root_comment_id,NEW.member_comment_id, NEW.continuation_task_id,NEW.recipient_id,NEW.agent_id, NEW.agent_comment_id,NEW.agent_comment_parent_id, NEW.agent_comment_content,NEW.agent_comment_type,NEW.occurred_at ) IS DISTINCT FROM ROW( OLD.event_id,OLD.event_type,OLD.strikeflow_command_id,OLD.workspace_key, OLD.workspace_id,OLD.project_id,OLD.issue_id,OLD.issue_identifier, OLD.inbox_item_id,OLD.root_comment_id,OLD.member_comment_id, OLD.continuation_task_id,OLD.recipient_id,OLD.agent_id, OLD.agent_comment_id,OLD.agent_comment_parent_id, OLD.agent_comment_content,OLD.agent_comment_type,OLD.occurred_at ) THEN RAISE EXCEPTION ''strikeflow response outbox identity is immutable''; END IF; RETURN NEW; END;'
     )
  THEN RAISE EXCEPTION 'immutability trigger function body mismatch'; END IF;
  IF EXISTS (SELECT event_id FROM public.strikeflow_response_outbox GROUP BY event_id HAVING count(*)>1) THEN RAISE EXCEPTION 'duplicate response event identity'; END IF;
  IF (SELECT count(*) FROM pg_index WHERE indexrelid IN (
      'public.idx_strikeflow_connector_reply_command_unique'::regclass,
      'public.idx_strikeflow_response_outbox_event_unique'::regclass,
      'public.idx_strikeflow_response_outbox_due'::regclass,
      'public.idx_strikeflow_response_outbox_event_id_unique'::regclass
    ) AND indisvalid AND indisready AND indislive) <> 4 THEN RAISE EXCEPTION 'response index catalog mismatch'; END IF;
  IF (SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am a ON a.oid=c.relam WHERE i.indexrelid IN (
      'public.idx_strikeflow_connector_reply_command_unique'::regclass,
      'public.idx_strikeflow_response_outbox_event_unique'::regclass,
      'public.idx_strikeflow_response_outbox_due'::regclass,
      'public.idx_strikeflow_response_outbox_event_id_unique'::regclass
    ) AND a.amname='btree') <> 4 THEN RAISE EXCEPTION 'response index access method mismatch'; END IF;
  IF NOT (SELECT indisunique FROM pg_index WHERE indexrelid='public.idx_strikeflow_response_outbox_event_id_unique'::regclass) THEN RAISE EXCEPTION 'event id index is not unique'; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_index WHERE indexrelid='public.idx_strikeflow_connector_reply_command_unique'::regclass AND indrelid='public.strikeflow_connector_reply_receipt'::regclass AND indisunique AND indnkeyatts=1 AND indexprs IS NULL AND pg_get_indexdef(indexrelid,1,true)='strikeflow_command_id' AND regexp_replace(pg_get_expr(indpred,indrelid),'[[:space:]()]','','g')='strikeflow_command_idISNOTNULL') THEN RAISE EXCEPTION 'receipt command index semantics mismatch'; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_index WHERE indexrelid='public.idx_strikeflow_response_outbox_event_unique'::regclass AND indrelid='public.strikeflow_response_outbox'::regclass AND indisunique AND indnkeyatts=3 AND pg_get_indexdef(indexrelid,1,true)='event_type' AND pg_get_indexdef(indexrelid,2,true)='continuation_task_id' AND regexp_replace(pg_get_indexdef(indexrelid,3,true),'[[:space:]()]','','g')='COALESCEagent_comment_id,''00000000-0000-0000-0000-000000000000''::uuid' AND indpred IS NULL) THEN RAISE EXCEPTION 'outbox natural event index semantics mismatch'; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_index WHERE indexrelid='public.idx_strikeflow_response_outbox_due'::regclass AND indrelid='public.strikeflow_response_outbox'::regclass AND NOT indisunique AND indnkeyatts=2 AND indexprs IS NULL AND pg_get_indexdef(indexrelid,1,true)='next_attempt_at' AND pg_get_indexdef(indexrelid,2,true)='created_at' AND regexp_replace(pg_get_expr(indpred,indrelid),'[[:space:]()]','','g')='delivered_atISNULL') THEN RAISE EXCEPTION 'outbox due index semantics mismatch'; END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_index WHERE indexrelid='public.idx_strikeflow_response_outbox_event_id_unique'::regclass AND indrelid='public.strikeflow_response_outbox'::regclass AND indisunique AND indnkeyatts=1 AND pg_get_indexdef(indexrelid,1,true)='event_id' AND indexprs IS NULL AND indpred IS NULL) THEN RAISE EXCEPTION 'outbox event id index semantics mismatch'; END IF;
END $$;
SQL

if [ "$mode" != rollback-preflight ]; then
  attention_count=$(docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT count(*) FROM strikeflow_response_outbox WHERE needs_attention_at IS NOT NULL"')
  test "$attention_count" = 0
fi

case "$mode" in
  adoption-before-start|adoption-after-start)
    # shellcheck source=/dev/null
    . "$adoption_contract"
    validate_adoption_manifest "$adoption_manifest"
    verify_adoption_config
    verify_response_reconciliation_stopped
    verify_adoption_source_catalog
    test "$(sed -n 's/^STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE=//p' "$config_file")" = receipt_lineage
    test -z "$(sed -n 's/^STRIKEFLOW_RESPONSE_COMMAND_IDS=//p' "$config_file")"
    if [ "$mode" = adoption-before-start ]; then
      verify_adoption_outbox initial
    else
      verify_adoption_outbox delivered
    fi
    ;;
esac

echo "enabled_multica_response_publisher_ok mode=$mode image=$image_digest release=$(basename "$release_dir")"

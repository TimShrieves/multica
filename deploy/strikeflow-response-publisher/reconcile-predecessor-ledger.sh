#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
  echo "run as root" >&2
  exit 1
fi
if [ "$#" -ne 6 ] || [ "$6" != "--confirm-reconcile" ]; then
  echo "usage: $0 RELEASE_DIR PREFLIGHT_DIR ENCRYPTED_BACKUP BACKUP_SHA256 EVIDENCE_DIR --confirm-reconcile" >&2
  exit 64
fi

release_dir=$(readlink -f "$1")
preflight_dir=$(readlink -f "$2")
backup_file=$(readlink -f "$3")
backup_sha=$(readlink -f "$4")
evidence_parent=$(readlink -f "$(dirname "$5")")
evidence_name=$(basename "$5")
evidence_dir=$evidence_parent/$evidence_name
lock_file=/run/lock/multica-response-publisher-deploy.lock
image_digest=$(sed -n 's/^image_digest=//p' "$release_dir/ARTIFACTS")

case "$release_dir" in /opt/multica-response-publisher/releases/*) ;; *) echo "release path escaped install root" >&2; exit 1;; esac
case "$preflight_dir" in /var/backups/multica-response-publisher/*) ;; *) echo "preflight path escaped backup root" >&2; exit 1;; esac
case "$backup_file" in /var/backups/multica/*.dump.age) ;; *) echo "invalid encrypted backup path" >&2; exit 1;; esac
case "$backup_sha" in /var/backups/multica/*.sha256) ;; *) echo "invalid backup checksum path" >&2; exit 1;; esac
test "$evidence_parent" = /var/backups/multica-response-publisher
case "$evidence_name" in reconcile-predecessor-ledger-*) ;; *) echo "invalid evidence basename" >&2; exit 1;; esac
case "$evidence_name" in *[!A-Za-z0-9._-]*) echo "invalid evidence basename" >&2; exit 1;; esac
test ! -e "$evidence_dir"
test "$(stat -c '%U:%G %a' "$backup_file")" = "root:root 600"
test "$(stat -c '%U:%G %a' "$backup_sha")" = "root:root 600"
test "$(wc -l <"$backup_sha" | tr -d ' ')" -eq 1
backup_expected_hash=$(awk 'NF == 2 {print $1}' "$backup_sha")
backup_expected_name=$(awk 'NF == 2 {print $2}' "$backup_sha" | sed 's/^\*//')
# Accept the absolute filename emitted by `sha256sum /absolute/path` and the
# basename emitted when the sidecar was created from its containing directory.
test "$backup_expected_name" = "$backup_file" || test "$backup_expected_name" = "$(basename "$backup_file")"
test "$backup_expected_hash" = "$(sha256sum "$backup_file" | awk '{print $1}')"
test "$(readlink -f /opt/multica-response-publisher/current)" = "$release_dir"
test -f "$release_dir/ARTIFACTS" -a -f "$release_dir/SHA256SUMS"
test "$(stat -c '%U:%G' "$release_dir")" = root:root
test "$(stat -c '%U:%G %a' "$preflight_dir")" = "root:root 700"
test -z "$(find "$release_dir" -xdev -type l -print -quit)"
test -z "$(find "$release_dir" -xdev \( ! -user root -o ! -group root \) -print -quit)"
test -z "$(find "$release_dir" -xdev -perm /022 -print -quit)"
(cd "$release_dir" && sha256sum -c SHA256SUMS >/dev/null)
(cd / && sha256sum -c "$preflight_dir/active-compose.sha256" >/dev/null)
test "$(sed -n 's/^migrations_applied_to_production=//p' "$release_dir/ARTIFACTS")" = false
test "$(docker image inspect "$image_digest" --format '{{.Id}}')" = "$(sed -n 's/^image_id=//p' "$release_dir/ARTIFACTS")"
docker image inspect "$image_digest" --format '{{range .RepoDigests}}{{println .}}{{end}}' | grep -Fqx "$image_digest"
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.state"}}')" = dormant
test "$(docker image inspect "$image_digest" --format '{{index .Config.Labels "co.strikeflow.response-publisher.source"}}')" = "$(sed -n 's/^source_commit=//p' "$release_dir/ARTIFACTS")"

exec 9>"$lock_file"
flock -n 9 || { echo "another response deployment is running" >&2; exit 1; }
install -d -o root -g root -m 0700 "$evidence_dir"
install -o root -g root -m 0600 "$backup_sha" "$evidence_dir/backup.sha256"
printf '%s\n' "$backup_file" >"$evidence_dir/encrypted-backup.path"
chmod 0600 "$evidence_dir/encrypted-backup.path"

for unit in \
  multica-response-publisher.service multica-response-publisher.timer \
  strikeflow-multica-content-dispatch.service strikeflow-multica-content-dispatch.timer \
  strikeflow-multica-content-ongoing.service strikeflow-multica-content-ongoing.timer; do
  systemctl is-active --quiet "$unit" 2>/dev/null && { echo "$unit is active" >&2; exit 1; } || true
  enabled_state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  # `systemctl is-enabled --quiet` also succeeds for static units. Static
  # response services are intentionally dormant and must not be rejected;
  # reject only states that create an enablement path at boot/runtime.
  case "$enabled_state" in
    enabled|enabled-runtime|linked|linked-runtime|alias|generated|transient)
      echo "$unit is enabled ($enabled_state)" >&2
      exit 1
      ;;
  esac
  systemctl show "$unit" --property=MainPID --value 2>/dev/null || true
done >"$evidence_dir/units.before"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.before"
done
test "$(cat "$evidence_dir/multica-backend-1.before")" = "$(cat "$preflight_dir/multica-backend-1.identity")"
test "$(cat "$evidence_dir/multica-frontend-1.before")" = "$(cat "$preflight_dir/multica-frontend-1.identity")"
test "$(cat "$evidence_dir/multica-postgres-1.before")" = "$(cat "$preflight_dir/multica-postgres-1.identity")"
docker inspect multica-backend-1 --format '{{json .Config.Env}}' |
  python3 -c '
import json, sys
env = dict(item.split("=", 1) for item in json.load(sys.stdin) if "=" in item)
response = {k: v for k, v in env.items() if k.startswith("STRIKEFLOW_RESPONSE_")}
if response.pop("STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED", None) != "false" or any(response.values()):
    raise SystemExit("publisher must remain disabled during ledger reconciliation")
'
docker inspect multica-backend-1 --format '{{json .Mounts}}' |
  python3 -c 'import json,sys; raise SystemExit("HMAC mount present") if any(m.get("Destination")=="/run/secrets/strikeflow_response_hmac" for m in json.load(sys.stdin)) else None'
docker exec -i multica-postgres-1 sh -c 'psql -X -A -t -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version || '\''|'\'' || applied_at FROM schema_migrations ORDER BY version"' \
  >"$evidence_dir/migration-ledger.before"

cat >"$evidence_dir/reconcile.sql" <<'SQL'
SELECT pg_advisory_lock(7244554146635925501);
SELECT pg_advisory_lock(hashtextextended('multica.strikeflow.response.producer.freeze', 0));
BEGIN;
DO $$
DECLARE
  old_versions text[] := ARRAY[
    '900001_strikeflow_response_outbox',
    '900002_strikeflow_connector_reply_command_unique',
    '900003_strikeflow_response_outbox_event_unique',
    '900004_strikeflow_response_outbox_due_index',
    '900005_strikeflow_response_outbox_event_id_unique'
  ];
  new_versions text[] := ARRAY[
    '253_strikeflow_response_outbox',
    '254_strikeflow_connector_reply_command_unique',
    '255_strikeflow_response_outbox_event_unique',
    '256_strikeflow_response_outbox_due_index',
    '257_strikeflow_response_outbox_event_id_unique'
  ];
  old_count integer;
  new_count integer;
  outbox_before text;
  reply_before text;
  content_before text;
  ledger_before text;
  ledger_after text;
  outbox_after text;
  reply_after text;
  content_after text;
  already_reconciled boolean := false;
BEGIN
  SELECT count(*) INTO old_count FROM schema_migrations WHERE version = ANY(old_versions);
  SELECT count(*) INTO new_count FROM schema_migrations WHERE version = ANY(new_versions);
  IF old_count = 5 AND new_count = 5 THEN
    already_reconciled := true;
  ELSIF old_count <> 5 OR new_count <> 0 THEN
    RAISE EXCEPTION 'predecessor ledger must contain exactly five old rows and no aliases (old %, new %)', old_count, new_count;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = '235_strikeflow_connector_principal') THEN
    RAISE EXCEPTION 'predecessor content connector ledger row is absent';
  END IF;
  IF EXISTS (SELECT 1 FROM schema_migrations WHERE version = '259_strikeflow_content_reply_receipt_unique') THEN
    RAISE EXCEPTION '259 must remain pending during predecessor reconciliation';
  END IF;
  IF EXISTS (SELECT 1 FROM schema_migrations WHERE version IN (
    '258_strikeflow_content_reply_connector',
    '260_strikeflow_response_outbox_identity_immutable'
  )) THEN
    RAISE EXCEPTION 'canonical content/identity migrations must remain pending during predecessor reconciliation';
  END IF;
  IF to_regclass('public.strikeflow_response_outbox') IS NULL
     OR to_regclass('public.strikeflow_connector_reply_receipt') IS NULL
     OR to_regclass('public.strikeflow_connector_token') IS NULL
     OR to_regclass('public.strikeflow_connector_audit') IS NULL
     OR to_regclass('public.strikeflow_connector_content_reply_receipt') IS NULL THEN
    RAISE EXCEPTION 'required response tables are absent';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_connector_reply_receipt' AND column_name='strikeflow_command_id' AND data_type='uuid')
     OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid='public.strikeflow_connector_reply_receipt'::regclass AND tgname='strikeflow_reply_command_binding_immutable' AND tgfoid='public.reject_strikeflow_reply_command_binding_change()'::regprocedure AND tgtype=19 AND tgenabled='O' AND NOT tgisinternal)
     OR NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid='public.strikeflow_response_outbox'::regclass AND tgname='strikeflow_response_outbox_identity_immutable' AND tgfoid='public.reject_strikeflow_response_outbox_identity_change()'::regprocedure AND tgtype=19 AND tgenabled='O' AND NOT tgisinternal)
     OR (SELECT count(*) FROM pg_trigger WHERE tgrelid IN ('public.strikeflow_connector_reply_receipt'::regclass,'public.strikeflow_response_outbox'::regclass) AND NOT tgisinternal) <> 2 THEN
    RAISE EXCEPTION 'predecessor reply-receipt command binding is not exact';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang
    WHERE p.oid='public.reject_strikeflow_reply_command_binding_change()'::regprocedure
      AND l.lanname='plpgsql' AND p.prorettype='trigger'::regtype
      AND p.prosrc LIKE '%NEW.strikeflow_command_id IS DISTINCT FROM OLD.strikeflow_command_id%'
      AND p.prosrc LIKE '%strikeflow command binding is immutable%'
  ) OR NOT EXISTS (
    SELECT 1 FROM pg_proc p JOIN pg_language l ON l.oid=p.prolang
    WHERE p.oid='public.reject_strikeflow_response_outbox_identity_change()'::regprocedure
      AND l.lanname='plpgsql' AND p.prorettype='trigger'::regtype
      AND p.prosrc LIKE '%NEW.event_id%OLD.event_id%'
      AND p.prosrc LIKE '%NEW.occurred_at%OLD.occurred_at%'
      AND p.prosrc LIKE '%strikeflow response outbox identity is immutable%'
  ) THEN
    RAISE EXCEPTION 'predecessor immutable trigger function body is not exact';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_index
    WHERE indexrelid='public.idx_strikeflow_connector_reply_command_unique'::regclass
      AND indrelid='public.strikeflow_connector_reply_receipt'::regclass
      AND indisunique AND indisvalid AND indisready AND indislive AND indnkeyatts=1
      AND pg_get_indexdef(indexrelid,1,true)='strikeflow_command_id'
      AND regexp_replace(pg_get_expr(indpred,indrelid),'[[:space:]()]','','g')='strikeflow_command_idISNOTNULL'
  ) THEN
    RAISE EXCEPTION 'predecessor command uniqueness index is absent';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_index
    WHERE indexrelid='public.idx_strikeflow_response_outbox_event_unique'::regclass
      AND indrelid='public.strikeflow_response_outbox'::regclass
      AND indisunique AND indisvalid AND indisready AND indislive AND indnkeyatts=3
      AND pg_get_indexdef(indexrelid,1,true)='event_type'
      AND pg_get_indexdef(indexrelid,2,true)='continuation_task_id'
      AND regexp_replace(pg_get_indexdef(indexrelid,3,true),'[[:space:]()]','','g')='COALESCEagent_comment_id,''00000000-0000-0000-0000-000000000000''::uuid'
      AND indpred IS NULL
  ) OR NOT EXISTS (
    SELECT 1 FROM pg_index
    WHERE indexrelid='public.idx_strikeflow_response_outbox_due'::regclass
      AND indrelid='public.strikeflow_response_outbox'::regclass
      AND NOT indisunique AND indisvalid AND indisready AND indislive AND indnkeyatts=2
      AND pg_get_indexdef(indexrelid,1,true)='next_attempt_at'
      AND pg_get_indexdef(indexrelid,2,true)='created_at'
      AND regexp_replace(pg_get_expr(indpred,indrelid),'[[:space:]()]','','g')='delivered_atISNULL'
  ) OR NOT EXISTS (
    SELECT 1 FROM pg_index
    WHERE indexrelid='public.idx_strikeflow_response_outbox_event_id_unique'::regclass
      AND indrelid='public.strikeflow_response_outbox'::regclass
      AND indisunique AND indisvalid AND indisready AND indislive AND indnkeyatts=1
      AND pg_get_indexdef(indexrelid,1,true)='event_id' AND indpred IS NULL
  ) THEN
    RAISE EXCEPTION 'predecessor outbox indexes are not exact';
  END IF;
  IF NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_connector_token' AND column_name='agent_id' AND data_type='uuid')
     OR NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='strikeflow_connector_audit' AND column_name='root_comment_id' AND data_type='uuid') THEN
    RAISE EXCEPTION 'predecessor content connector columns are absent';
  END IF;
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid='public.strikeflow_connector_token'::regclass AND contype='c' AND convalidated
      AND pg_get_constraintdef(oid) LIKE '%content:reply%'
      AND pg_get_constraintdef(oid) LIKE '%cardinality(project_ids) = 1%'
      AND pg_get_constraintdef(oid) LIKE '%agent_id IS NOT NULL%'
  ) THEN
    RAISE EXCEPTION 'predecessor content purpose constraint semantics are absent';
  END IF;
  IF (SELECT count(*) FROM pg_constraint WHERE conrelid='public.strikeflow_connector_content_reply_receipt'::regclass AND contype='c' AND convalidated) <> 5
     OR (SELECT count(*) FROM pg_constraint WHERE conrelid='public.strikeflow_connector_content_reply_receipt'::regclass AND contype='f') <> 0
     OR NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid='public.strikeflow_connector_content_reply_receipt'::regclass AND contype='p') THEN
    RAISE EXCEPTION 'predecessor content receipt constraints are not exact';
  END IF;
  IF to_regclass('public.idx_strikeflow_content_reply_receipt_unique') IS NOT NULL THEN
    RAISE EXCEPTION '259 unique content receipt index must remain pending';
  END IF;
  IF EXISTS (SELECT 1 FROM strikeflow_response_outbox WHERE delivered_at IS NULL OR needs_attention_at IS NOT NULL OR lease_until IS NOT NULL) THEN
    RAISE EXCEPTION 'unsafe response outbox rows prevent ledger reconciliation';
  END IF;
  SELECT md5(COALESCE(string_agg(to_jsonb(o)::text, '|' ORDER BY o.event_id), '')) INTO outbox_before FROM strikeflow_response_outbox o;
  SELECT md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.token_id, r.idempotency_key), '')) INTO reply_before FROM strikeflow_connector_reply_receipt r;
  SELECT md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.token_id, r.idempotency_key), '')) INTO content_before FROM strikeflow_connector_content_reply_receipt r;
  SELECT md5(COALESCE(string_agg(version || '|' || applied_at::text, '|' ORDER BY version), '')) INTO ledger_before
    FROM schema_migrations WHERE version <> ALL(new_versions);
  IF NOT already_reconciled THEN
    INSERT INTO schema_migrations(version, applied_at)
    SELECT v.new_version, old.applied_at
    FROM (VALUES
      ('900001_strikeflow_response_outbox','253_strikeflow_response_outbox'),
      ('900002_strikeflow_connector_reply_command_unique','254_strikeflow_connector_reply_command_unique'),
      ('900003_strikeflow_response_outbox_event_unique','255_strikeflow_response_outbox_event_unique'),
      ('900004_strikeflow_response_outbox_due_index','256_strikeflow_response_outbox_due_index'),
      ('900005_strikeflow_response_outbox_event_id_unique','257_strikeflow_response_outbox_event_id_unique')
    ) AS v(old_version, new_version)
    JOIN schema_migrations old ON old.version = v.old_version;
  END IF;
  SELECT md5(COALESCE(string_agg(to_jsonb(o)::text, '|' ORDER BY o.event_id), '')) INTO outbox_after FROM strikeflow_response_outbox o;
  SELECT md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.token_id, r.idempotency_key), '')) INTO reply_after FROM strikeflow_connector_reply_receipt r;
  SELECT md5(COALESCE(string_agg(to_jsonb(r)::text, '|' ORDER BY r.token_id, r.idempotency_key), '')) INTO content_after FROM strikeflow_connector_content_reply_receipt r;
  SELECT md5(COALESCE(string_agg(version || '|' || applied_at::text, '|' ORDER BY version), '')) INTO ledger_after FROM schema_migrations WHERE version NOT IN (SELECT unnest(new_versions));
  IF outbox_before IS DISTINCT FROM outbox_after OR reply_before IS DISTINCT FROM reply_after OR content_before IS DISTINCT FROM content_after OR ledger_before IS DISTINCT FROM ledger_after THEN
    RAISE EXCEPTION 'non-schema response evidence changed during predecessor reconciliation';
  END IF;
  RAISE NOTICE 'predecessor ledger aliases inserted; 259 remains pending';
END $$;
COMMIT;
SELECT pg_advisory_unlock(hashtextextended('multica.strikeflow.response.producer.freeze', 0));
SELECT pg_advisory_unlock(7244554146635925501);
SQL
chmod 0600 "$evidence_dir/reconcile.sql"

record_failure() {
  status=$?
  trap - EXIT
  trap '' HUP INT TERM
  printf 'reconciliation_status=%s\n' "$status" >"$evidence_dir/status.txt"
  docker exec -i multica-postgres-1 sh -c 'psql -X -A -t -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version FROM schema_migrations ORDER BY version"' >"$evidence_dir/migration-ledger.failure-final" 2>&1 || true
  for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
    docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.failure-final" 2>&1 || true
  done
  chmod 0600 "$evidence_dir"/* 2>/dev/null || true
  (cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS) || true
  chmod 0600 "$evidence_dir/SHA256SUMS" 2>/dev/null || true
  exit "$status"
}
trap record_failure EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

docker exec -i multica-postgres-1 sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  <"$evidence_dir/reconcile.sql" >"$evidence_dir/reconcile.log" 2>&1
docker exec -i multica-postgres-1 sh -c 'psql -X -A -t -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version FROM schema_migrations ORDER BY version"' >"$evidence_dir/migration-ledger.after"
for container in multica-backend-1 multica-frontend-1 multica-postgres-1; do
  docker inspect -f '{{.Id}}|{{.Image}}|{{.State.Running}}|{{json .NetworkSettings.Ports}}' "$container" >"$evidence_dir/$container.after"
  cmp "$evidence_dir/$container.before" "$evidence_dir/$container.after"
done
grep -Fx '253_strikeflow_response_outbox' "$evidence_dir/migration-ledger.after" >/dev/null
grep -Fx '235_strikeflow_connector_principal' "$evidence_dir/migration-ledger.after" >/dev/null
if grep -Fx '258_strikeflow_content_reply_connector' "$evidence_dir/migration-ledger.after" >/dev/null; then
  echo "258 must remain pending for the canonical content connector migration" >&2
  exit 1
fi
if grep -Fx '259_strikeflow_content_reply_receipt_unique' "$evidence_dir/migration-ledger.after" >/dev/null; then
  echo "259 must remain pending" >&2
  exit 1
fi
date -u +%FT%TZ >"$evidence_dir/completed-at.txt"
printf 'reconciliation_status=0\n' >"$evidence_dir/status.txt"
(cd "$evidence_dir" && find . -type f ! -name SHA256SUMS -print0 | LC_ALL=C sort -z | xargs -0 sha256sum >SHA256SUMS)
chmod 0600 "$evidence_dir"/*
trap - EXIT HUP INT TERM
echo "multica_predecessor_ledger_reconciled evidence=$evidence_dir"

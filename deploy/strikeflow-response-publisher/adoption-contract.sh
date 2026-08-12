#!/bin/sh
# Shared, read-only contract for adopting one exact reconciled response pair.
# Callers must set release_dir and config_file before sourcing this file.

validate_adoption_manifest() {
  adoption_manifest=$(readlink -f "$1")
  case "$adoption_manifest" in /var/backups/multica-response-publisher/*) ;; *)
    echo "adoption manifest escaped backup root" >&2
    return 1
  esac
  test -f "$adoption_manifest" -a ! -L "$adoption_manifest"
  test "$(stat -c '%U:%G %a' "$adoption_manifest")" = "root:root 600"
  python3 - "$adoption_manifest" <<'PY'
import datetime, hashlib, pathlib, re, sys, uuid

values = {}
for line in pathlib.Path(sys.argv[1]).read_text(encoding="utf-8").splitlines():
    key, sep, value = line.partition("=")
    if not sep or not key or key in values:
        raise SystemExit("invalid adoption manifest shape")
    values[key] = value
expected = {
    "ADOPTION_CONTRACT_VERSION", "ADOPTION_EVENT_IDS", "ADOPTION_COMMAND_ID",
    "ADOPTION_WORKSPACE_KEY", "ADOPTION_WORKSPACE_ID", "ADOPTION_PROJECT_ID",
    "ADOPTION_ISSUE_ID", "ADOPTION_ISSUE_IDENTIFIER", "ADOPTION_INBOX_ITEM_ID",
    "ADOPTION_ROOT_COMMENT_ID", "ADOPTION_MEMBER_COMMENT_ID",
    "ADOPTION_CONTINUATION_TASK_ID", "ADOPTION_RECIPIENT_ID", "ADOPTION_AGENT_ID",
    "ADOPTION_AGENT_COMMENT_ID", "ADOPTION_AGENT_COMMENT_PARENT_ID",
    "ADOPTION_COMMENT_CONTENT_SHA256", "ADOPTION_COMMENT_TYPE",
    "ADOPTION_COMMENT_OCCURRED_AT", "ADOPTION_COMPLETION_OCCURRED_AT",
    "ADOPTION_NOT_BEFORE", "ADOPTION_INITIAL_STATE",
    "ADOPTION_COMMENT_ATTEMPT_COUNT", "ADOPTION_COMPLETION_ATTEMPT_COUNT",
    "ADOPTION_COMMENT_NEXT_ATTEMPT_AT", "ADOPTION_COMPLETION_NEXT_ATTEMPT_AT",
    "ADOPTION_COMMENT_LEASE_UNTIL", "ADOPTION_COMPLETION_LEASE_UNTIL",
    "ADOPTION_COMMENT_DELIVERED_AT", "ADOPTION_COMPLETION_DELIVERED_AT",
    "ADOPTION_COMMENT_NEEDS_ATTENTION_AT", "ADOPTION_COMPLETION_NEEDS_ATTENTION_AT",
    "ADOPTION_COMMENT_LAST_ERROR", "ADOPTION_COMPLETION_LAST_ERROR",
    "ADOPTION_RECEIPT_TOKEN_ID", "ADOPTION_RECEIPT_IDEMPOTENCY_KEY",
    "ADOPTION_RECEIPT_PAYLOAD_HASH", "ADOPTION_RECEIPT_CREATED_AT",
    "ADOPTION_RECEIPT_COMMITTED_AT", "ADOPTION_RECONCILED_AT",
    "ADOPTION_STRIKEFLOW_DEPLOYMENT_ID", "ADOPTION_STRIKEFLOW_SOURCE_COMMIT",
}
if set(values) != expected or values["ADOPTION_CONTRACT_VERSION"] != "2":
    raise SystemExit("invalid adoption manifest contract")
event_ids = values["ADOPTION_EVENT_IDS"].split(",")
if len(event_ids) != 2 or len(set(event_ids)) != 2:
    raise SystemExit("adoption requires exactly two distinct response event ids")
uuid_keys = [
    "ADOPTION_COMMAND_ID", "ADOPTION_WORKSPACE_ID", "ADOPTION_PROJECT_ID",
    "ADOPTION_ISSUE_ID", "ADOPTION_INBOX_ITEM_ID", "ADOPTION_ROOT_COMMENT_ID",
    "ADOPTION_MEMBER_COMMENT_ID", "ADOPTION_CONTINUATION_TASK_ID",
    "ADOPTION_RECIPIENT_ID", "ADOPTION_AGENT_ID", "ADOPTION_AGENT_COMMENT_ID",
    "ADOPTION_RECEIPT_TOKEN_ID", "ADOPTION_RECEIPT_IDEMPOTENCY_KEY",
]
for value in event_ids + [values[k] for k in uuid_keys]:
    if str(uuid.UUID(value)) != value:
        raise SystemExit("adoption manifest UUID is not canonical")
parent = values["ADOPTION_AGENT_COMMENT_PARENT_ID"]
if parent != "null" and str(uuid.UUID(parent)) != parent:
    raise SystemExit("adoption parent UUID is invalid")
for key in ("ADOPTION_WORKSPACE_KEY", "ADOPTION_ISSUE_IDENTIFIER", "ADOPTION_COMMENT_TYPE"):
    if not re.fullmatch(r"[A-Za-z0-9_.:-]{1,100}", values[key]):
        raise SystemExit(f"unsafe manifest scalar: {key}")
for key in ("ADOPTION_COMMENT_CONTENT_SHA256", "ADOPTION_RECEIPT_PAYLOAD_HASH"):
    if not re.fullmatch(r"[a-f0-9]{64}", values[key]):
        raise SystemExit(f"invalid digest: {key}")
for key in ("ADOPTION_COMMENT_ATTEMPT_COUNT", "ADOPTION_COMPLETION_ATTEMPT_COUNT"):
    if not re.fullmatch(r"[0-9]+", values[key]) or int(values[key]) >= 12:
        raise SystemExit(f"invalid attempt state: {key}")
timestamp_keys = [
    "ADOPTION_COMMENT_OCCURRED_AT", "ADOPTION_COMPLETION_OCCURRED_AT", "ADOPTION_NOT_BEFORE",
    "ADOPTION_COMMENT_NEXT_ATTEMPT_AT", "ADOPTION_COMPLETION_NEXT_ATTEMPT_AT",
    "ADOPTION_RECEIPT_CREATED_AT", "ADOPTION_RECEIPT_COMMITTED_AT", "ADOPTION_RECONCILED_AT",
]
nullable_timestamp_keys = [
    "ADOPTION_COMMENT_LEASE_UNTIL", "ADOPTION_COMPLETION_LEASE_UNTIL",
    "ADOPTION_COMMENT_DELIVERED_AT", "ADOPTION_COMPLETION_DELIVERED_AT",
    "ADOPTION_COMMENT_NEEDS_ATTENTION_AT", "ADOPTION_COMPLETION_NEEDS_ATTENTION_AT",
]
def parse_time(value):
    if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", value):
        raise SystemExit("timestamp is not exact RFC3339")
    parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None or parsed.utcoffset() is None:
        raise SystemExit("timestamp has no timezone")
    return parsed.astimezone(datetime.timezone.utc)
for key in timestamp_keys:
    parse_time(values[key])
for key in nullable_timestamp_keys:
    if values[key] != "null": parse_time(values[key])
if values["ADOPTION_COMMENT_NEEDS_ATTENTION_AT"] != "null" or values["ADOPTION_COMPLETION_NEEDS_ATTENTION_AT"] != "null":
    raise SystemExit("adoption cannot include attention rows")
for key in ("ADOPTION_COMMENT_LAST_ERROR", "ADOPTION_COMPLETION_LAST_ERROR"):
    if values[key] != "null" and not re.fullmatch(r"[a-f0-9]{64}", values[key]):
        raise SystemExit("last-error evidence must be null or a SHA256 digest")
state = values["ADOPTION_INITIAL_STATE"]
if state == "pending_pair":
    if values["ADOPTION_COMMENT_DELIVERED_AT"] != "null" or values["ADOPTION_COMPLETION_DELIVERED_AT"] != "null":
        raise SystemExit("pending_pair delivery state mismatch")
    if any(values[key] != "null" for key in (
        "ADOPTION_COMMENT_LEASE_UNTIL", "ADOPTION_COMPLETION_LEASE_UNTIL",
        "ADOPTION_COMMENT_LAST_ERROR", "ADOPTION_COMPLETION_LAST_ERROR")):
        raise SystemExit("pending_pair must be clean and unleased")
elif state == "comment_delivered_completion_pending":
    if values["ADOPTION_COMMENT_DELIVERED_AT"] == "null" or values["ADOPTION_COMPLETION_DELIVERED_AT"] != "null":
        raise SystemExit("partial delivery state is not resumable in order")
    if values["ADOPTION_COMMENT_LEASE_UNTIL"] != "null" or values["ADOPTION_COMMENT_LAST_ERROR"] != "null":
        raise SystemExit("delivered comment must not retain lease or error state")
    completion_lease = values["ADOPTION_COMPLETION_LEASE_UNTIL"]
    if completion_lease != "null" and parse_time(completion_lease) >= datetime.datetime.now(datetime.timezone.utc):
        raise SystemExit("partial completion lease is still active")
    if parse_time(values["ADOPTION_COMPLETION_NEXT_ATTEMPT_AT"]) > datetime.datetime.now(datetime.timezone.utc):
        raise SystemExit("partial completion retry is not due")
else:
    raise SystemExit("invalid adoption initial state")
if parse_time(values["ADOPTION_COMMENT_OCCURRED_AT"]) > parse_time(values["ADOPTION_COMPLETION_OCCURRED_AT"]):
    raise SystemExit("adoption response order mismatch")
age = datetime.datetime.now(datetime.timezone.utc) - parse_time(values["ADOPTION_RECONCILED_AT"])
if age < datetime.timedelta(0) or age > datetime.timedelta(hours=24):
    raise SystemExit("reconciliation evidence must be from the previous 24 hours")
floor = parse_time(values["ADOPTION_NOT_BEFORE"])
if parse_time(values["ADOPTION_RECEIPT_CREATED_AT"]) >= floor or parse_time(values["ADOPTION_RECEIPT_COMMITTED_AT"]) >= floor:
    raise SystemExit("adoption floor must be newer than the reconciled receipt")
if parse_time(values["ADOPTION_COMMENT_OCCURRED_AT"]) >= floor or parse_time(values["ADOPTION_COMPLETION_OCCURRED_AT"]) >= floor:
    raise SystemExit("adoption floor must be newer than the adopted response pair")
if not re.fullmatch(r"dep-[a-z0-9]{16,64}", values["ADOPTION_STRIKEFLOW_DEPLOYMENT_ID"]):
    raise SystemExit("invalid StrikeFlow deployment evidence")
if not re.fullmatch(r"[a-f0-9]{40}", values["ADOPTION_STRIKEFLOW_SOURCE_COMMIT"]):
    raise SystemExit("invalid StrikeFlow source evidence")
PY
  # `value` is consumed by eval after the manifest's exact key/value grammar
  # and every value type have been validated by the Python block above.
  # shellcheck disable=SC2034
  while IFS='=' read -r key value; do
    case "$key" in ADOPTION_*) eval "adopt_${key#ADOPTION_}=\$value" ;; esac
  done <"$adoption_manifest"
}

# config_file is an intentional caller contract; adoption_manifest and its
# adopt_* values are populated by validate_adoption_manifest before this call.
# shellcheck disable=SC2154
verify_adoption_config() {
  python3 - "$config_file" "$adoption_manifest" <<'PY'
import pathlib, sys
def load(path):
    return dict(line.split("=", 1) for line in pathlib.Path(path).read_text().splitlines())
config, manifest = load(sys.argv[1]), load(sys.argv[2])
expected = {
    "STRIKEFLOW_RESPONSE_WORKSPACE_KEY": manifest["ADOPTION_WORKSPACE_KEY"],
    "STRIKEFLOW_RESPONSE_WORKSPACE_ID": manifest["ADOPTION_WORKSPACE_ID"],
    "STRIKEFLOW_RESPONSE_PROJECT_IDS": manifest["ADOPTION_PROJECT_ID"],
    "STRIKEFLOW_RESPONSE_RECIPIENT_ID": manifest["ADOPTION_RECIPIENT_ID"],
    "STRIKEFLOW_RESPONSE_AGENT_ID": manifest["ADOPTION_AGENT_ID"],
    "STRIKEFLOW_RESPONSE_NOT_BEFORE": manifest["ADOPTION_NOT_BEFORE"],
    "STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE": "receipt_lineage",
    "STRIKEFLOW_RESPONSE_COMMAND_IDS": "",
}

for key, value in expected.items():
    if config.get(key) != value: raise SystemExit(f"adoption config mismatch: {key}")
if manifest["ADOPTION_ISSUE_ID"] in config["STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS"].split(","):
    raise SystemExit("adoption issue is excluded")
PY
}

# adopt_* variables are the validated manifest values loaded above.
# shellcheck disable=SC2154
verify_adoption_source_catalog() {
  # The adopted pair is already durable in the outbox. This repeats the
  # publisher's receipt-lineage eligibility gates and requires the immutable
  # recovery floor to expose zero historical source rows. RecoverOnce therefore
  # cannot insert or deliver an unmanifested row during the adoption recreate.
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<SQL
DO \$\$
DECLARE eligible_count integer; newer_receipt_count integer;
BEGIN
  WITH eligible AS (
    SELECT 'agent_comment.created'::text event_type, rr.strikeflow_command_id, q.id task_id,
           c.id comment_id, c.parent_id parent_id, c.content, c.type comment_type, c.created_at occurred_at
    FROM comment c
    JOIN agent_task_queue q ON q.id=c.source_task_id AND q.agent_id=c.author_id
    JOIN strikeflow_connector_reply_receipt rr ON rr.comment_id=q.trigger_comment_id AND rr.issue_id=q.issue_id
    JOIN strikeflow_connector_token t ON t.id=rr.token_id
    JOIN issue i ON i.id=q.issue_id AND i.workspace_id=c.workspace_id
    JOIN comment member ON member.id=rr.comment_id AND member.issue_id=i.id AND member.workspace_id=i.workspace_id
      AND member.parent_id=rr.root_comment_id AND member.author_type='member' AND member.author_id=t.recipient_id
    JOIN comment root ON root.id=rr.root_comment_id AND root.issue_id=i.id AND root.workspace_id=i.workspace_id
    WHERE c.author_type='agent' AND i.workspace_id='$adopt_WORKSPACE_ID'::uuid
      AND i.project_id='$adopt_PROJECT_ID'::uuid AND t.workspace_id=i.workspace_id
      AND t.recipient_id='$adopt_RECIPIENT_ID'::uuid AND q.agent_id='$adopt_AGENT_ID'::uuid
      AND q.originator_user_id='$adopt_RECIPIENT_ID'::uuid AND q.accountable_user_id='$adopt_RECIPIENT_ID'::uuid
      AND q.originator_source='direct_human' AND q.trigger_evidence_kind='comment'
      AND q.trigger_evidence_ref_id=rr.comment_id AND rr.strikeflow_command_id IS NOT NULL
      AND i.id<>ALL(ARRAY['b41bcb97-8b63-43f6-9d6c-4ee9e9ada891'::uuid,
                              '39dcf540-bedf-4449-bc71-2e9e15fa0573'::uuid,
                              'b1839f3d-97e5-449a-9059-21b3b393d096'::uuid])
      AND rr.created_at >= '$adopt_NOT_BEFORE'::timestamptz AND rr.committed_at >= '$adopt_NOT_BEFORE'::timestamptz
      AND c.created_at >= '$adopt_NOT_BEFORE'::timestamptz AND octet_length(c.content)<=1048576
      AND (SELECT count(*) FROM comment prior WHERE prior.source_task_id=q.id AND prior.author_type='agent'
           AND prior.author_id=q.agent_id AND (prior.created_at,prior.id)<=(c.created_at,c.id)) <= 100
      AND EXISTS (WITH RECURSIVE chain(id,parent_id) AS (SELECT c.id,c.parent_id UNION ALL
           SELECT p.id,p.parent_id FROM comment p JOIN chain ch ON p.id=ch.parent_id
           WHERE p.issue_id=i.id AND p.workspace_id=i.workspace_id AND p.author_type='agent'
             AND p.author_id=q.agent_id AND p.source_task_id=q.id)
           SELECT 1 FROM chain WHERE parent_id=rr.comment_id)
    UNION ALL
    SELECT 'task.completed',rr.strikeflow_command_id,q.id,NULL::uuid,NULL::uuid,NULL::text,NULL::text,q.completed_at
    FROM agent_task_queue q
    JOIN strikeflow_connector_reply_receipt rr ON rr.comment_id=q.trigger_comment_id AND rr.issue_id=q.issue_id
    JOIN strikeflow_connector_token t ON t.id=rr.token_id JOIN issue i ON i.id=q.issue_id
    JOIN comment member ON member.id=rr.comment_id AND member.issue_id=i.id AND member.workspace_id=i.workspace_id
      AND member.parent_id=rr.root_comment_id AND member.author_type='member' AND member.author_id=t.recipient_id
    JOIN comment root ON root.id=rr.root_comment_id AND root.issue_id=i.id AND root.workspace_id=i.workspace_id
    WHERE q.status='completed' AND q.completed_at IS NOT NULL AND i.workspace_id='$adopt_WORKSPACE_ID'::uuid
      AND i.project_id='$adopt_PROJECT_ID'::uuid AND t.workspace_id=i.workspace_id
      AND t.recipient_id='$adopt_RECIPIENT_ID'::uuid AND q.agent_id='$adopt_AGENT_ID'::uuid
      AND q.originator_user_id='$adopt_RECIPIENT_ID'::uuid AND q.accountable_user_id='$adopt_RECIPIENT_ID'::uuid
      AND q.originator_source='direct_human' AND q.trigger_evidence_kind='comment'
      AND q.trigger_evidence_ref_id=rr.comment_id AND rr.strikeflow_command_id IS NOT NULL
      AND i.id<>ALL(ARRAY['b41bcb97-8b63-43f6-9d6c-4ee9e9ada891'::uuid,
                              '39dcf540-bedf-4449-bc71-2e9e15fa0573'::uuid,
                              'b1839f3d-97e5-449a-9059-21b3b393d096'::uuid])
      AND rr.created_at >= '$adopt_NOT_BEFORE'::timestamptz AND rr.committed_at >= '$adopt_NOT_BEFORE'::timestamptz
      AND q.completed_at >= '$adopt_NOT_BEFORE'::timestamptz
      AND (SELECT count(*) FROM comment response WHERE response.source_task_id=q.id
           AND response.author_type='agent' AND response.author_id=q.agent_id) <= 100
  )
  SELECT count(*) INTO eligible_count FROM eligible;
  IF eligible_count <> 0 THEN
    RAISE EXCEPTION 'adoption recovery floor exposes eligible source rows';
  END IF;
  SELECT count(*) INTO newer_receipt_count
  FROM strikeflow_connector_reply_receipt rr
  JOIN strikeflow_connector_token t ON t.id=rr.token_id
  JOIN issue i ON i.id=rr.issue_id
  WHERE i.workspace_id='$adopt_WORKSPACE_ID'::uuid AND i.project_id='$adopt_PROJECT_ID'::uuid
    AND t.workspace_id=i.workspace_id AND t.recipient_id='$adopt_RECIPIENT_ID'::uuid
    AND (rr.created_at >= '$adopt_NOT_BEFORE'::timestamptz
      OR rr.committed_at >= '$adopt_NOT_BEFORE'::timestamptz);
  IF newer_receipt_count <> 0 THEN
    RAISE EXCEPTION 'adoption floor is not newer than every scoped receipt';
  END IF;
END \$\$;
SQL
}

systemd_unit_is_enabled() {
  unit=$1
  enabled_state=$(systemctl is-enabled "$unit" 2>/dev/null || true)
  case "$enabled_state" in
    enabled|enabled-runtime|linked|linked-runtime|alias|generated|transient)
      return 0
      ;;
    disabled|static|indirect|masked)
      return 1
      ;;
    *)
      return 0
      ;;
  esac
}

verify_response_reconciliation_stopped() {
  for unit in strikeflow-multica-content-dispatch.timer strikeflow-multica-content-dispatch.service \
              strikeflow-multica-content-ongoing.timer strikeflow-multica-content-ongoing.service; do
    if systemctl is-active --quiet "$unit"; then return 1; fi
    case "$unit" in *.timer) if systemd_unit_is_enabled "$unit"; then return 1; fi ;; esac
    test "$(systemctl show "$unit" --property=MainPID --value)" = 0 || return 1
  done
}

stop_response_reconciliation_fail_closed() {
  stop_attempt=0
  while [ "$stop_attempt" -lt 3 ]; do
    systemctl disable --now strikeflow-multica-content-dispatch.timer \
      strikeflow-multica-content-ongoing.timer >/dev/null 2>&1 || true
    systemctl stop strikeflow-multica-content-dispatch.service \
      strikeflow-multica-content-ongoing.service >/dev/null 2>&1 || true
    if verify_response_reconciliation_stopped; then return 0; fi
    stop_attempt=$((stop_attempt + 1))
  done
  return 1
}

# Hold the same global advisory lock acquired by the sealed connector receipt
# producer transaction. The psql session remains open until fd 8 is closed.
# External SQL writers that bypass the sealed handler do not honor this lock;
# they are unsupported and must be independently frozen before activation.
acquire_receipt_producer_freeze() {
  producer_freeze_dir=$(mktemp -d /run/multica-response-producer-freeze.XXXXXX)
  producer_freeze_fifo=$producer_freeze_dir/release
  producer_freeze_log=$producer_freeze_dir/psql.log
  mkfifo -m 0600 "$producer_freeze_fifo"
  (
    {
      printf '%s\n' "SELECT pg_advisory_lock(hashtextextended('multica.strikeflow.response.producer.freeze',0));"
      printf '%s\n' '\echo PRODUCER_FREEZE_ACQUIRED'
      cat "$producer_freeze_fifo"
      printf '%s\n' "SELECT pg_advisory_unlock(hashtextextended('multica.strikeflow.response.producer.freeze',0));"
      printf '%s\n' '\echo PRODUCER_FREEZE_RELEASED'
    } | docker exec -i multica-postgres-1 sh -c \
      'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
  ) >"$producer_freeze_log" 2>&1 &
  producer_freeze_pid=$!
  exec 8>"$producer_freeze_fifo"
  freeze_attempt=0
  while [ "$freeze_attempt" -lt 100 ]; do
    grep -Fqx PRODUCER_FREEZE_ACQUIRED "$producer_freeze_log" && return 0
    if ! kill -0 "$producer_freeze_pid" 2>/dev/null; then
      abort_receipt_producer_freeze
      return 1
    fi
    freeze_attempt=$((freeze_attempt + 1))
    sleep 0.1
  done
  abort_receipt_producer_freeze
  return 1
}

abort_receipt_producer_freeze() {
  exec 8>&- 2>/dev/null || true
  if [ -n "${producer_freeze_pid:-}" ]; then
    # Closing the FIFO lets the psql session execute its unlock and exit even
    # when acquisition was still waiting behind an in-flight producer. Do not
    # kill only the pipeline parent: that can orphan docker/psql and leak the
    # advisory lock across a fail-closed shell exit.
    wait "$producer_freeze_pid" 2>/dev/null || true
  fi
  rm -rf "${producer_freeze_dir:-/run/multica-response-producer-freeze.invalid}"
  producer_freeze_pid=
  producer_freeze_dir=
}

release_receipt_producer_freeze() {
  exec 8>&-
  wait "$producer_freeze_pid"
  grep -Fqx PRODUCER_FREEZE_RELEASED "$producer_freeze_log"
}

# adopt_* variables are the validated manifest values loaded above.
# shellcheck disable=SC2154
verify_adoption_outbox() {
  adoption_state=$1
  case "$adoption_state" in initial|delivered) ;; *) return 64 ;; esac
  event_one=${adopt_EVENT_IDS%,*}; event_two=${adopt_EVENT_IDS#*,}
  if [ "$adoption_state" = initial ]; then
    comment_delivered=$adopt_COMMENT_DELIVERED_AT
    completion_delivered=$adopt_COMPLETION_DELIVERED_AT
  else
    # Valid timestamps keep the SQL parseable; delivered mode does not compare
    # these placeholders because it requires only non-NULL acknowledgements.
    comment_delivered=$adopt_COMMENT_OCCURRED_AT
    completion_delivered=$adopt_COMPLETION_OCCURRED_AT
  fi
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<SQL
DO \$\$
DECLARE exact_count integer; unsafe_count integer;
BEGIN
  SELECT count(*) INTO unsafe_count FROM strikeflow_response_outbox
   WHERE (delivered_at IS NULL OR needs_attention_at IS NOT NULL)
     AND event_id NOT IN ('$event_one'::uuid,'$event_two'::uuid);
  IF unsafe_count <> 0 THEN RAISE EXCEPTION 'extra unsafe outbox rows exist'; END IF;
  SELECT count(*) INTO exact_count FROM strikeflow_response_outbox o
  JOIN strikeflow_connector_reply_receipt rr ON rr.strikeflow_command_id=o.strikeflow_command_id
    AND rr.token_id='$adopt_RECEIPT_TOKEN_ID'::uuid AND rr.idempotency_key='$adopt_RECEIPT_IDEMPOTENCY_KEY'::uuid
  WHERE o.event_id IN ('$event_one'::uuid,'$event_two'::uuid)
    AND o.strikeflow_command_id='$adopt_COMMAND_ID'::uuid AND o.workspace_key='$adopt_WORKSPACE_KEY'
    AND o.workspace_id='$adopt_WORKSPACE_ID'::uuid AND o.project_id='$adopt_PROJECT_ID'::uuid
    AND o.issue_id='$adopt_ISSUE_ID'::uuid AND o.issue_identifier='$adopt_ISSUE_IDENTIFIER'
    AND o.inbox_item_id='$adopt_INBOX_ITEM_ID'::uuid AND o.root_comment_id='$adopt_ROOT_COMMENT_ID'::uuid
    AND o.member_comment_id='$adopt_MEMBER_COMMENT_ID'::uuid AND o.continuation_task_id='$adopt_CONTINUATION_TASK_ID'::uuid
    AND o.recipient_id='$adopt_RECIPIENT_ID'::uuid AND o.agent_id='$adopt_AGENT_ID'::uuid
    AND rr.inbox_item_id=o.inbox_item_id AND rr.issue_id=o.issue_id AND rr.root_comment_id=o.root_comment_id
    AND rr.comment_id=o.member_comment_id AND rr.payload_hash='$adopt_RECEIPT_PAYLOAD_HASH'
    AND rr.created_at='$adopt_RECEIPT_CREATED_AT'::timestamptz AND rr.committed_at='$adopt_RECEIPT_COMMITTED_AT'::timestamptz
    AND o.needs_attention_at IS NULL
    AND ((o.event_id='$event_one'::uuid AND o.event_type='agent_comment.created'
          AND o.agent_comment_id='$adopt_AGENT_COMMENT_ID'::uuid
          AND o.agent_comment_parent_id IS NOT DISTINCT FROM NULLIF('$adopt_AGENT_COMMENT_PARENT_ID','null')::uuid
          AND encode(digest(o.agent_comment_content,'sha256'),'hex')='$adopt_COMMENT_CONTENT_SHA256'
          AND o.agent_comment_type='$adopt_COMMENT_TYPE' AND o.occurred_at='$adopt_COMMENT_OCCURRED_AT'::timestamptz)
      OR (o.event_id='$event_two'::uuid AND o.event_type='task.completed' AND o.agent_comment_id IS NULL
          AND o.agent_comment_parent_id IS NULL AND o.agent_comment_content IS NULL AND o.agent_comment_type IS NULL
          AND o.occurred_at='$adopt_COMPLETION_OCCURRED_AT'::timestamptz));
  IF exact_count <> 2 THEN RAISE EXCEPTION 'adoption full lineage mismatch'; END IF;
  IF '$adoption_state'='initial' THEN
    IF NOT EXISTS (SELECT 1 FROM strikeflow_response_outbox WHERE event_id='$event_one'::uuid
      AND attempt_count=$adopt_COMMENT_ATTEMPT_COUNT AND next_attempt_at='$adopt_COMMENT_NEXT_ATTEMPT_AT'::timestamptz
      AND lease_until IS NOT DISTINCT FROM NULLIF('$adopt_COMMENT_LEASE_UNTIL','null')::timestamptz
      AND delivered_at IS NOT DISTINCT FROM NULLIF('$comment_delivered','null')::timestamptz
      AND (('$adopt_COMMENT_LAST_ERROR'='null' AND last_error IS NULL)
        OR encode(digest(last_error,'sha256'),'hex')='$adopt_COMMENT_LAST_ERROR')) OR
       NOT EXISTS (SELECT 1 FROM strikeflow_response_outbox WHERE event_id='$event_two'::uuid
      AND attempt_count=$adopt_COMPLETION_ATTEMPT_COUNT AND next_attempt_at='$adopt_COMPLETION_NEXT_ATTEMPT_AT'::timestamptz
      AND lease_until IS NOT DISTINCT FROM NULLIF('$adopt_COMPLETION_LEASE_UNTIL','null')::timestamptz
      AND delivered_at IS NOT DISTINCT FROM NULLIF('$completion_delivered','null')::timestamptz
      AND (('$adopt_COMPLETION_LAST_ERROR'='null' AND last_error IS NULL)
        OR encode(digest(last_error,'sha256'),'hex')='$adopt_COMPLETION_LAST_ERROR')) THEN
      RAISE EXCEPTION 'adoption mutable initial state mismatch';
    END IF;
  ELSE
    IF (SELECT count(*) FROM strikeflow_response_outbox WHERE event_id IN ('$event_one'::uuid,'$event_two'::uuid)
        AND delivered_at IS NOT NULL AND needs_attention_at IS NULL AND lease_until IS NULL AND last_error IS NULL) <> 2 THEN
      RAISE EXCEPTION 'adoption pair did not finish delivery';
    END IF;
  END IF;
END \$\$;
SQL
}

adoption_identity_fingerprint() {
  docker exec -i multica-postgres-1 sh -c \
    'psql -X -A -t -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' <<'SQL'
SELECT count(*) || '|' || md5(COALESCE(string_agg(
  (to_jsonb(o)-'attempt_count'-'next_attempt_at'-'lease_until'-'delivered_at'-'needs_attention_at'-'last_error')::text,
  '|' ORDER BY o.event_id),'')) FROM strikeflow_response_outbox o;
SELECT count(*) || '|' || md5(COALESCE(string_agg(to_jsonb(r)::text,'|' ORDER BY r.token_id,r.idempotency_key),''))
FROM strikeflow_connector_reply_receipt r;
SQL
}

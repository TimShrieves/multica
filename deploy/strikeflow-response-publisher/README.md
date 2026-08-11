# Dormant local StrikeFlow response publisher

This package stages a VPS-local Multica backend candidate containing the
durable, signed StrikeFlow response publisher. Dormant installation does not
replace or restart the active Multica containers, apply migrations, provision
an HMAC secret, scan response rows, or send HTTP.

The publisher remains fail-closed until all exact `STRIKEFLOW_RESPONSE_*`
scope values, one explicit authorization mode, a dedicated secret file, and a
fresh rollout floor are configured. The Compose overlay is activation-only and
must not be added to the active project without a separate approved window.

`verify-disabled-install.sh` requires the sealed release directory, the built
candidate image tag, and the fresh preflight snapshot directory. It verifies
checksums, strict artifact provenance, the candidate image digest and labels,
disabled/blank configuration, absent credentials, no candidate container,
unchanged active container identities/digests/ports and Compose inputs, and
inactive/unenabled publisher units.

The activation-only Compose overlay requires both the immutable candidate
image digest and `STRIKEFLOW_RESPONSE_HMAC_HOST_FILE`, the non-secret host path
to the dedicated secret. It intentionally has no fallback image or `/dev/null`
secret mount. The tracked disabled environment includes this host-path key but
keeps it blank, so activation never depends on an ad-hoc shell export.

## Authorization modes

`STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE` is mandatory when enabled:

- `explicit_commands` requires 1–32 unique exact command UUIDs. Use it for a
  bounded canary or a deliberately narrow production window.
- `receipt_lineage` requires the command list to be empty. It authorizes only
  post-floor receipts whose non-null command binding and exact
  workspace/project/recipient/agent/comment/task ancestry pass the same SQL
  lineage joins. It is not a wildcard or a legacy-receipt fallback.

Both modes retain the exact project and principal scope, the immutable rollout
floor, and an exact historical exclusion ledger. The production verifier
requires that ledger to contain STR-94, STR-166, and STR-172, so none can be
recovered or backfilled even if an operator supplies a bad floor. A missing,
unknown, or ambiguous mode prevents backend startup.

The sealed production order is exact:
`STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS=b41bcb97-8b63-43f6-9d6c-4ee9e9ada891,39dcf540-bedf-4449-bc71-2e9e15fa0573,b1839f3d-97e5-449a-9059-21b3b393d096`.

## Activation gates (not authorized by dormant installation)

Before any backend restart, preserve a root-only preflight containing the
effective Compose files, active container image identities and ports, the
current migration ledger, and checksums of the backup copies themselves. Keep
the publisher disabled while migrations `253`–`259` are rehearsed twice
on a disposable clone of a fresh production dump. Migrations `254`–`257` and
`259` are single-statement concurrent index builds; `257`
binds the webhook event UUID as a database-unique delivery identity. A
forward-only `258` adds the purpose-bound Content reply credential and forensic
receipt schema, and `259` adds its natural idempotency identity. A
separate catalog check must prove the command column, outbox, immutable command
binding trigger, purpose-scope constraints, absence of response foreign keys,
exact seven-row migration ledger,
and all four ready/live/valid indexes before the publisher can be enabled.

The first rehearsal applies the migrations; the second must skip all seven.
Capture the receipt count before and after, prove existing command bindings
remain null, prove the outbox is empty, and run the publisher unit and connector
integration tests against the clone. Production application is a separate
approved one-shot candidate `migrate up`; never use backend startup as the SQL
approval gate. Take a fresh encrypted database backup and root-only host
preflight immediately before production application. Operational rollback never
invokes migration down; forward-only migrations `257`–`259` explicitly abort.

The sealed migration wrapper requires the fresh encrypted backup and checksum,
uses a one-off `./migrate` container with no dependencies or published ports,
and proves that every active container identity remained unchanged:

On the current main lineage, this production wrapper is deliberately narrower
than the clone rehearsal: it requires the complete release migration ledger
through `257`, proves that `258` and `259` are the only missing release migrations, and
compares the complete ledger again after the one-off migrator exits. Any other
pending or unexpected migration fails before the migrator is created.
This includes installations whose catalog is equivalent but whose migration
ledger uses the predecessor response lineage: the wrapper does not infer or
write ledger aliases. Such a host requires a separately reviewed, approval-
gated reconciliation that proves every catalog meaning before recording any
current-main alias under the migration lock.

```text
apply-production-migrations.sh RELEASE IMAGE_DIGEST ORIGINAL_PREFLIGHT \
  ENCRYPTED_BACKUP BACKUP_SHA256 \
  /var/backups/multica-response-publisher/gate-a-<UTC>-<source> \
  --confirm-migrate
```

After the catalog gate succeeds, deploy the candidate backend with the
publisher exactly false and every response scope/key/floor blank. The disabled
overlay uses direct `./server`, has no HMAC interpolation or mount, and is
reboot-persistent through the backend container's existing restart policy:

```text
deploy-candidate-disabled.sh RELEASE IMAGE_DIGEST ORIGINAL_PREFLIGHT \
  /var/backups/multica-response-publisher/candidate-disabled-<UTC>-<source> \
  --confirm-deploy-disabled
verify-candidate-disabled-install.sh RELEASE IMAGE_DIGEST ORIGINAL_PREFLIGHT
```

Before enablement, a fresh `STARTING_PREFLIGHT` must capture that exact running
disabled candidate. A pre-canary rollback restores the original base+pin
backend while preserving the additive schema and evidence:

```text
rollback-candidate-disabled.sh RELEASE ORIGINAL_PREFLIGHT \
  /var/backups/multica-response-publisher/rollback-candidate-disabled-<UTC>-<source> \
  --confirm-rollback-disabled
```

Activation must use the exact image digest recorded in `ARTIFACTS`, one exact
authorization mode, one workspace/project/recipient/agent scope, a fresh
not-before floor, and a dedicated root-owned `0600` HMAC secret file of at
least 32 bytes with no surrounding whitespace. The StrikeFlow receiver must
use the same key ID, secret, and floor and be enabled before the publisher. A
missing or mismatched value is a hard stop.

The candidate backend must first be deployed with the publisher false, before
the fresh reply is created, so the receipt records its command UUID. Do not
reuse a receipt created by the legacy backend because its command binding is
null and permanently ineligible.

Keep two distinct, root-only preflights for activation. `ORIGINAL_PREFLIGHT`
is captured before the candidate-disabled deployment and is authoritative only
for restoring the original base+pin backend. `STARTING_PREFLIGHT` is captured
after that disabled candidate is healthy and immediately before enablement; it
is authoritative for current runtime identity and activation verification.
Never use one directory for both roles.

Compose file and environment order is a safety boundary:

1. `/opt/multica/.env` (existing non-publisher runtime values), then the
   publisher environment;
2. `docker-compose.selfhost.yml`, then `docker-compose.pin.yml`, then the
   response overlay last.

Using only the publisher env can silently replace database/JWT inputs with
defaults; putting the pin file last can silently restore the old image. Always
inspect the rendered config without printing values.

After the seven migrations and receiver gate are separately approved, stage the
exact enabled config and secret, then run:

```text
verify-candidate-disabled-install.sh --allow-delivered-outbox \
  RELEASE IMAGE_DIGEST STARTING_PREFLIGHT
verify-enabled-install.sh --before-start RELEASE IMAGE_DIGEST STARTING_PREFLIGHT
activate.sh RELEASE IMAGE_DIGEST ORIGINAL_PREFLIGHT STARTING_PREFLIGHT \
  /var/backups/multica-response-publisher/activation-<UTC>-<source> \
  --confirm-activate
```

### Reconciled-pending adoption

The normal activation gate refuses every pending or attention outbox row. After
StrikeFlow's cross-ingest idempotency fix is live, an operator may instead adopt
one exact, already-reconciled response pair with the explicit adoption mode.
The root-owned mode-0600 manifest must live below
`/var/backups/multica-response-publisher/` and contain exactly:

```text
ADOPTION_CONTRACT_VERSION=2
ADOPTION_EVENT_IDS=<agent-comment-event-uuid>,<task-completed-event-uuid>
ADOPTION_COMMAND_ID=<command-uuid>
ADOPTION_WORKSPACE_KEY=<exact-workspace-key>
ADOPTION_WORKSPACE_ID=<workspace-uuid>
ADOPTION_PROJECT_ID=<project-uuid>
ADOPTION_ISSUE_ID=<issue-uuid>
ADOPTION_ISSUE_IDENTIFIER=<issue-identifier>
ADOPTION_INBOX_ITEM_ID=<inbox-item-uuid>
ADOPTION_ROOT_COMMENT_ID=<root-comment-uuid>
ADOPTION_MEMBER_COMMENT_ID=<member-reply-uuid>
ADOPTION_CONTINUATION_TASK_ID=<task-uuid>
ADOPTION_RECIPIENT_ID=<recipient-uuid>
ADOPTION_AGENT_ID=<agent-uuid>
ADOPTION_AGENT_COMMENT_ID=<agent-comment-uuid>
ADOPTION_AGENT_COMMENT_PARENT_ID=<parent-agent-comment-uuid-or-null>
ADOPTION_COMMENT_CONTENT_SHA256=<lowercase-sha256>
ADOPTION_COMMENT_TYPE=<comment-type>
ADOPTION_COMMENT_OCCURRED_AT=<RFC3339-with-timezone>
ADOPTION_COMPLETION_OCCURRED_AT=<RFC3339-with-timezone>
ADOPTION_NOT_BEFORE=<exact-configured-RFC3339-floor>
ADOPTION_INITIAL_STATE=<pending_pair-or-comment_delivered_completion_pending>
ADOPTION_COMMENT_ATTEMPT_COUNT=<0-11>
ADOPTION_COMPLETION_ATTEMPT_COUNT=<0-11>
ADOPTION_COMMENT_NEXT_ATTEMPT_AT=<RFC3339-with-timezone>
ADOPTION_COMPLETION_NEXT_ATTEMPT_AT=<RFC3339-with-timezone>
ADOPTION_COMMENT_LEASE_UNTIL=null
ADOPTION_COMPLETION_LEASE_UNTIL=<expired-RFC3339-or-null>
ADOPTION_COMMENT_DELIVERED_AT=<RFC3339-with-timezone-or-null>
ADOPTION_COMPLETION_DELIVERED_AT=null
ADOPTION_COMMENT_NEEDS_ATTENTION_AT=null
ADOPTION_COMPLETION_NEEDS_ATTENTION_AT=null
ADOPTION_COMMENT_LAST_ERROR=null
ADOPTION_COMPLETION_LAST_ERROR=<SHA256-of-transient-error-or-null>
ADOPTION_RECEIPT_TOKEN_ID=<token-uuid>
ADOPTION_RECEIPT_IDEMPOTENCY_KEY=<idempotency-uuid>
ADOPTION_RECEIPT_PAYLOAD_HASH=<lowercase-sha256>
ADOPTION_RECEIPT_CREATED_AT=<RFC3339-with-timezone>
ADOPTION_RECEIPT_COMMITTED_AT=<RFC3339-with-timezone>
ADOPTION_RECONCILED_AT=<RFC3339-with-timezone>
ADOPTION_STRIKEFLOW_DEPLOYMENT_ID=<live-deployment-id>
ADOPTION_STRIKEFLOW_SOURCE_COMMIT=<40-character-lowercase-commit>
```

The reconciliation evidence must be from the previous 24 hours. The verifier
requires the configured immutable `NOT_BEFORE` recovery floor to be newer than
the adopted receipt and response pair and every existing receipt in the exact
scope. The publisher-equivalent source query must expose zero historical rows,
so `RecoverOnce` cannot insert a third row during startup. Response
reconciliation timer/service must remain disabled, inactive and at `MainPID=0`
through the adoption transition. The manifest binds the full workspace, project,
issue, inbox, root, member, recipient, agent, task, comment, content, receipt,
attempt, lease, delivery and error state. It accepts either two pending rows or
the resumable ordered state where the comment is delivered and completion is
pending. That completion may retain an exactly fingerprinted transient error
and expired lease, but it must already be due; active leases, attention state,
completion-first, or dirty delivered comments fail closed. It
does not mark, delete, reset, or otherwise edit either ledger.

Activation holds the global `multica.strikeflow.response.producer.freeze`
PostgreSQL advisory lock from the first source-catalog proof through publisher
recovery, delivery and the post-start catalog proof. The sealed connector reply
handler takes the matching transaction lock before reserving a receipt. Both
ongoing-content and response-reconciliation service/timer pairs must be
inactive, their timers disabled, and every `MainPID` zero. External direct SQL
receipt writers do not honor the application lock; they are unsupported and
must be independently frozen before this gate.

```text
activate.sh RELEASE IMAGE_DIGEST ORIGINAL_PREFLIGHT STARTING_PREFLIGHT \
  /var/backups/multica-response-publisher/activation-adoption-<UTC>-<source> \
  /var/backups/multica-response-publisher/adoption-<UTC>.env \
  --confirm-activate-adopt-reconciled
```

The sealed release checksum closure must include `adoption-contract.sh` exactly
once. Activation requires `receipt_lineage` with an empty command list. The publisher
must receive normal authenticated `200` acknowledgments for both events within
60 seconds. The post-start verifier requires both rows delivered, zero unsafe
outbox rows, and a byte-stable receipt/outbox identity fingerprint that excludes
only normal delivery-state fields. Failure recreates the disabled candidate
without its HMAC mount and preserves the pending/attention evidence. Never use
this mode before the recorded StrikeFlow cross-mode fix is live.

The verifier checks the sealed release and image, secret metadata, exact scope
and authorization mode, rendered Compose semantics, production catalog, active
container identities, restart policy, mount, config-file ordering, and absence
of any `needs_attention` outbox row. The
activation script refuses to apply migrations and recreates only the backend.
Both the candidate-disabled and activation overlays use the direct `./server`
entrypoint, bypassing the image entrypoint that invokes migrations. SQL remains
a separate, explicitly approved gate; activation and safe-off never invoke it.
It writes root-only checksummed evidence and requires `/readyz` plus the strict
post-start verifier.

The publisher is part of the backend lifecycle, not a systemd timer. It wakes
on matching domain events, crash-recovers every 30 seconds, polls due outbox
work every second, uses a 10-second HTTP timeout and 30-second lease, and retries
up to 12 times with bounded exponential backoff. Docker `unless-stopped`
preserves it across reboot, but every future Compose recreate must keep the
same two env sources and three files in the exact order above.

## Evidence-preserving rollback

Rollback always recreates the candidate with the publisher false first; merely
editing an environment file does not change a running container. That first
safe-off recreate uses the sealed disabled overlay, direct `./server`, and no
HMAC mount. Verify the
activated candidate against `STARTING_PREFLIGHT`, then restore the exact
original backend image and disabled response environment exclusively from
`ORIGINAL_PREFLIGHT`, then
recreate only the backend container and verify its image digest, ports, health,
and active Compose inputs. Leave migrations `253`–`259`, outbox rows, and
audit evidence in place; their down files deliberately abort and must never be
used as an operational rollback. Do not delete receipts, outbox rows, secrets,
source archives, image archives, or the previous release. If the preflight
identity or checksum differs, stop instead of guessing.

```text
rollback-activated.sh RELEASE ORIGINAL_PREFLIGHT STARTING_PREFLIGHT \
  /var/backups/multica-response-publisher/rollback-<UTC>-<source> \
  --confirm-rollback
```

The script first verifies the currently running candidate image, exact
three-file Compose identity, enabled environment, secret mount, and catalog
before it mutates anything. It preserves the enabled and safe-off environments
in a root-only evidence directory, records both preflight paths, restores the
original two-file Compose project, requires the exact original-preflight backend
image and `/readyz`, and only then restores the
tracked blank disabled environment. Disable the StrikeFlow receiver after the
publisher is proven false. Do not remove the HMAC file or database evidence.

For a successful bounded canary that must retain the command-binding candidate
for the next rollout gate, use the dedicated safe-off command instead of the
full original-image rollback:

```text
safe-off-activated-to-candidate.sh RELEASE IMAGE_DIGEST ORIGINAL_PREFLIGHT \
  STARTING_PREFLIGHT \
  /var/backups/multica-response-publisher/safe-off-activated-<UTC>-<source> \
  --confirm-safe-off-to-candidate
```

This path accepts either an enabled `explicit_commands` configuration containing
exactly one command UUID or `receipt_lineage` with an empty command list. Before
publisher safe-off it disables and stops
`strikeflow-multica-content-dispatch.timer` and stops its service, proving both
inactive and the timer disabled; it never restarts continuous scheduling. Normal
mode refuses to proceed while any outbox row is pending or needs attention. It
snapshots the enabled configuration, response-reconciliation unit state, and database
fingerprint, recreates only the sealed candidate with the publisher false,
uses direct `./server` with no HMAC mount, and requires the receipt/outbox
fingerprint to remain byte-identical across the recreate. Only after the live
runtime is proven safe does it atomically restore the tracked blank environment.
It preserves the HMAC credential, delivered outbox rows, migrations, receipts,
and checksummed audit evidence. A failed safe-off retries the disabled candidate
and falls back to the exact original backend only when the candidate cannot be
proven safe; it never re-enables the publisher.

If the canary itself fails and queued or `needs_attention` outbox rows must be
preserved, use the same command with
`--confirm-emergency-safe-off-to-candidate`. That mode relaxes only the outbox
drain assertion: image, direct entrypoint, false environment, absent HMAC mount,
health, catalog, scope precondition, container identity, and byte-identical
database fingerprint checks remain mandatory. It records the emergency mode in
the checksummed evidence and never retries delivery or edits evidence rows.

## Exact replay canary

Replay only the delivered `agent_comment.created` event with the sealed helper;
never reconstruct JSON by hand or reset an outbox row:

```text
replay-delivered-comment.sh RELEASE IMAGE_DIGEST STARTING_PREFLIGHT \
  COMMAND_ID EVENT_ID PAYLOAD_SHA256 RECORDED_AT \
  /var/backups/multica-response-publisher/replay-comment-<UTC>-<source> \
  --confirm-replay
```

The helper loads the exact delivered row, requires the one-command canary
authorization, attempt count one, no attention state, and the immutable
StrikeFlow payload hash. It serializes through the publisher's own payload
type, signs fresh raw bytes inside the already-enabled container, and requires
an HTTP 200 acknowledgement with `replay=true`, the same event ID, `responding`
state, and the same `recorded_at`. The HMAC secret and signature never enter the
command line or evidence. Multica receipt and full outbox fingerprints must be
byte-identical before and after the replay, and the wrapper seals success or
failure evidence.

Dormant rollback removes or repoints only the candidate `current` symlink,
release/config directory, and unused local image after verifying that the
active Multica containers still match the preflight. It never runs Compose,
SQL, or systemd against the active Multica service.

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
the publisher disabled while migrations `900001`–`900005` are rehearsed twice
on a disposable clone of a fresh production dump. Migrations `900002`–`900005`
are single-statement concurrent index builds with `IF NOT EXISTS`; `900005`
binds the webhook event UUID as a database-unique delivery identity. A
separate catalog check must prove the command column, outbox, two immutable
identity triggers, absence of foreign keys, exact five-row migration ledger,
and all four ready/live/valid indexes before the publisher can be enabled.

The first rehearsal applies the migrations; the second must skip all five.
Capture the receipt count before and after, prove existing command bindings
remain null, prove the outbox is empty, and run the publisher unit and connector
integration tests against the clone. Production application is a separate
approved one-shot candidate `migrate up`; never use backend startup as the SQL
approval gate. Take a fresh encrypted database backup and root-only host
preflight immediately before production application. All five down migrations
intentionally abort.

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

Compose file and environment order is a safety boundary:

1. `/opt/multica/.env` (existing non-publisher runtime values), then the
   publisher environment;
2. `docker-compose.selfhost.yml`, then `docker-compose.pin.yml`, then the
   response overlay last.

Using only the publisher env can silently replace database/JWT inputs with
defaults; putting the pin file last can silently restore the old image. Always
inspect the rendered config without printing values.

After the five migrations and receiver gate are separately approved, stage the
exact enabled config and secret, then run:

```text
verify-enabled-install.sh --before-start RELEASE IMAGE_DIGEST PREFLIGHT
activate.sh RELEASE IMAGE_DIGEST PREFLIGHT \
  /var/backups/multica-response-publisher/activation-<UTC>-<source> \
  --confirm-activate
```

The verifier checks the sealed release and image, secret metadata, exact scope
and authorization mode, rendered Compose semantics, production catalog, active
container identities, restart policy, mount, config-file ordering, and absence
of any `needs_attention` outbox row. The
activation script refuses to apply migrations and recreates only the backend.
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
editing an environment file does not change a running container. Restore the
exact pre-activation backend image and disabled response environment from the
named preflight, then
recreate only the backend container and verify its image digest, ports, health,
and active Compose inputs. Leave migrations `900001`–`900005`, outbox rows, and
audit evidence in place; their down files deliberately abort and must never be
used as an operational rollback. Do not delete receipts, outbox rows, secrets,
source archives, image archives, or the previous release. If the preflight
identity or checksum differs, stop instead of guessing.

```text
rollback-activated.sh RELEASE PREFLIGHT \
  /var/backups/multica-response-publisher/rollback-<UTC>-<source> \
  --confirm-rollback
```

The script first verifies the currently running candidate image, exact
three-file Compose identity, enabled environment, secret mount, and catalog
before it mutates anything. It preserves the enabled and safe-off environments
in a root-only evidence directory, restores the original two-file Compose project, requires
the exact preflight backend image and `/readyz`, and only then restores the
tracked blank disabled environment. Disable the StrikeFlow receiver after the
publisher is proven false. Do not remove the HMAC file or database evidence.

Dormant rollback removes or repoints only the candidate `current` symlink,
release/config directory, and unused local image after verifying that the
active Multica containers still match the preflight. It never runs Compose,
SQL, or systemd against the active Multica service.

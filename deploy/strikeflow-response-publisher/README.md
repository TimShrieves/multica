# Dormant local StrikeFlow response publisher

This package stages a VPS-local Multica backend candidate containing the
durable, signed StrikeFlow response publisher. Dormant installation does not
replace or restart the active Multica containers, apply migrations, provision
an HMAC secret, scan response rows, or send HTTP.

The publisher remains fail-closed until all exact `STRIKEFLOW_RESPONSE_*`
scope values, a dedicated secret file, a fresh rollout floor, and at least one
command UUID are configured. The Compose overlay is activation-only and must
not be added to the active project without a separate approved window.

`verify-disabled-install.sh` requires the sealed release directory, the built
candidate image tag, and the fresh preflight snapshot directory. It verifies
checksums, strict artifact provenance, the candidate image digest and labels,
disabled/blank configuration, absent credentials, no candidate container,
unchanged active container identities/digests/ports and Compose inputs, and
inactive/unenabled publisher units.

The activation-only Compose overlay requires both the immutable candidate
image digest and the dedicated host secret path. It intentionally has no
fallback image or `/dev/null` secret mount.

## Activation gates (not authorized by dormant installation)

Before any backend restart, preserve a root-only preflight containing the
effective Compose files, active container image identities and ports, the
current migration ledger, and checksums of the backup copies themselves. Keep
the publisher disabled while migrations `900001`–`900004` are rehearsed on a
disposable clone. The three index migrations are single-statement concurrent
builds with `IF NOT EXISTS`; a separate catalog check must prove their exact
definitions and validity before the publisher can be enabled.

Activation must use the exact image digest recorded in `ARTIFACTS`, an exact
command allowlist, one workspace/project/recipient/agent scope, a fresh
not-before floor, and a dedicated root-owned `0600` HMAC secret file. A missing
or mismatched value is a hard stop. Do not add the overlay to the active Compose
project until all preflight evidence has been read back.

## Evidence-preserving rollback

Rollback always disables the publisher first. Restore the exact pre-activation
backend image and disabled response environment from the named preflight, then
recreate only the backend container and verify its image digest, ports, health,
and active Compose inputs. Leave migrations `900001`–`900004`, outbox rows, and
audit evidence in place; their down files deliberately abort and must never be
used as an operational rollback. Do not delete receipts, outbox rows, secrets,
source archives, image archives, or the previous release. If the preflight
identity or checksum differs, stop instead of guessing.

Dormant rollback removes or repoints only the candidate `current` symlink,
release/config directory, and unused local image after verifying that the
active Multica containers still match the preflight. It never runs Compose,
SQL, or systemd against the active Multica service.

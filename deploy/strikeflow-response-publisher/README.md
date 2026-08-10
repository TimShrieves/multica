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

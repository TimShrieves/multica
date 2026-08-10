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
checksums, disabled/blank configuration, absent credentials, no candidate
container, unchanged active container identities/digests/ports, unchanged
active Compose files, and an inactive/unenabled publisher service.

package strikeflowresponse

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func responsePublisherRepoRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve deployment contract test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..", ".."))
}

func TestDormantDeploymentContractCarriesActivationOnlyHostPath(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	disabled, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "publisher.env.disabled"))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(disabled)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key == "" {
			t.Fatalf("invalid disabled environment line %q", line)
		}
		if _, exists := values[key]; exists {
			t.Fatalf("duplicate disabled environment key %q", key)
		}
		values[key] = value
	}
	if values["STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED"] != "false" {
		t.Fatal("dormant publisher is not exactly false")
	}
	for _, key := range []string{"STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE", "STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS", "STRIKEFLOW_RESPONSE_HMAC_HOST_FILE"} {
		if value, exists := values[key]; !exists || value != "" {
			t.Fatalf("%s must exist and remain blank while dormant", key)
		}
	}

	overlay, err := os.ReadFile(filepath.Join(root, "docker-compose.strikeflow-response-publisher.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(overlay)
	for _, required := range []string{
		"STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE",
		"STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS",
		"${STRIKEFLOW_RESPONSE_HMAC_HOST_FILE:?set the dedicated HMAC secret file}",
		"/run/secrets/strikeflow_response_hmac:ro",
		`entrypoint: ["./server"]`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("activation overlay missing %q", required)
		}
	}
}

func TestActivationScriptCannotBecomeTheMigrationApprovalGate(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	activate, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "activate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(activate)
	if strings.Contains(text, "migrate up") || strings.Contains(text, "migrate down") {
		t.Fatal("activation script must not apply or reverse migrations")
	}
	for _, required := range []string{
		"original_preflight",
		"flock -n",
		"starting_preflight",
		`test "$original_preflight" != "$starting_preflight"`,
		"verify-candidate-disabled-install.sh",
		`--before-start "$release_dir" "$image_digest" "$starting_preflight"`,
		"original-preflight.txt",
		"starting-preflight.txt",
		"assert_activation_overlay",
		`backend.get("entrypoint") != ["./server"]`,
		"disabled_overlay",
		"--allow-delivered-outbox",
		"fail_closed",
		"publisher.env.safe-off",
		"activation_verified=false",
		"fail-closed-fallback.log",
		"fail-closed-install-disabled-config.log",
		"install_disabled_config",
		"SHA256SUMS",
		"fallback_status=not_attempted",
		"assert_original_backend",
		"restored_image_ref",
		"/run/secrets/strikeflow_response_hmac",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("activation script is missing fail-closed contract %q", required)
		}
	}
	base := strings.Index(text, `-f "$base_compose"`)
	if base < 0 {
		t.Fatal("activation script does not include base compose")
	}
	pin := strings.Index(text[base:], `-f "$pin_compose"`)
	if pin < 0 {
		t.Fatal("activation script does not put pin compose after base")
	}
	overlay := strings.Index(text[base+pin:], `-f "$overlay"`)
	if overlay < 0 {
		t.Fatal("activation script does not preserve base, pin, overlay ordering")
	}
}

func TestCandidateVerifierCanPreserveFailedOutboxWithoutWeakeningRuntimeChecks(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	verifier, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "verify-candidate-disabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(verifier)
	for _, required := range []string{
		"--preserve-outbox",
		"outbox_policy=preserve",
		`if [ "$outbox_policy" = empty ]`,
		`elif [ "$outbox_policy" = delivered ]`,
		"disabled candidate must bypass the migration entrypoint",
		"disabled candidate must not mount the response HMAC secret",
		"rendered candidate publisher is not exactly false",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("preservation verifier is missing contract %q", required)
		}
	}
}

func TestReplayUtilityUsesSealedBinaryAndNeverMutatesOutbox(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "replay-delivered-comment.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, forbidden := range []string{"migrate", "UPDATE ", "DELETE ", "TRUNCATE ", "DROP "} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("replay wrapper contains forbidden operation %q", forbidden)
		}
	}
	for _, required := range []string{
		"verify-enabled-install.sh",
		"--rollback-preflight",
		"./strikeflow-response-replay",
		"--command-id",
		"--event-id",
		"--payload-sha256",
		"--recorded-at",
		"database.before",
		"database.after",
		`cmp -s "$evidence_dir/database.before" "$evidence_dir/database.after"`,
		"replay-result.json",
		"SHA256SUMS",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("replay wrapper is missing contract %q", required)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"-o bin/strikeflow-response-replay ./cmd/strikeflow-response-replay",
		"COPY --from=builder /src/server/bin/strikeflow-response-replay .",
	} {
		if !strings.Contains(string(dockerfile), required) {
			t.Fatalf("candidate image omits replay binary contract %q", required)
		}
	}
}

func TestRollbackVerifiesActivatedIdentityBeforeMutation(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	rollback, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "rollback-activated.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(rollback)
	verify := strings.Index(text, "verify-enabled-install.sh")
	recreateAfterVerify := -1
	if verify >= 0 {
		recreateAfterVerify = strings.Index(text[verify:], "up -d --no-deps --force-recreate backend")
	}
	if verify < 0 || recreateAfterVerify < 0 {
		t.Fatal("rollback must verify the activated identity before any backend recreate")
	}
	for _, required := range []string{
		"original_preflight",
		"flock -n",
		"starting_preflight",
		`test "$original_preflight" != "$starting_preflight"`,
		`--rollback-preflight "$release_dir" "$image_digest" "$starting_preflight"`,
		`expected_image=$(cut -d'|' -f2 "$original_preflight/multica-backend-1.identity")`,
		`sha256sum -c "$original_preflight/active-compose.sha256"`,
		"original-preflight.txt",
		"starting-preflight.txt",
		"assert_activation_overlay",
		`backend.get("entrypoint") != ["./server"]`,
		"disabled_overlay",
		"--preserve-outbox",
		"restored_image_ref",
		"assert_original_backend",
		"restore_original_backend",
		"failure-restore-original.log",
		"failure-restore-disabled.log",
		"failure-install-disabled-config.log",
		"install_disabled_config",
		"SHA256SUMS",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("rollback is missing original-backend safety contract %q", required)
		}
	}
}

func TestSuccessfulCanarySafeOffStopsAtDisabledCandidate(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "safe-off-activated-to-candidate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, forbidden := range []string{"migrate up", "migrate down", "DELETE FROM", "TRUNCATE ", "DROP TABLE"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("candidate safe-off contains forbidden operation %q", forbidden)
		}
	}
	for _, required := range []string{
		"verify-enabled-install.sh",
		"--rollback-preflight",
		"explicit_commands",
		"len(commands) != 1",
		"WHERE delivered_at IS NULL OR needs_attention_at IS NOT NULL",
		"database.before",
		"database.after",
		`cmp -s "$evidence_dir/database.before" "$evidence_dir/database.after"`,
		"verify-candidate-disabled-install.sh",
		"--allow-delivered-outbox",
		"--preserve-outbox",
		"--confirm-emergency-safe-off-to-candidate",
		"safe-off-mode.txt",
		`--env-file "$base_env" --env-file "$disabled_env"`,
		`-f "$base_compose" -f "$pin_compose" -f "$disabled_overlay"`,
		"install_disabled_config",
		"failure-restore-disabled.log",
		"failure-restore-original.log",
		"database.failure-final",
		"SHA256SUMS",
		"trap '' HUP INT TERM",
		"safe_off_verified=true",
		"multica_response_publisher_safe_off_candidate",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("candidate safe-off is missing contract %q", required)
		}
	}
	if strings.Count(text, "restore_original_backend") < 2 {
		t.Fatal("candidate safe-off must retain original backend as failure-only fallback")
	}
	successStart := strings.Index(text, `backend_changed=true`)
	successEnd := strings.Index(text, `safe_off_verified=true`)
	if successStart < 0 || successEnd < successStart {
		t.Fatal("candidate safe-off success path is not identifiable")
	}
	if strings.Contains(text[successStart:successEnd], "restore_original_backend") {
		t.Fatal("successful candidate safe-off must not restore the original backend")
	}
}

func TestEnabledVerifierDistinguishesRepositoryDigestFromImageID(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	verifier, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "verify-enabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(verifier)
	if strings.Contains(text, `${image_digest#*@}`) {
		t.Fatal("enabled verifier incorrectly treats a repository digest as the image config ID")
	}
	for _, required := range []string{"image_id=$(sed", ".RepoDigests", `grep -Fqx "$image_digest"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("enabled verifier is missing sealed image identity check %q", required)
		}
	}
}

func TestEnabledVerifierChecksExactImmutabilitySemantics(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	verifier, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "verify-enabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(verifier)
	for _, required := range []string{
		"len(raw) > 4096",
		`backend.get("entrypoint") != ["./server"]`,
		`Config.Entrypoint`,
		"regexp_replace(pg_get_constraintdef",
		"column_default='gen_random_uuid()'",
		"column_default='0'",
		"column_default='now()'",
		"='strikeflow_command_idISNOTNULL'",
		"='delivered_atISNULL'",
		"='COALESCEagent_comment_id,''00000000-0000-0000-0000-000000000000''::uuid'",
		"tgrelid='public.strikeflow_connector_reply_receipt'::regclass",
		"tgrelid='public.strikeflow_response_outbox'::regclass",
		"indrelid='public.strikeflow_connector_reply_receipt'::regclass",
		"indrelid='public.strikeflow_response_outbox'::regclass",
		"tgrelid IN ('public.strikeflow_connector_reply_receipt'::regclass,'public.strikeflow_response_outbox'::regclass)",
		"tgtype=19",
		"p.prosrc",
		"strikeflow command binding is immutable",
		"strikeflow response outbox identity is immutable",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("enabled verifier is missing exact immutability gate %q", required)
		}
	}
}

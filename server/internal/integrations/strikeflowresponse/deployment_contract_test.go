package strikeflowresponse

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
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

func TestMainlineMigrationGateIsBoundedAndProducerFrozen(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	deploy := filepath.Join(root, "deploy", "strikeflow-response-publisher")
	verifier, err := os.ReadFile(filepath.Join(deploy, "verify-candidate-disabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	verifierText := string(verifier)
	for _, required := range []string{
		"while [ \"$#\" -gt 0 ]", "--before-start", "--migration-preflight", "--allow-delivered-outbox",
		"outbox_policy=delivered",
	} {
		if !strings.Contains(verifierText, required) {
			t.Fatalf("candidate verifier must support combined pre-start/delivered mode %q", required)
		}
	}
	gate, err := os.ReadFile(filepath.Join(deploy, "apply-mainline-migrations.sh"))
	if err != nil {
		t.Fatal(err)
	}
	gateText := string(gate)
	for _, required := range []string{
		"224_agent_task_session_rollout_missing",
		"252_strikeflow_connector_principal",
		"258_strikeflow_content_reply_connector",
		"259_strikeflow_content_reply_receipt_unique",
		"260_strikeflow_response_outbox_identity_immutable",
		"900001_strikeflow_response_outbox",
		"migration-ledger.before.normalized",
		"migration-ledger.after.normalized",
		"MULTICA_MIGRATION_ALLOWLIST",
		"pg_advisory_lock(hashtextextended('multica.strikeflow.response.producer.freeze'",
		"--migration-preflight --allow-delivered-outbox",
		"--before-start --allow-delivered-outbox",
		"strikeflow-multica-content-ongoing.service",
		"strikeflow-multica-content-dispatch.timer",
		"outbox_identity|",
		"cmp \"$evidence_dir/database.before\" \"$evidence_dir/database.after\"",
	} {
		if !strings.Contains(gateText, required) {
			t.Fatalf("mainline migration gate is missing bounded-scope contract %q", required)
		}
	}
	for _, forbidden := range []string{"migrate down", "DROP DATABASE", "TRUNCATE strikeflow_", "DELETE FROM strikeflow_"} {
		if strings.Contains(gateText, forbidden) {
			t.Fatalf("mainline migration gate contains forbidden operation %q", forbidden)
		}
	}
	legacy, err := os.ReadFile(filepath.Join(deploy, "apply-production-migrations.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(legacy), "apply-mainline-migrations.sh") {
		t.Fatal("historical migration entrypoint must delegate to the bounded mainline gate")
	}
}

func TestReconciledPendingAdoptionIsExactAuditedAndDeliveryOnly(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	deploy := filepath.Join(root, "deploy", "strikeflow-response-publisher")
	contract, err := os.ReadFile(filepath.Join(deploy, "adoption-contract.sh"))
	if err != nil {
		t.Fatal(err)
	}
	contractText := string(contract)
	for _, forbidden := range []string{"UPDATE ", "DELETE ", "TRUNCATE ", "DROP ", "migrate"} {
		if strings.Contains(contractText, forbidden) {
			t.Fatalf("adoption contract contains forbidden mutation %q", forbidden)
		}
	}
	for _, required := range []string{
		"ADOPTION_CONTRACT_VERSION", "ADOPTION_EVENT_IDS", "ADOPTION_COMMAND_ID",
		"ADOPTION_WORKSPACE_ID", "ADOPTION_PROJECT_ID", "ADOPTION_ISSUE_ID",
		"ADOPTION_INBOX_ITEM_ID", "ADOPTION_ROOT_COMMENT_ID", "ADOPTION_MEMBER_COMMENT_ID",
		"ADOPTION_CONTINUATION_TASK_ID", "ADOPTION_RECIPIENT_ID", "ADOPTION_AGENT_ID",
		"ADOPTION_AGENT_COMMENT_ID", "ADOPTION_COMMENT_CONTENT_SHA256", "ADOPTION_NOT_BEFORE",
		"ADOPTION_RECEIPT_TOKEN_ID", "ADOPTION_RECEIPT_IDEMPOTENCY_KEY", "ADOPTION_RECEIPT_PAYLOAD_HASH",
		"ADOPTION_INITIAL_STATE", "comment_delivered_completion_pending", "ADOPTION_RECONCILED_AT",
		"ADOPTION_STRIKEFLOW_DEPLOYMENT_ID", "ADOPTION_STRIKEFLOW_SOURCE_COMMIT",
		"exactly two distinct response event ids", "extra unsafe outbox rows exist",
		"adoption recovery floor exposes eligible source rows", "adoption floor is not newer than every scoped receipt", "attempt_count",
		"strikeflow_connector_reply_receipt", "adoption_identity_fingerprint",
		"-'attempt_count'-'next_attempt_at'-'lease_until'-'delivered_at'-'needs_attention_at'-'last_error'",
	} {
		if !strings.Contains(contractText, required) {
			t.Fatalf("adoption contract is missing %q", required)
		}
	}

	activate, err := os.ReadFile(filepath.Join(deploy, "activate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	activateText := string(activate)
	for _, required := range []string{
		"--confirm-activate-adopt-reconciled", "--allow-reconciled-pending-outbox",
		"--adoption-before-start", "--adoption-after-start", "adoption-manifest.env",
		"adoption-manifest.env.sha256", "verify_adoption_source_catalog", "verify_response_reconciliation_stopped",
		"stop_response_reconciliation_fail_closed", "fail-closed-stop-reconciliation.log", "reconciliation_stop_status",
		"adoption-identity.before", "adoption-identity.after",
		`cmp -s "$evidence_dir/adoption-identity.before" "$evidence_dir/adoption-identity.after"`,
		"fail_closed_outbox_mode=--preserve-outbox", `test "$adoption_attempt" -lt 30`,
	} {
		if !strings.Contains(activateText, required) {
			t.Fatalf("adoption activation is missing %q", required)
		}
	}
	for _, forbidden := range []string{"UPDATE strikeflow_", "DELETE FROM", "TRUNCATE ", "DROP TABLE", "migrate up", "migrate down"} {
		if strings.Contains(activateText, forbidden) {
			t.Fatalf("adoption activation contains forbidden mutation %q", forbidden)
		}
	}
}

func TestAdoptionVerifierModesRemainFailClosed(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	deploy := filepath.Join(root, "deploy", "strikeflow-response-publisher")
	candidate, err := os.ReadFile(filepath.Join(deploy, "verify-candidate-disabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--allow-reconciled-pending-outbox", "outbox_policy=adoption",
		"validate_adoption_manifest", "verify_adoption_config", "verify_response_reconciliation_stopped", "verify_adoption_source_catalog", "verify_adoption_outbox initial",
		"disabled candidate must not mount the response HMAC secret",
	} {
		if !strings.Contains(string(candidate), required) {
			t.Fatalf("candidate adoption verifier is missing %q", required)
		}
	}
	enabled, err := os.ReadFile(filepath.Join(deploy, "verify-enabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--adoption-before-start", "--adoption-after-start",
		"validate_adoption_manifest", "verify_adoption_config", "verify_response_reconciliation_stopped", "verify_adoption_source_catalog", "verify_adoption_outbox initial",
		"verify_adoption_outbox delivered", "receipt_lineage",
		"STRIKEFLOW_RESPONSE_COMMAND_IDS",
	} {
		if !strings.Contains(string(enabled), required) {
			t.Fatalf("enabled adoption verifier is missing %q", required)
		}
	}
}

func TestAdoptionRejectsMalformedLineagePartialStateAndRecoveryDrift(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	contract, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "adoption-contract.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(contract)
	for _, required := range []string{
		`if set(values) != expected`, `ADOPTION_CONTRACT_VERSION"] != "2"`,
		`partial completion lease is still active`, `adoption cannot include attention rows`,
		`last-error evidence must be null or a SHA256 digest`, `pending_pair must be clean and unleased`,
		`partial delivery state is not resumable in order`, `completion_delivered`,
		`partial completion retry is not due`, `delivered comment must not retain lease or error state`,
		`eligible_count <> 0`, `newer_receipt_count <> 0`, `extra unsafe outbox rows exist`,
		`adoption mutable initial state mismatch`, `adoption pair did not finish delivery`,
		`rr.inbox_item_id=o.inbox_item_id`, `rr.issue_id=o.issue_id`, `rr.root_comment_id=o.root_comment_id`,
		`rr.comment_id=o.member_comment_id`, `ADOPTION_RECEIPT_COMMITTED_AT`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("adoption malformed/recovery contract is missing %q", required)
		}
	}
}

func TestAdoptionFreezesEverySealedReceiptProducerAcrossRecovery(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	contract, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "adoption-contract.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(contract)
	for _, required := range []string{
		"multica.strikeflow.response.producer.freeze", "pg_advisory_lock", "pg_advisory_unlock",
		"PRODUCER_FREEZE_ACQUIRED", "PRODUCER_FREEZE_RELEASED", "acquire_receipt_producer_freeze", "abort_receipt_producer_freeze",
		"strikeflow-multica-content-dispatch.timer", "strikeflow-multica-content-dispatch.service",
		"strikeflow-multica-content-ongoing.timer", "strikeflow-multica-content-ongoing.service",
		`systemctl is-active --quiet`, `--property=MainPID --value`,
		"External SQL writers that bypass the sealed handler",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("producer freeze/quiescence contract is missing %q", required)
		}
	}
	handler, err := os.ReadFile(filepath.Join(root, "server", "internal", "handler", "strikeflow_connector.go"))
	if err != nil {
		t.Fatal(err)
	}
	handlerText := string(handler)
	lock := strings.Index(handlerText, "util.LockResponseProducer")
	insert := strings.Index(handlerText, "INSERT INTO strikeflow_connector_reply_receipt")
	if lock < 0 || insert < 0 || lock > insert {
		t.Fatal("sealed receipt producer must acquire the adoption advisory lock before receipt insertion")
	}
	utilLock, err := os.ReadFile(filepath.Join(root, "server", "internal", "util", "response_producer_lock.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(utilLock), "pg_advisory_xact_lock") ||
		!strings.Contains(string(utilLock), "multica.strikeflow.response.producer.freeze") {
		t.Fatal("shared response producer lock must bind the transaction helper to the sealed advisory key")
	}
	activate, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "activate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	activationText := string(activate)
	acquire := strings.Index(activationText, "acquire_receipt_producer_freeze")
	recoverCheck := strings.Index(activationText, "verify_adoption_source_catalog")
	release := strings.LastIndex(activationText, "release_receipt_producer_freeze")
	if acquire < 0 || recoverCheck < acquire || release < recoverCheck {
		t.Fatal("producer freeze must span source catalog verification and publisher recovery")
	}
}

func TestAdoptionManifestResumeStateMatrix(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	contract, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "adoption-contract.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(contract)
	marker := "python3 - \"$adoption_manifest\" <<'PY'\n"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatal("manifest validator start marker absent")
	}
	start += len(marker)
	end := strings.Index(text[start:], "\nPY\n")
	if end < 0 {
		t.Fatal("manifest validator end marker absent")
	}
	validator := filepath.Join(t.TempDir(), "validate.py")
	if err := os.WriteFile(validator, []byte(text[start:start+end]), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	rfc := func(offset time.Duration) string { return now.Add(offset).Format(time.RFC3339) }
	base := map[string]string{
		"ADOPTION_CONTRACT_VERSION": "2", "ADOPTION_EVENT_IDS": "10000000-0000-4000-8000-000000000001,10000000-0000-4000-8000-000000000002",
		"ADOPTION_COMMAND_ID": "20000000-0000-4000-8000-000000000001", "ADOPTION_WORKSPACE_KEY": "strike",
		"ADOPTION_WORKSPACE_ID": "30000000-0000-4000-8000-000000000001", "ADOPTION_PROJECT_ID": "40000000-0000-4000-8000-000000000001",
		"ADOPTION_ISSUE_ID": "50000000-0000-4000-8000-000000000001", "ADOPTION_ISSUE_IDENTIFIER": "STR-207",
		"ADOPTION_INBOX_ITEM_ID": "60000000-0000-4000-8000-000000000001", "ADOPTION_ROOT_COMMENT_ID": "70000000-0000-4000-8000-000000000001",
		"ADOPTION_MEMBER_COMMENT_ID": "80000000-0000-4000-8000-000000000001", "ADOPTION_CONTINUATION_TASK_ID": "90000000-0000-4000-8000-000000000001",
		"ADOPTION_RECIPIENT_ID": "a0000000-0000-4000-8000-000000000001", "ADOPTION_AGENT_ID": "b0000000-0000-4000-8000-000000000001",
		"ADOPTION_AGENT_COMMENT_ID": "c0000000-0000-4000-8000-000000000001", "ADOPTION_AGENT_COMMENT_PARENT_ID": "80000000-0000-4000-8000-000000000001",
		"ADOPTION_COMMENT_CONTENT_SHA256": strings.Repeat("a", 64), "ADOPTION_COMMENT_TYPE": "text",
		"ADOPTION_COMMENT_OCCURRED_AT": rfc(-3 * time.Hour), "ADOPTION_COMPLETION_OCCURRED_AT": rfc(-2 * time.Hour),
		"ADOPTION_NOT_BEFORE": rfc(-time.Hour), "ADOPTION_INITIAL_STATE": "pending_pair",
		"ADOPTION_COMMENT_ATTEMPT_COUNT": "0", "ADOPTION_COMPLETION_ATTEMPT_COUNT": "0",
		"ADOPTION_COMMENT_NEXT_ATTEMPT_AT": rfc(-2 * time.Hour), "ADOPTION_COMPLETION_NEXT_ATTEMPT_AT": rfc(-2 * time.Hour),
		"ADOPTION_COMMENT_LEASE_UNTIL": "null", "ADOPTION_COMPLETION_LEASE_UNTIL": "null",
		"ADOPTION_COMMENT_DELIVERED_AT": "null", "ADOPTION_COMPLETION_DELIVERED_AT": "null",
		"ADOPTION_COMMENT_NEEDS_ATTENTION_AT": "null", "ADOPTION_COMPLETION_NEEDS_ATTENTION_AT": "null",
		"ADOPTION_COMMENT_LAST_ERROR": "null", "ADOPTION_COMPLETION_LAST_ERROR": "null",
		"ADOPTION_RECEIPT_TOKEN_ID": "d0000000-0000-4000-8000-000000000001", "ADOPTION_RECEIPT_IDEMPOTENCY_KEY": "e0000000-0000-4000-8000-000000000001",
		"ADOPTION_RECEIPT_PAYLOAD_HASH": strings.Repeat("b", 64), "ADOPTION_RECEIPT_CREATED_AT": rfc(-4 * time.Hour),
		"ADOPTION_RECEIPT_COMMITTED_AT": rfc(-3 * time.Hour), "ADOPTION_RECONCILED_AT": rfc(-30 * time.Minute),
		"ADOPTION_STRIKEFLOW_DEPLOYMENT_ID": "dep-abcdefghijklmnop", "ADOPTION_STRIKEFLOW_SOURCE_COMMIT": strings.Repeat("c", 40),
	}
	run := func(name string, mutate func(map[string]string), wantOK bool) {
		t.Helper()
		values := make(map[string]string, len(base))
		for key, value := range base {
			values[key] = value
		}
		mutate(values)
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		var manifest strings.Builder
		for _, key := range keys {
			manifest.WriteString(key + "=" + values[key] + "\n")
		}
		path := filepath.Join(t.TempDir(), name+".env")
		if err := os.WriteFile(path, []byte(manifest.String()), 0o600); err != nil {
			t.Fatal(err)
		}
		err := exec.Command("python3", validator, path).Run()
		if (err == nil) != wantOK {
			t.Fatalf("%s validator result=%v wantOK=%v", name, err, wantOK)
		}
	}
	run("pending-clean", func(map[string]string) {}, true)
	run("partial-expired-lease-error", func(v map[string]string) {
		v["ADOPTION_INITIAL_STATE"] = "comment_delivered_completion_pending"
		v["ADOPTION_COMMENT_DELIVERED_AT"] = rfc(-90 * time.Minute)
		v["ADOPTION_COMPLETION_LEASE_UNTIL"] = rfc(-time.Minute)
		v["ADOPTION_COMPLETION_LAST_ERROR"] = strings.Repeat("d", 64)
	}, true)
	run("partial-active-lease", func(v map[string]string) {
		v["ADOPTION_INITIAL_STATE"] = "comment_delivered_completion_pending"
		v["ADOPTION_COMMENT_DELIVERED_AT"] = rfc(-90 * time.Minute)
		v["ADOPTION_COMPLETION_LEASE_UNTIL"] = rfc(time.Hour)
	}, false)
	run("attention", func(v map[string]string) { v["ADOPTION_COMPLETION_NEEDS_ATTENTION_AT"] = rfc(-time.Minute) }, false)
	run("future-retry", func(v map[string]string) {
		v["ADOPTION_INITIAL_STATE"] = "comment_delivered_completion_pending"
		v["ADOPTION_COMMENT_DELIVERED_AT"] = rfc(-90 * time.Minute)
		v["ADOPTION_COMPLETION_NEXT_ATTEMPT_AT"] = rfc(time.Hour)
	}, false)
	run("malformed-lineage", func(v map[string]string) { v["ADOPTION_ISSUE_ID"] = "not-a-uuid" }, false)
}

func TestAdoptionQuiescenceBehavior(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	contract := filepath.Join(root, "deploy", "strikeflow-response-publisher", "adoption-contract.sh")
	bin := t.TempDir()
	fake := filepath.Join(bin, "systemctl")
	script := `#!/bin/sh
case "$1" in
  is-active) test "${ACTIVE_UNIT:-}" = "$3" ;;
  is-enabled) test "${ENABLED_UNIT:-}" = "$3" ;;
  show) if test "${PID_UNIT:-}" = "$2"; then echo 42; else echo 0; fi ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(name, active, enabled, pid string, wantOK bool) {
		t.Helper()
		cmd := exec.Command("sh", "-c", `. "$1"; verify_response_reconciliation_stopped`, "sh", contract)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "ACTIVE_UNIT="+active, "ENABLED_UNIT="+enabled, "PID_UNIT="+pid)
		err := cmd.Run()
		if (err == nil) != wantOK {
			t.Fatalf("%s quiescence result=%v wantOK=%v", name, err, wantOK)
		}
	}
	run("all-stopped", "", "", "", true)
	run("ongoing-service-active", "strikeflow-multica-content-ongoing.service", "", "", false)
	run("ongoing-timer-enabled", "", "strikeflow-multica-content-ongoing.timer", "", false)
	run("dispatch-service-pid", "", "", "strikeflow-multica-content-dispatch.service", false)
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
		"receipt_lineage",
		"receipt_lineage safe-off requires an empty command list",
		"strikeflow-multica-content-dispatch.timer",
		"strikeflow-multica-content-dispatch.service",
		"stop_response_reconciliation",
		"response-reconciliation.before",
		"response-reconciliation.after-stop",
		"response-reconciliation.final",
		"safe_off_started=true",
		`while [ "$stop_attempt" -lt 3 ]`,
		`[ "$timer_pid" = 0 ]`, `[ "$service_pid" = 0 ]`,
		"failure-stop-reconciliation.log", "reconciliation_stop_status",
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
	stopScheduler := strings.LastIndex(text[:successStart], "stop_response_reconciliation")
	recreatePublisher := strings.Index(text[successStart:successEnd], "restore_disabled_candidate") + successStart
	if stopScheduler < 0 || recreatePublisher < successStart || stopScheduler > recreatePublisher {
		t.Fatal("response reconciliation must stop before publisher safe-off")
	}
}

func TestAdoptionToolIsRequiredBySealedReleaseClosure(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	verifier, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "verify-disabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(verifier)
	for _, required := range []string{"adoption-contract.sh$", `grep -c`, `-eq 1`, `sha256sum -c SHA256SUMS`} {
		if !strings.Contains(text, required) {
			t.Fatalf("sealed closure omits adoption tool requirement %q", required)
		}
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

func TestEnabledVerifierChecksExactMainLineCatalogSemantics(t *testing.T) {
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
		"indrelid='public.strikeflow_connector_reply_receipt'::regclass",
		"indrelid='public.strikeflow_response_outbox'::regclass",
		"tgrelid IN ('public.strikeflow_connector_reply_receipt'::regclass,'public.strikeflow_response_outbox'::regclass)",
		"tgtype=19",
		"strikeflow_response_outbox_identity_immutable",
		"reject_strikeflow_response_outbox_identity_change",
		"<> 2",
		"p.prosrc",
		"strikeflow command binding is immutable",
		"253_strikeflow_response_outbox",
		"257_strikeflow_response_outbox_event_id_unique",
		"258_strikeflow_content_reply_connector",
		"259_strikeflow_content_reply_receipt_unique",
		"260_strikeflow_response_outbox_identity_immutable",
		"strikeflow_connector_token_content_reply_scope_check",
		"idx_strikeflow_content_reply_receipt_unique",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("enabled verifier is missing exact immutability gate %q", required)
		}
	}
}

func TestPredecessorLedgerReconciliationIsSeparateEvidencePreservingGate(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "reconcile-predecessor-ledger.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"--confirm-reconcile", "flock -n", "7244554146635925501",
		"multica.strikeflow.response.producer.freeze", "pg_advisory_lock", "pg_advisory_unlock",
		"900001_strikeflow_response_outbox", "253_strikeflow_response_outbox",
		"900002_strikeflow_connector_reply_command_unique", "254_strikeflow_connector_reply_command_unique",
		"900003_strikeflow_response_outbox_event_unique", "255_strikeflow_response_outbox_event_unique",
		"900004_strikeflow_response_outbox_due_index", "256_strikeflow_response_outbox_due_index",
		"900005_strikeflow_response_outbox_event_id_unique", "257_strikeflow_response_outbox_event_id_unique",
		"235_strikeflow_connector_principal", "258_strikeflow_content_reply_connector",
		"259_strikeflow_content_reply_receipt_unique", "idx_strikeflow_content_reply_receipt_unique",
		"strikeflow_response_outbox", "strikeflow_connector_reply_receipt", "strikeflow_connector_content_reply_receipt",
		"outbox_before", "reply_before", "content_before", "ledger_before",
		"non-schema response evidence changed", "SHA256SUMS", "migration-ledger.after",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("predecessor reconciliation is missing %q", required)
		}
	}
	for _, forbidden := range []string{"UPDATE strikeflow_", "DELETE FROM", "TRUNCATE", "DROP TABLE", "migrate up", "migrate down"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("predecessor reconciliation contains forbidden mutation %q", forbidden)
		}
	}
}

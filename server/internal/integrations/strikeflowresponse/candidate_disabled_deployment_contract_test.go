package strikeflowresponse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisabledCandidateOverlayBypassesMigratorAndCarriesNoSecret(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "docker-compose.strikeflow-response-candidate-disabled.yml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{
		`entrypoint: ["./server"]`,
		`STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED: "false"`,
		`STRIKEFLOW_RESPONSE_AUTHORIZATION_MODE: ""`,
		`STRIKEFLOW_RESPONSE_EXCLUDED_ISSUE_IDS: ""`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("disabled candidate overlay missing %q", required)
		}
	}
	for _, forbidden := range []string{"strikeflow_response_hmac", "HMAC_HOST_FILE", "migrate"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("disabled candidate overlay contains forbidden %q", forbidden)
		}
	}
}

func TestDisabledCandidateVerifierReusesExactCatalogAndRequiresEmptyOutbox(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "verify-candidate-disabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{
		"verify-enabled-install.sh",
		`stat -c '%U:%G' "$release_dir"`,
		`stat -c '%U:%G %a' "$preflight_dir"`,
		`! -user root -o ! -group root`,
		`grep -c '^DO \$\$$'`,
		"--migration-preflight",
		`SELECT count(*) FROM strikeflow_response_outbox`,
		`test "$outbox_count" = 0`,
		`--allow-delivered-outbox`,
		`delivered_at IS NULL OR needs_attention_at IS NOT NULL`,
		`backend.get("entrypoint") != ["./server"]`,
		`STRIKEFLOW_RESPONSE_PUBLISHER_ENABLED`,
		`if [ "$mode" = migration-preflight ]; then`,
		`if [ "$mode" = before-start ]; then`,
		`test "$current" = "$(cat "$preflight_dir/multica-backend-1.identity")"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("disabled candidate verifier missing %q", required)
		}
	}
}

func TestDisabledCandidateDeployVerifiesBeforeRecreateAndRestoresOriginal(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "deploy-candidate-disabled.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	verify := strings.Index(text, "--before-start")
	change := strings.Index(text, "backend_changed=true")
	recreate := -1
	if change >= 0 {
		recreate = strings.Index(text[change:], "up -d --no-deps --force-recreate backend")
	}
	if verify < 0 || change < 0 || recreate < 0 || verify > change {
		t.Fatal("disabled candidate must be verified before backend recreation")
	}
	for _, required := range []string{
		"flock -n", "restore_original_backend", "failure-restore-original.log",
		"restore_disabled_candidate", "failure-restore-disabled.log",
		`-f "$base_compose" -f "$pin_compose" -f "$overlay"`,
		`--env-file "$base_env"`, `--env-file "$disabled_env"`,
		"verify-candidate-disabled-install.sh", "database.before", "database.after",
		"expected_original_ports", "SHA256SUMS",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("disabled candidate deploy missing %q", required)
		}
	}
	for _, forbidden := range []string{"migrate up", "migrate down", "DELETE FROM", "DROP TABLE"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("disabled candidate deploy contains forbidden mutation %q", forbidden)
		}
	}
}

func TestDisabledCandidateRollbackVerifiesBeforeRestoreAndPreservesEvidence(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "rollback-candidate-disabled.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	verify := strings.Index(text, "verify-candidate-disabled-install.sh")
	recreate := strings.Index(text, "up -d --no-deps --force-recreate backend")
	if verify < 0 || recreate < 0 || verify > recreate {
		t.Fatal("disabled candidate rollback must verify before restoring the original backend")
	}
	for _, required := range []string{
		"flock -n", "expected_original_image", "expected_original_ports", "SHA256SUMS",
		"restore_disabled_candidate", "failure-restore-disabled.log", "/run/secrets/strikeflow_response_hmac",
		"database.before", "database.after", "database.failure-final",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("disabled candidate rollback missing %q", required)
		}
	}
	for _, forbidden := range []string{"migrate down", "DELETE FROM", "DROP TABLE", "docker image rm"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("disabled candidate rollback contains forbidden evidence loss %q", forbidden)
		}
	}
}

func TestProductionMigrationWrapperUsesOneOffMigratorAndNeverStartsBackend(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "apply-mainline-migrations.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, required := range []string{
		"--confirm-migrate", "flock -n", `entrypoint: ["./migrate"]`,
		`test "$image_digest" = "$artifact_image_digest"`,
		`docker image inspect "$image_digest"`,
		`test "$expected_backup_name" = "$backup_file" || test "$expected_backup_name" = "$(basename "$backup_file")"`,
		"run --name", "--rm --no-deps -T -e", "MULTICA_MIGRATION_ALLOWLIST", "backend up", "verify-candidate-disabled-install.sh",
		"outbox_identity|", "content_identity|", "outbox_state|",
		"253_strikeflow_response_outbox", "257_strikeflow_response_outbox_event_id_unique",
		"258_strikeflow_content_reply_connector", "259_strikeflow_content_reply_receipt_unique",
		"migration-ledger.expected-before", "migration-ledger.before.normalized",
		"migration-ledger.after.normalized", "cmp", "producer-freeze.fifo",
		"strikeflow-multica-content-ongoing.service", "strikeflow-multica-content-dispatch.timer",
		"failure-migrator-stop.log", "database.failure-final", "SHA256SUMS",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("production migration wrapper missing %q", required)
		}
	}
	for _, forbidden := range []string{
		"up -d", "migrate down", "DELETE FROM", "DROP TABLE",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("production migration wrapper contains forbidden operation %q", forbidden)
		}
	}
	if strings.Contains(text, `-v aliases="$(printf '%s' "$canonical_aliases"`) {
		t.Fatal("mainline before-ledger normalization must retain already-reconciled canonical aliases")
	}
}

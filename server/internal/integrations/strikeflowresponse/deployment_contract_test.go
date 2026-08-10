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
	for _, required := range []string{"fail_closed", "publisher.env.safe-off", "activation_verified=false", "fail-closed-fallback.log", "assert_original_backend", "restored_image_ref", "/run/secrets/strikeflow_response_hmac"} {
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
	for _, required := range []string{"restored_image_ref", "assert_original_backend", "restore_original_backend", "failure-restore-original.log"} {
		if !strings.Contains(text, required) {
			t.Fatalf("rollback is missing original-backend safety contract %q", required)
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

func TestEnabledVerifierChecksExactImmutabilitySemantics(t *testing.T) {
	root := responsePublisherRepoRoot(t)
	verifier, err := os.ReadFile(filepath.Join(root, "deploy", "strikeflow-response-publisher", "verify-enabled-install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(verifier)
	for _, required := range []string{
		"len(raw) > 4096",
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

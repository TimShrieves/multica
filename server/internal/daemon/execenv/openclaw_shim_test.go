package execenv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIsOpenclawShimPath locks the shim-detection surface. Case-insensitivity
// matters because Windows PATH resolution is case-insensitive and npm/PATHEXT
// can hand back `OPENCLAW.CMD`; paths containing spaces and non-ASCII segments
// are included because those are the Windows install locations most likely to
// be mis-parsed, and #6061's open questions called them out explicitly.
func TestIsOpenclawShimPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		bin  string
		want bool
	}{
		{"npm cmd shim", `C:\Users\dev\AppData\Roaming\npm\openclaw.cmd`, true},
		{"uppercase extension", `C:\npm\OPENCLAW.CMD`, true},
		{"mixed case extension", `C:\npm\openclaw.Cmd`, true},
		{"legacy bat shim", `C:\npm\openclaw.bat`, true},
		{"path with spaces", `C:\Program Files\node modules\openclaw.cmd`, true},
		{"path with unicode segment", `C:\用户\开发\npm\openclaw.cmd`, true},
		{"surrounding whitespace", "  C:\\npm\\openclaw.cmd  ", true},
		{"real executable", `C:\npm\openclaw.exe`, false},
		{"powershell shim is not a batch shim", `C:\npm\openclaw.ps1`, false},
		{"unix binary without extension", "/usr/local/bin/openclaw", false},
		{"unix path with dotted directory", "/opt/openclaw.cmd.d/openclaw", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isOpenclawShimPath(tc.bin); got != tc.want {
				t.Fatalf("isOpenclawShimPath(%q) = %v, want %v", tc.bin, got, tc.want)
			}
		})
	}
}

// exitError produces a real *exec.ExitError so the diagnostic's errors.As gate
// is exercised against the same type production sees, not a stand-in.
//
// The interpreter is invoked by absolute path on purpose: callers stub PATH to
// control the interpreter lookup, and a PATH-dependent helper would break
// depending on the order those two happen in.
func exitError(t *testing.T) error {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		shell := os.Getenv("ComSpec")
		if shell == "" {
			shell = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
		}
		cmd = exec.Command(shell, "/c", "exit 1")
	} else {
		cmd = exec.Command("/bin/sh", "-c", "exit 1")
	}
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T (%v)", err, err)
	}
	return err
}

// pathWithout points PATH at an empty directory so the interpreter cannot
// resolve. Setting PATH rather than clearing it keeps LookPath on its normal
// code path instead of its empty-PATH special case.
func pathWithout(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// pathWithFakeNode puts an executable named like the interpreter on PATH.
// The file only has to be *resolvable* — the diagnostic reports lookup results
// and never runs it.
func pathWithFakeNode(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	name := openclawShimInterpreter
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	fake := filepath.Join(dir, name)
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake interpreter: %v", err)
	}
	t.Setenv("PATH", dir)
	return fake
}

// TestOpenclawShimDiagnosticNamesMissingInterpreter is the core #6061
// regression: a silent shim exit must be reported as an unresolvable
// interpreter, with an actionable next step, instead of a bare exit code.
func TestOpenclawShimDiagnosticNamesMissingInterpreter(t *testing.T) {
	pathWithout(t)
	got := openclawShimDiagnostic(`C:\npm\openclaw.cmd`, exitError(t))
	if got == "" {
		t.Fatal("expected a diagnostic for a silent .cmd shim failure, got none")
	}
	for _, want := range []string{
		"not resolvable on the daemon PATH",
		openclawShimInterpreter,
		`C:\npm\openclaw.cmd`,
		"restart the daemon",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic missing %q\ngot: %s", want, got)
		}
	}
}

// TestOpenclawShimDiagnosticReportsResolvableInterpreter guards the other
// direction, which is the evidence that actually discriminates between the
// competing #6061 hypotheses. If the interpreter resolves, the diagnostic must
// say so rather than blaming PATH, otherwise the next bug report gets steered
// toward the wrong root cause.
func TestOpenclawShimDiagnosticReportsResolvableInterpreter(t *testing.T) {
	fake := pathWithFakeNode(t)
	got := openclawShimDiagnostic(`C:\npm\openclaw.cmd`, exitError(t))
	if got == "" {
		t.Fatal("expected a diagnostic for a silent .cmd shim failure, got none")
	}
	if !strings.Contains(got, "the interpreter is reachable") {
		t.Errorf("diagnostic should clear PATH of blame\ngot: %s", got)
	}
	if !strings.Contains(got, fake) {
		t.Errorf("diagnostic should name the resolved interpreter %q\ngot: %s", fake, got)
	}
	if strings.Contains(got, "not resolvable") {
		t.Errorf("diagnostic must not claim the interpreter is missing\ngot: %s", got)
	}
}

// TestOpenclawShimDiagnosticDoesNotLeakFullPath keeps the message log-safe:
// it summarises PATH as a count, so daemon logs and pasted bug reports do not
// carry a full environment dump.
func TestOpenclawShimDiagnosticDoesNotLeakFullPath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "totally-not-a-secret-dir")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("PATH", secret)
	got := openclawShimDiagnostic(`C:\npm\openclaw.cmd`, exitError(t))
	if got == "" {
		t.Fatal("expected a diagnostic, got none")
	}
	if strings.Contains(got, secret) {
		t.Errorf("diagnostic leaked a raw PATH entry\ngot: %s", got)
	}
	if !strings.Contains(got, "1 entry") {
		t.Errorf("diagnostic should summarise PATH as a count\ngot: %s", got)
	}
}

// TestOpenclawShimDiagnosticStaysSilentOutOfScope pins the no-op cases. A
// diagnostic attached to a timeout, a missing binary, or a normal native
// executable would be actively misleading.
func TestOpenclawShimDiagnosticStaysSilentOutOfScope(t *testing.T) {
	pathWithout(t)
	realExit := exitError(t)
	cases := []struct {
		name string
		bin  string
		err  error
	}{
		{"native executable", `C:\npm\openclaw.exe`, realExit},
		{"unix binary", "/usr/local/bin/openclaw", realExit},
		{"context deadline", `C:\npm\openclaw.cmd`, context.DeadlineExceeded},
		{"context canceled", `C:\npm\openclaw.cmd`, context.Canceled},
		{"binary not found", `C:\npm\openclaw.cmd`, exec.ErrNotFound},
		{"nil error", `C:\npm\openclaw.cmd`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openclawShimDiagnostic(tc.bin, tc.err); got != "" {
				t.Fatalf("expected no diagnostic, got: %s", got)
			}
		})
	}
}

// TestOpenclawShimDiagnosticSurvivesWrappedError confirms the errors.As gate
// still fires when the exit error arrives wrapped, which is how it reaches this
// code once callers have annotated it.
func TestOpenclawShimDiagnosticSurvivesWrappedError(t *testing.T) {
	pathWithout(t)
	wrapped := errors.Join(errors.New("openclaw config file"), exitError(t))
	if got := openclawShimDiagnostic(`C:\npm\openclaw.cmd`, wrapped); got == "" {
		t.Fatal("expected diagnostic through a wrapped exit error, got none")
	}
}

// writeSilentFailingShim creates an executable named with a `.cmd` extension
// that exits 1 without writing to stderr — the exact shape #6061 reported.
//
// On Unix the shebang makes a `.cmd`-named file genuinely executable, so the
// full execOpenclawCLI integration path is provable on the normal test job.
// The real npm-shim-plus-interpreter reproduction lives in the windows-tagged
// test file.
func writeSilentFailingShim(t *testing.T, dir string) string {
	t.Helper()
	shim := filepath.Join(dir, "openclaw.cmd")
	body := "#!/bin/sh\nexit 1\n"
	if runtime.GOOS == "windows" {
		body = "@echo off\r\nexit /b 1\r\n"
	}
	if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return shim
}

// TestExecOpenclawCLIAnnotatesSilentShimFailure is the end-to-end proof that
// the diagnostic reaches the error the daemon logs. Before this change the
// message stopped at `exit status 1`, which is what left #6061's reporter
// running their own subprocess experiments to find the cause.
func TestExecOpenclawCLIAnnotatesSilentShimFailure(t *testing.T) {
	shim := writeSilentFailingShim(t, t.TempDir())
	// Set PATH after creating the shim: the shim is invoked by absolute path,
	// while the interpreter lookup must miss.
	pathWithout(t)

	_, err := execOpenclawCLI(context.Background(), shim, "config", "file")
	if err == nil {
		t.Fatal("expected the shim failure to surface as an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "openclaw config file") {
		t.Errorf("error should name the failing subcommand\ngot: %s", msg)
	}
	if !strings.Contains(msg, "not resolvable on the daemon PATH") {
		t.Errorf("error should carry the shim diagnostic\ngot: %s", msg)
	}
}

// TestExecOpenclawCLIPrefersRealStderr guarantees the diagnostic never masks a
// genuine message from the CLI. #6061 asserted that a failing `.cmd` produces
// stderr Go cannot capture; that claim is unproven, so the code must behave
// correctly either way — and when stderr does arrive, it wins.
func TestExecOpenclawCLIPrefersRealStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("covered by the windows-tagged shim test with a real cmd.exe host")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "openclaw.cmd")
	body := "#!/bin/sh\necho 'openclaw doctor says hello' >&2\nexit 1\n"
	if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	pathWithout(t)

	_, err := execOpenclawCLI(context.Background(), shim, "config", "file")
	if err == nil {
		t.Fatal("expected the shim failure to surface as an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "openclaw doctor says hello") {
		t.Errorf("real stderr must be preserved\ngot: %s", msg)
	}
	if strings.Contains(msg, "no stderr output") {
		t.Errorf("diagnostic must not fire when stderr is present\ngot: %s", msg)
	}
}

// TestExecOpenclawCLIMissingTempDoesNotChangeOutcome pins the root cause #6061
// originally reported and then retracted. The reporter's own follow-up
// experiment showed `{PATH, SystemRoot}` alone succeeds, so TEMP/TMP must not
// be load-bearing for the OpenClaw CLI invocation. Locking that keeps a future
// change from quietly reintroducing a temp-dir dependency and resurrecting a
// root cause we already ruled out.
func TestExecOpenclawCLIMissingTempDoesNotChangeOutcome(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "openclaw.cmd")
	body := "#!/bin/sh\necho '/tmp/openclaw/config.json'\n"
	if runtime.GOOS == "windows" {
		body = "@echo off\r\necho C:\\openclaw\\config.json\r\n"
	}
	if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("TEMP", "")
	t.Setenv("TMP", "")

	out, err := execOpenclawCLI(context.Background(), shim, "config", "file")
	if err != nil {
		t.Fatalf("invocation must not depend on TEMP/TMP: %v", err)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatal("expected the shim's stdout to be returned")
	}
}

// TestExecOpenclawCLIHandlesShimInPathWithSpacesAndUnicode covers the install
// locations #6061's open questions flagged as unverified. A directory
// containing a space or non-ASCII characters must not break invocation or
// mangle the captured output.
func TestExecOpenclawCLIHandlesShimInPathWithSpacesAndUnicode(t *testing.T) {
	for _, segment := range []string{"Program Files", "用户 開發", "café dir"} {
		t.Run(segment, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), segment)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %q: %v", dir, err)
			}
			shim := filepath.Join(dir, "openclaw.cmd")
			body := "#!/bin/sh\necho 'ok-marker'\n"
			if runtime.GOOS == "windows" {
				body = "@echo off\r\necho ok-marker\r\n"
			}
			if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
				t.Fatalf("write shim: %v", err)
			}
			out, err := execOpenclawCLI(context.Background(), shim, "config", "file")
			if err != nil {
				t.Fatalf("shim in %q should be invocable: %v", dir, err)
			}
			if !strings.Contains(out, "ok-marker") {
				t.Fatalf("expected shim stdout to survive intact, got %q", out)
			}
		})
	}
}

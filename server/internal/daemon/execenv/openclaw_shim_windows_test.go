//go:build windows

package execenv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is the Windows half of the MUL-5422 / #6061 regression. The
// cross-platform file proves the diagnostic's logic; only a real cmd.exe host
// can prove how an npm batch shim actually behaves when its interpreter is
// missing — which is the specific claim #6061 made and could not substantiate
// ("the error goes to the .cmd layer and Go's stderr pipe misses it").
//
// Run via the ci.yml `windows-execenv` job, which scopes -run patterns to the
// Windows-safe tests in this package.

// writeNpmStyleShim writes a batch shim shaped like the one npm generates for
// `openclaw`: it re-execs a JavaScript entrypoint through `node`, resolved from
// PATH rather than by absolute path. That PATH-relative interpreter lookup is
// the invisible second resolution step at the heart of #6061.
func writeNpmStyleShim(t *testing.T, dir string) string {
	t.Helper()
	entry := filepath.Join(dir, "openclaw.mjs")
	if err := os.WriteFile(entry, []byte("console.log(String.raw`C:\\openclaw\\config.json`);\n"), 0o644); err != nil {
		t.Fatalf("write entrypoint: %v", err)
	}
	shim := filepath.Join(dir, "openclaw.cmd")
	body := "@echo off\r\nnode \"%~dp0openclaw.mjs\" %*\r\n"
	if err := os.WriteFile(shim, []byte(body), 0o644); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	return shim
}

// nodeDir returns the directory holding a real node.exe, skipping the test when
// the runner has no Node installed.
func nodeDir(t *testing.T) string {
	t.Helper()
	resolved, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("no node on PATH, cannot exercise a real npm shim: %v", err)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		t.Fatalf("resolve node path: %v", err)
	}
	return filepath.Dir(abs)
}

// systemPath is the minimum PATH a batch shim needs to run at all (cmd.exe and
// friends live there), without any Node directory on it.
func systemPath(t *testing.T) string {
	t.Helper()
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	return strings.Join([]string{
		filepath.Join(root, "System32"),
		root,
	}, string(os.PathListSeparator))
}

// TestWindowsOpenclawShimMissingNodeProducesActionableError is the empirical
// answer #6061 needs. With a real npm-style shim and no Node on PATH, the
// failure the daemon reports must be actionable — either cmd.exe's own
// "not recognized" text reached Go's stderr pipe, or our diagnostic supplied
// the equivalent. Asserting the disjunction (rather than guessing which)
// is deliberate: the test stays honest about an unresolved platform question
// while still forbidding the bare `exit status 1` that started this issue.
func TestWindowsOpenclawShimMissingNodeProducesActionableError(t *testing.T) {
	nodeDir(t) // skip early if the runner has no Node to remove from PATH
	shim := writeNpmStyleShim(t, t.TempDir())
	t.Setenv("PATH", systemPath(t))

	out, err := execOpenclawCLI(context.Background(), shim, "config", "file")
	if err == nil {
		t.Fatalf("shim must fail without node on PATH, got output %q", out)
	}
	msg := err.Error()
	t.Logf("observed error: %s", msg)

	stderrReachedGo := strings.Contains(strings.ToLower(msg), "not recognized")
	diagnosticFired := strings.Contains(msg, "not resolvable on the daemon PATH")
	switch {
	case stderrReachedGo:
		t.Log("cmd.exe stderr DID reach Go's pipe; #6061's stderr claim does not hold")
	case diagnosticFired:
		t.Log("cmd.exe stderr did NOT reach Go's pipe; the shim diagnostic supplied the cause")
	default:
		t.Fatalf("error is not actionable — no stderr and no shim diagnostic: %s", msg)
	}
	if !strings.Contains(msg, "openclaw config file") {
		t.Errorf("error should name the failing subcommand\ngot: %s", msg)
	}
}

// TestWindowsOpenclawShimSucceedsWithNodeOnPath is the positive control. The
// same shim, same absolute path, differing only by whether Node is reachable —
// this is what makes interpreter resolution the proven discriminator rather
// than an assumed one.
func TestWindowsOpenclawShimSucceedsWithNodeOnPath(t *testing.T) {
	dir := nodeDir(t)
	shim := writeNpmStyleShim(t, t.TempDir())
	t.Setenv("PATH", strings.Join([]string{dir, systemPath(t)}, string(os.PathListSeparator)))

	out, err := execOpenclawCLI(context.Background(), shim, "config", "file")
	if err != nil {
		t.Fatalf("shim should succeed with node on PATH: %v", err)
	}
	if !strings.Contains(out, `C:\openclaw\config.json`) {
		t.Fatalf("expected the entrypoint's stdout, got %q", out)
	}
}

// TestWindowsOpenclawShimSucceedsWithoutTempVars closes out the root cause
// #6061 first reported and then retracted: with Node reachable but TEMP and TMP
// both unset, the shim must still succeed. Node falls back to a system temp
// directory, so these variables are not load-bearing — and pinning that stops a
// future change from reintroducing the dependency.
func TestWindowsOpenclawShimSucceedsWithoutTempVars(t *testing.T) {
	dir := nodeDir(t)
	shim := writeNpmStyleShim(t, t.TempDir())
	t.Setenv("PATH", strings.Join([]string{dir, systemPath(t)}, string(os.PathListSeparator)))
	t.Setenv("TEMP", "")
	t.Setenv("TMP", "")

	out, err := execOpenclawCLI(context.Background(), shim, "config", "file")
	if err != nil {
		t.Fatalf("shim must not depend on TEMP/TMP: %v", err)
	}
	if !strings.Contains(out, `C:\openclaw\config.json`) {
		t.Fatalf("expected the entrypoint's stdout, got %q", out)
	}
}

// TestWindowsOpenclawShimInPathWithSpacesAndUnicode covers the install
// locations #6061's open questions flagged. `%~dp0` expansion and Go's batch
// argument handling both have to survive a directory with a space or non-ASCII
// characters — `C:\Program Files\...` is a completely ordinary install target.
func TestWindowsOpenclawShimInPathWithSpacesAndUnicode(t *testing.T) {
	nodeRoot := nodeDir(t)
	for _, segment := range []string{"Program Files copy", "用户 開發"} {
		t.Run(segment, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), segment)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("mkdir %q: %v", dir, err)
			}
			shim := writeNpmStyleShim(t, dir)
			t.Setenv("PATH", strings.Join([]string{nodeRoot, systemPath(t)}, string(os.PathListSeparator)))

			out, err := execOpenclawCLI(context.Background(), shim, "config", "file")
			if err != nil {
				t.Fatalf("shim in %q should be invocable: %v", dir, err)
			}
			if !strings.Contains(out, `C:\openclaw\config.json`) {
				t.Fatalf("expected the entrypoint's stdout, got %q", out)
			}
		})
	}
}

package execenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// openclawShimExtensions are the batch-wrapper extensions npm uses when it
// installs the `openclaw` command on Windows. The shim is not the real
// program: it re-execs OpenClaw's JavaScript entrypoint through an
// interpreter, so a shim path that resolves and is executable says nothing
// about whether the interpreter it depends on is reachable.
var openclawShimExtensions = map[string]struct{}{
	".cmd": {},
	".bat": {},
}

// openclawShimInterpreter is the interpreter an npm-installed OpenClaw shim
// re-execs. OpenClaw's package `bin` entry points at `openclaw.mjs`, whose
// shebang is `#!/usr/bin/env node`, so a missing `node` breaks the shim from
// the inside while the shim itself still looks perfectly valid to the daemon.
const openclawShimInterpreter = "node"

// isOpenclawShimPath reports whether bin is a batch shim rather than a directly
// executable binary.
//
// Keyed on the file extension alone, deliberately not on runtime.GOOS: a
// `.cmd`/`.bat` shim only ever appears on Windows in production, and testing
// the extension instead of the host OS lets the whole diagnostic be exercised
// from the normal Linux/macOS test job rather than only on a Windows runner.
func isOpenclawShimPath(bin string) bool {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(bin)))
	_, ok := openclawShimExtensions[ext]
	return ok
}

// openclawShimDiagnostic explains a batch-shim invocation that failed without
// writing anything to stderr, and returns "" when it has nothing to add.
//
// Why this exists (MUL-5422 / #6061): a Windows user reported every OpenClaw
// task failing in execenv prep with a bare `exit status 1` and no stderr. The
// daemon pins `openclaw` to an absolute path, so the failing command looked
// correct; what the error could not show is that the shim's own `node` lookup
// is a second, invisible resolution step that can fail independently. The
// reporter needed hand-run `subprocess` experiments to discover that, which is
// exactly the diagnostic the daemon should have handed them.
//
// This only enriches the error text — it never changes control flow, and it
// never suppresses a real stderr message (callers try stderr first). It also
// deliberately reports the interpreter lookup result in BOTH directions:
//
//   - interpreter missing → names it as the likely cause, with a fix hint.
//   - interpreter present → says so, which is the more valuable evidence. It
//     rules the PATH theory out and points at the remaining hypotheses (PATH
//     drift between the runtime's `--version` gate and task prep, a broken
//     OpenClaw install, or a shim failing for an unrelated reason).
//
// The PATH itself is summarised as an entry count rather than dumped: this
// string lands in daemon logs and issue reports, and a full PATH is both noisy
// and more environment detail than a bug report needs.
func openclawShimDiagnostic(bin string, runErr error) string {
	// Only an actual non-zero exit is in scope. A context deadline, a missing
	// binary, or a permission error already describes itself, and appending an
	// interpreter lookup to those would be misleading noise.
	var exitErr *exec.ExitError
	if !errors.As(runErr, &exitErr) {
		return ""
	}
	if !isOpenclawShimPath(bin) {
		return ""
	}

	// LookPath reads the current process PATH, which is the same environment
	// execOpenclawCLI hands the child via os.Environ() — so this reports the
	// interpreter visibility the shim actually had, not a different view.
	pathSummary := openclawPathEntrySummary()
	interpreterPath, lookErr := exec.LookPath(openclawShimInterpreter)
	if lookErr != nil {
		return fmt.Sprintf(
			"no stderr output; %s shim %s re-execs %q, which is not resolvable on the daemon PATH (%s) — "+
				"install Node.js or restart the daemon from an environment where %q is on PATH",
			strings.ToLower(filepath.Ext(bin)), bin, openclawShimInterpreter, pathSummary, openclawShimInterpreter,
		)
	}
	return fmt.Sprintf(
		"no stderr output; %s shim %s failed even though %q resolves to %s on the daemon PATH (%s) — "+
			"the interpreter is reachable, so check the OpenClaw install itself",
		strings.ToLower(filepath.Ext(bin)), bin, openclawShimInterpreter, interpreterPath, pathSummary,
	)
}

// openclawPathEntrySummary describes the daemon PATH by size alone. A count is
// enough to tell "the daemon inherited a stripped environment" apart from "PATH
// looks normal but the interpreter still is not on it", without copying the
// user's full PATH into a daemon log or a pasted bug report.
func openclawPathEntrySummary() string {
	if n := len(filepath.SplitList(os.Getenv("PATH"))); n != 1 {
		return fmt.Sprintf("%d entries", n)
	}
	return "1 entry"
}

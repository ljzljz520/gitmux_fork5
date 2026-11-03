//go:build !windows
// +build !windows

package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/arl/gitstatus"

	jsonfmt "github.com/arl/gitmux/json"
	"github.com/arl/gitmux/tmux"
)

// fakeGitScript is a deterministic, controllable git replacement used by the
// tests. It answers the exact git invocations issued by gitstatus and by the
// collect probes, sourcing its answers from a small set of named generations:
//
//   - alpha: attached branch, short HEAD 1111111
//   - beta:  attached branch, short HEAD 2222222
//   - gamma: detached HEAD, short HEAD 3333333
//   - delta: attached, unborn branch (no commit yet)
//
// The behavior around 'git status' calls is driven by the .mode file, which
// makes branch switches fully controllable (a switch can be forced to straddle
// the porcelain sampling and the later rev-parse, settle after one round, or
// flap forever). All state lives in files of the current working directory.
const fakeGitScript = `#!/bin/sh
[ -r .inv ] || echo 0 > .inv
read v < .inv
echo $((v+1)) > .inv

if [ -r .slow ]; then
	read pat < .slow
	case " $* " in
	*"$pat"*) exec /bin/sleep 5 ;;
	esac
fi

read m < .mode
read g < .g
read i < .id

short_of() {
	case $1 in
	alpha) echo 1111111 ;;
	beta) echo 2222222 ;;
	gamma) echo 3333333 ;;
	esac
}
full_of() {
	case $1 in
	alpha) echo 1111111111111111111111111111111111111111 ;;
	beta) echo 2222222222222222222222222222222222222222 ;;
	gamma) echo 3333333333333333333333333333333333333333 ;;
	esac
}
paths_of() {
	echo "$PWD/git-$1-$i"
	echo "$PWD/top-$1-$i"
}
status_of() {
	case $1 in
	alpha) printf '## alpha\000' ;;
	beta) printf '## beta\000' ;;
	gamma) printf '## HEAD (no branch)\000' ;;
	delta) printf '## No commits yet on delta\000' ;;
	esac
}

case $1 in
status)
	read c < .calls
	c=$((c+1))
	echo $c > .calls
	switch=
	case $m in
	settle)
		if [ $c -eq 1 ]; then g=alpha; switch=beta; fi
		;;
	detached-settle)
		if [ $c -eq 1 ]; then g=alpha; switch=gamma; fi
		;;
	unborn-settle)
		if [ $c -eq 1 ]; then g=alpha; switch=delta; fi
		;;
	flap)
		g=alpha
		switch=beta
		;;
	identity-settle)
		# Branch/HEAD stay alpha, only worktree identity changes once.
		status_of alpha
		if [ $c -eq 1 ]; then echo 1 > .id; fi
		exit 0
		;;
	esac
	status_of "$g"
	if [ -n "$switch" ]; then echo "$switch" > .g; fi
	exit 0
	;;
diff|stash)
	exit 0
	;;
rev-parse)
	if [ "$2" = "--absolute-git-dir" ]; then
		if [ "$g" = "delta" ] && [ "${4:-}" = "HEAD" ]; then
			paths_of "$g"
			echo "fatal: ambiguous argument 'HEAD': unknown revision" >&2
			exit 128
		fi
		paths_of "$g"
		full_of "$g"
		exit 0
	fi
	# gitstatus form: rev-parse --git-dir --short HEAD
	echo .git
	short_of "$g"
	;;
symbolic-ref)
	case $g in
	alpha|beta|delta) echo "$g" ;;
	gamma) exit 1 ;;
	esac
	;;
*)
	echo "fake git: unexpected invocation: $*" >&2
	exit 2
	;;
esac
`

// setupFakeGit installs the fake git on PATH, creates a working directory
// populated with the given initial state and makes it the process working
// directory (all git invocations are relative to it).
func setupFakeGit(t *testing.T, mode, gen string) string {
	t.Helper()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitPath := filepath.Join(bin, "git")
	if err := os.WriteFile(gitPath, []byte(fakeGitScript), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	writeState := func(name, val string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(val), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeState(".mode", mode)
	writeState(".g", gen)
	writeState(".id", "0")
	writeState(".calls", "0")
	writeState(".inv", "0")

	t.Chdir(work)
	return work
}

func readState(t *testing.T, work, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(work, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func statusCalls(t *testing.T, work string) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(readState(t, work, ".calls"), "%d", &n); err != nil {
		t.Fatal(err)
	}
	return n
}

// formatBoth mirrors the gitmux main output flow and renders st through the
// two output ends: the debug JSON formatter and the tmux formatter.
func formatBoth(t *testing.T, st *gitstatus.Status) (dbgJSON, tmuxLine string) {
	t.Helper()

	var jb bytes.Buffer
	if err := (jsonfmt.Formater{}).Format(&jb, st); err != nil {
		t.Fatalf("json formatter: %v", err)
	}

	var tb bytes.Buffer
	fmter := tmux.Formater{Config: tmux.Config{Layout: []string{"branch"}}}
	if err := fmter.Format(&tb, st); err != nil {
		t.Fatalf("tmux formatter: %v", err)
	}
	return jb.String(), tb.String()
}

func jsonField(t *testing.T, raw, field string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("decoding formatter json %q: %v", raw, err)
	}
	v, ok := m[field]
	if !ok {
		t.Fatalf("json output has no %q field: %s", field, raw)
	}
	return v
}

func TestNewStatus(t *testing.T) {
	ctx := context.Background()

	t.Run("stable attached branch sampled once", func(t *testing.T) {
		work := setupFakeGit(t, "stable", "alpha")

		st, err := NewStatus(ctx, Options{})
		if err != nil {
			t.Fatalf("NewStatus: %v", err)
		}
		if st.LocalBranch != "alpha" || st.HEAD != "1111111" || st.IsDetached || st.IsInitial {
			t.Fatalf("unexpected status: %+v", st)
		}
		if n := statusCalls(t, work); n != 1 {
			t.Fatalf("status collections = %d, want 1", n)
		}

		dbg, line := formatBoth(t, st)
		if got := jsonField(t, dbg, "LocalBranch"); got != "alpha" {
			t.Fatalf("json LocalBranch = %v, want alpha", got)
		}
		if got := jsonField(t, dbg, "HEAD"); got != "1111111" {
			t.Fatalf("json HEAD = %v, want 1111111", got)
		}
		if !strings.Contains(line, "alpha") {
			t.Fatalf("tmux output %q does not show branch alpha", line)
		}
	})

	t.Run("switch during round re-samples and settles", func(t *testing.T) {
		work := setupFakeGit(t, "settle", "alpha")

		st, err := NewStatus(ctx, Options{})
		if err != nil {
			t.Fatalf("NewStatus: %v", err)
		}
		if st.LocalBranch != "beta" || st.HEAD != "2222222" || st.IsDetached {
			t.Fatalf("unexpected status: %+v", st)
		}
		if n := statusCalls(t, work); n != 2 {
			t.Fatalf("status collections = %d, want 2 (bounded resample)", n)
		}

		dbg, line := formatBoth(t, st)
		if got := jsonField(t, dbg, "LocalBranch"); got != "beta" {
			t.Fatalf("json LocalBranch = %v, want beta", got)
		}
		if got := jsonField(t, dbg, "HEAD"); got != "2222222" {
			t.Fatalf("json HEAD = %v, want 2222222", got)
		}
		if !strings.Contains(dbg, `"LocalBranch": "beta"`) {
			t.Fatalf("json output not from the settled generation:\n%s", dbg)
		}
		if strings.Contains(dbg, "alpha") || strings.Contains(line, "alpha") {
			t.Fatalf("output mixes old generation alpha:\n%s\n%s", dbg, line)
		}
		if !strings.Contains(line, "beta") {
			t.Fatalf("tmux output %q does not show branch beta", line)
		}
	})

	t.Run("never settling returns ErrUnstable and no summary is emitted", func(t *testing.T) {
		work := setupFakeGit(t, "flap", "alpha")

		st, err := NewStatus(ctx, Options{MaxAttempts: 2})
		if !errors.Is(err, ErrUnstable) {
			t.Fatalf("err = %v, want ErrUnstable", err)
		}
		if st != nil {
			t.Fatalf("status = %+v, want nil on instability", st)
		}
		if n := statusCalls(t, work); n != 2 {
			t.Fatalf("status collections = %d, want exactly MaxAttempts=2", n)
		}
		if !strings.Contains(err.Error(), "branch:alpha") ||
			!strings.Contains(err.Error(), "branch:beta") {
			t.Fatalf("error does not identify both generations: %v", err)
		}

		// Mirror main: the formatters must run only when collection succeeds.
		var js, tm bytes.Buffer
		if st != nil {
			_ = (jsonfmt.Formater{}).Format(&js, st)
		}
		if js.Len() != 0 || tm.Len() != 0 {
			t.Fatalf("mixed summary leaked to an output: json=%q tmux=%q", js, tm)
		}
	})

	t.Run("switch to detached HEAD re-samples and settles", func(t *testing.T) {
		work := setupFakeGit(t, "detached-settle", "alpha")

		st, err := NewStatus(ctx, Options{})
		if err != nil {
			t.Fatalf("NewStatus: %v", err)
		}
		if !st.IsDetached || st.LocalBranch != "" || st.HEAD != "3333333" {
			t.Fatalf("unexpected status: %+v", st)
		}
		if n := statusCalls(t, work); n != 2 {
			t.Fatalf("status collections = %d, want 2", n)
		}

		dbg, line := formatBoth(t, st)
		if got := jsonField(t, dbg, "IsDetached"); got != true {
			t.Fatalf("json IsDetached = %v, want true", got)
		}
		if got := jsonField(t, dbg, "HEAD"); got != "3333333" {
			t.Fatalf("json HEAD = %v, want 3333333", got)
		}
		if !strings.Contains(line, "3333333") {
			t.Fatalf("tmux output %q does not show detached HEAD", line)
		}
		if strings.Contains(line, "alpha") {
			t.Fatalf("tmux output mixes old branch alpha: %q", line)
		}
	})

	t.Run("stable unborn branch", func(t *testing.T) {
		work := setupFakeGit(t, "stable", "delta")

		st, err := NewStatus(ctx, Options{})
		if err != nil {
			t.Fatalf("NewStatus: %v", err)
		}
		if !st.IsInitial || st.IsDetached || st.LocalBranch != "delta" || st.HEAD != "" {
			t.Fatalf("unexpected status: %+v", st)
		}
		if n := statusCalls(t, work); n != 1 {
			t.Fatalf("status collections = %d, want 1", n)
		}

		dbg, line := formatBoth(t, st)
		if got := jsonField(t, dbg, "IsInitial"); got != true {
			t.Fatalf("json IsInitial = %v, want true", got)
		}
		if got := jsonField(t, dbg, "LocalBranch"); got != "delta" {
			t.Fatalf("json LocalBranch = %v, want delta", got)
		}
		if !strings.Contains(line, "delta") {
			t.Fatalf("tmux output %q does not show unborn branch delta", line)
		}
	})

	t.Run("switch to unborn branch re-samples and settles", func(t *testing.T) {
		work := setupFakeGit(t, "unborn-settle", "alpha")

		st, err := NewStatus(ctx, Options{})
		if err != nil {
			t.Fatalf("NewStatus: %v", err)
		}
		if !st.IsInitial || st.LocalBranch != "delta" || st.HEAD != "" {
			t.Fatalf("unexpected status: %+v", st)
		}
		if n := statusCalls(t, work); n != 2 {
			t.Fatalf("status collections = %d, want 2", n)
		}
	})

	t.Run("worktree identity change alone triggers resample", func(t *testing.T) {
		work := setupFakeGit(t, "identity-settle", "alpha")

		st, err := NewStatus(ctx, Options{})
		if err != nil {
			t.Fatalf("NewStatus: %v", err)
		}
		if st.LocalBranch != "alpha" || st.HEAD != "1111111" {
			t.Fatalf("unexpected status: %+v", st)
		}
		if n := statusCalls(t, work); n != 2 {
			t.Fatalf("status collections = %d, want 2 (identity resample)", n)
		}
		if id := readState(t, work, ".id"); id != "1" {
			t.Fatalf("identity switch not consumed, .id = %q", id)
		}
	})

	t.Run("timeout is preserved and bounds the wait", func(t *testing.T) {
		work := setupFakeGit(t, "stable", "alpha")
		if err := os.WriteFile(filepath.Join(work, ".slow"),
			[]byte("rev-parse --absolute-git-dir"), 0o644); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		defer cancel()

		start := time.Now()
		_, err := NewStatus(ctx, Options{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("collection waited %v after the timeout", elapsed)
		}
	})

	t.Run("already cancelled context does not sample", func(t *testing.T) {
		work := setupFakeGit(t, "stable", "alpha")

		ctx, cancel := context.WithCancel(ctx)
		cancel()

		_, err := NewStatus(ctx, Options{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if inv := readState(t, work, ".inv"); inv != "0" {
			t.Fatalf("git invoked %s times, want 0", inv)
		}
	})
}

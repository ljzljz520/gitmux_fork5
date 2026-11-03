// Package collect retrieves the status of a Git working tree while guarding
// against branch switches happening in the middle of status collection.
//
// The underlying collection (see github.com/arl/gitstatus) runs several
// independent git commands in sequence. If the working tree moves to another
// branch or commit between two of these commands, the resulting status mixes
// fields gathered from different states, for instance a branch name sampled
// on one branch with the HEAD SHA sampled after a checkout.
//
// Each collection round is therefore bracketed by a read-only probe of:
//
//   - the worktree identity: the absolute git directory and top-level
//     directory, so a swap of the observed worktree is detected as well;
//   - the metadata generation: the current branch and full HEAD SHA, or the
//     detached-HEAD marker together with its SHA.
//
// The collected status is accepted only when the pre and post probes agree
// and the identity-sensitive fields of the status match the observed
// generation. On mismatch the round is discarded and re-collected, up to a
// bounded number of attempts. If the worktree never settles, ErrUnstable is
// returned and no (possibly mixed) status is produced.
//
// All probes use read-only git plumbing with optional locks disabled, so
// collection never takes a write lock on the repository.
package collect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/arl/gitstatus"
)

// DefaultMaxAttempts is the default maximum number of collection rounds.
const DefaultMaxAttempts = 3

// ErrUnstable is returned (wrapped) when the worktree identity or its
// metadata generation keeps changing across collection rounds, so no
// single-generation status could be gathered.
var ErrUnstable = errors.New("worktree state unstable during status collection")

// Options customizes status collection.
type Options struct {
	// MaxAttempts bounds the number of collection rounds. A zero or negative
	// value means DefaultMaxAttempts.
	MaxAttempts int
}

// NewStatus collects the git status of the current working tree. When a
// worktree/metadata generation change is detected mid-collection, it
// re-collects, at most Options.MaxAttempts times. It returns ErrUnstable
// (wrapped) if the tree cannot be observed in one stable state, in which case
// no status is returned and callers must not emit a summary.
//
// The context preserves the cancellation/timeout semantics of the underlying
// git commands: as soon as it is done, collection stops without further
// sampling and the context error is returned (possibly wrapped).
func NewStatus(ctx context.Context, opts Options) (*gitstatus.Status, error) {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}

	var firstGen, lastGen generation

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		before, err := probe(ctx)
		if err != nil {
			return nil, err
		}

		st, err := gitstatus.NewWithContext(ctx)
		if err != nil {
			if ce := ctx.Err(); ce != nil {
				return nil, ce
			}
			return nil, err
		}

		after, err := probe(ctx)
		if err != nil {
			return nil, err
		}

		if attempt == 1 {
			firstGen = before.gen
		}
		lastGen = after.gen

		if before.sameState(after) && after.matches(st) {
			return st, nil
		}
	}

	return nil, fmt.Errorf("%w: branch/HEAD changed while collecting (first=%s last=%s)",
		ErrUnstable, firstGen, lastGen)
}

// snapshot is a read-only observation of a worktree identity and metadata
// generation.
type snapshot struct {
	gitDir string // absolute path of the worktree git directory
	top    string // absolute path of the worktree top-level directory
	gen    generation
}

// sameState reports whether both snapshots describe the same worktree in the
// same metadata generation.
func (s *snapshot) sameState(other *snapshot) bool {
	return s.gitDir == other.gitDir &&
		s.top == other.top &&
		s.gen == other.gen
}

// matches reports whether the identity-sensitive fields of st are consistent
// with the observed generation. This catches a switch that straddles the
// collection even if the two probes happen to agree (e.g. a round-trip).
func (s *snapshot) matches(st *gitstatus.Status) bool {
	g := s.gen
	switch {
	case g.detached:
		if st.IsInitial || !st.IsDetached || st.LocalBranch != "" {
			return false
		}
		return st.HEAD != "" && strings.HasPrefix(g.head, st.HEAD)

	case g.head == "":
		// Symbolic, unborn branch: no commit yet.
		if !st.IsInitial || st.IsDetached || st.HEAD != "" {
			return false
		}
		return st.LocalBranch == g.branch

	default:
		if st.IsInitial || st.IsDetached || st.HEAD == "" {
			return false
		}
		if st.LocalBranch != g.branch {
			return false
		}
		return strings.HasPrefix(g.head, st.HEAD)
	}
}

// generation identifies a point in the branch/HEAD metadata history. It
// changes on every checkout, branch switch or commit landing on HEAD.
type generation struct {
	detached bool
	branch   string // short branch name when attached (including unborn)
	head     string // full HEAD SHA, empty when HEAD is unborn
}

func (g generation) String() string {
	switch {
	case g.detached:
		return "detached@" + g.head
	case g.head == "":
		return "branch:" + g.branch + "@unborn"
	default:
		return "branch:" + g.branch + "@" + g.head
	}
}

// probe takes a read-only, lock-free snapshot of the current worktree.
func probe(ctx context.Context) (*snapshot, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	// Identity and full HEAD in a single invocation. With an unborn HEAD the
	// HEAD argument errors; git still prints the two directory lines.
	out, err := runGit(ctx, "rev-parse", "--absolute-git-dir", "--show-toplevel", "HEAD")

	var gitDir, top, head string
	switch {
	case err == nil:
		lines := strings.Split(out, "\n")
		if len(lines) != 3 || lines[0] == "" || lines[1] == "" || lines[2] == "" {
			return nil, fmt.Errorf("rev-parse: unexpected output %q", out)
		}
		gitDir, top, head = lines[0], lines[1], lines[2]

	default:
		// Probably an unborn HEAD: re-probe identity only.
		out2, err2 := runGit(ctx, "rev-parse", "--absolute-git-dir", "--show-toplevel")
		if err2 != nil {
			// Not the unborn case after all: surface the original error.
			return nil, err
		}
		lines := strings.Split(out2, "\n")
		if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
			return nil, fmt.Errorf("rev-parse: unexpected output %q", out2)
		}
		gitDir, top = lines[0], lines[1]
	}

	// Resolve the symbolic branch (works on unborn branches too). Detached
	// HEAD makes symbolic-ref fail with exit code 1 and no output.
	ref, refErr := runGit(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")

	gen := generation{head: head}
	switch code := exitCode(refErr); {
	case code == 0:
		if ref == "" {
			return nil, errors.New("symbolic-ref: empty branch name")
		}
		gen.branch = ref
	case code == 1 && head != "":
		gen.detached = true
	default:
		return nil, refErr
	}

	return &snapshot{gitDir: gitDir, top: top, gen: gen}, nil
}

// gitEnv is the minimal environment of every git subprocess. It mirrors
// github.com/arl/gitstatus: a fixed locale and, crucially, disabled optional
// locks, so probing never creates index.lock or takes any write lock.
var gitEnv = func() []string {
	env := []string{
		"LC_ALL=C",
		"GIT_OPTIONAL_LOCKS=0",
	}
	if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	return env
}()

// gitError wraps a failed git invocation with its arguments and stderr.
type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	msg := fmt.Sprintf("git %s: %v", strings.Join(e.args, " "), e.err)
	if e.stderr != "" {
		msg += ": " + strings.TrimSpace(e.stderr)
	}
	return msg
}

func (e *gitError) Unwrap() error { return e.err }

// runGit runs a read-only git plumbing command with the lock-free, fixed
// environment and returns its trimmed standard output.
func runGit(ctx context.Context, args ...string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = gitEnv

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Surface context cancellation/timeout as such even though the
		// underlying process reports a signal.
		if ce := ctx.Err(); ce != nil {
			return "", ce
		}
		return "", &gitError{args: args, stderr: stderr.String(), err: err}
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// exitCode returns the process exit code carried by err: 0 when err is nil,
// the actual code on exit failure, or -1 for another error (e.g. context
// cancellation).
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

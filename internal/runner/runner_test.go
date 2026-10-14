package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type collector struct {
	mu    sync.Mutex
	lines []string
}

func (c *collector) out(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *collector) get() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

var testEnv = []string{"PATH=/usr/bin:/bin"}

func run(t *testing.T, ctx context.Context, script string, opts Options) (Result, []string) {
	t.Helper()
	var c collector
	res, err := Run(ctx, Step{Script: script, Dir: t.TempDir(), Env: testEnv, Label: "[1/1]"}, opts, c.out)
	if err != nil {
		t.Fatal(err)
	}
	return res, c.get()
}

func TestRunOutputAndExitCode(t *testing.T) {
	res, lines := run(t, context.Background(), "echo one\necho two >&2\nprintf 'no newline'", Options{})
	if res.ExitCode != 0 || res.TimedOut {
		t.Errorf("result = %+v", res)
	}
	if want := []string{"one", "two", "no newline"}; !reflect.DeepEqual(lines, want) {
		t.Errorf("lines = %q, want %q", lines, want)
	}

	res, _ = run(t, context.Background(), "exit 3", Options{})
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestRunStopsOnFirstError(t *testing.T) {
	res, lines := run(t, context.Background(), "false\necho after", Options{})
	if res.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1", res.ExitCode)
	}
	if len(lines) != 0 {
		t.Errorf("lines = %q, want none", lines)
	}
}

func TestRunEnvAndDir(t *testing.T) {
	dir := t.TempDir()
	var c collector
	step := Step{
		Script: `echo "$FOO"; echo "${HOME:-unset}"; pwd -P`,
		Dir:    dir,
		Env:    append([]string{"FOO=bar baz"}, testEnv...),
	}
	if _, err := Run(context.Background(), step, Options{}, c.out); err != nil {
		t.Fatal(err)
	}
	realDir, _ := filepath.EvalSymlinks(dir)
	if want := []string{"bar baz", "unset", realDir}; !reflect.DeepEqual(c.get(), want) {
		t.Errorf("lines = %q, want %q", c.get(), want)
	}
}

func TestRunKeepalive(t *testing.T) {
	_, lines := run(t, context.Background(), "sleep 0.4", Options{Keepalive: 100 * time.Millisecond})
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "==> still running [1/1] (") {
		t.Errorf("lines = %q, want keepalive messages", lines)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	var c collector
	// The background child must die with the group, so the marker is never written.
	step := Step{Script: "(sleep 1; touch " + marker + ") &\nsleep 10", Dir: dir, Env: testEnv}
	res, err := Run(ctx, step, Options{KillGrace: 100 * time.Millisecond}, c.out)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Errorf("result = %+v, want TimedOut", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run took %v", elapsed)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("background child survived the timeout")
	}
}

func TestRunTimeoutEscalatesToSIGKILL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	res, _ := run(t, ctx, "trap '' TERM\nsleep 10", Options{KillGrace: 200 * time.Millisecond})
	if !res.TimedOut {
		t.Errorf("result = %+v, want TimedOut", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run took %v, SIGKILL was not sent", elapsed)
	}
}

func TestRunDoesNotWaitForDetachedChildren(t *testing.T) {
	start := time.Now()
	res, _ := run(t, context.Background(), "sleep 5 &\necho started", Options{})
	if res.ExitCode != 0 {
		t.Errorf("result = %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Run took %v, it waited for a child holding stdout", elapsed)
	}
}

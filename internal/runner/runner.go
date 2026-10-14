// Package runner executes a single deploy step as `/bin/sh -e -c <script>` in its
// own process group, streaming merged stdout/stderr line by line.
package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Step is one script to execute.
type Step struct {
	Script string
	Dir    string
	Env    []string // complete environment; nothing is inherited from the daemon
	Label  string   // shown in keepalive messages, e.g. "[2/3]"
}

// Options tune timing; zero values mean defaults.
type Options struct {
	Keepalive time.Duration // print "still running" after this much silence (default 30s)
	KillGrace time.Duration // time between SIGTERM and SIGKILL on cancellation (default 10s)
}

// Result of a step.
type Result struct {
	ExitCode int  // 128+signal if the process was killed by a signal
	TimedOut bool // ctx was done before the step finished
}

// drainTimeout bounds how long output is read after the shell exits; a detached
// background child may keep the pipe open forever.
const drainTimeout = time.Second

// Run executes the step. out is called sequentially, never after Run returns.
// When ctx is done the process group gets SIGTERM, then SIGKILL after KillGrace.
func Run(ctx context.Context, s Step, opts Options, out func(line string)) (Result, error) {
	if opts.Keepalive == 0 {
		opts.Keepalive = 30 * time.Second
	}
	if opts.KillGrace == 0 {
		opts.KillGrace = 10 * time.Second
	}
	if ctx.Err() != nil {
		return Result{TimedOut: true}, nil
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return Result{}, err
	}
	defer pr.Close()
	cmd := exec.Command("/bin/sh", "-e", "-c", s.Script)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.Stdout = pw
	cmd.Stderr = pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = cmd.Start()
	pw.Close()
	if err != nil {
		return Result{}, fmt.Errorf("start step: %w", err)
	}

	stop := make(chan struct{})
	defer close(stop)
	lines := make(chan string)
	go readLines(pr, lines, stop)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	start := time.Now()
	idle := time.NewTimer(opts.Keepalive)
	defer idle.Stop()
	var killTimer <-chan time.Time
	done := ctx.Done()
	var res Result

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			out(line)
			idle.Reset(opts.Keepalive)
		case <-idle.C:
			out(fmt.Sprintf("==> still running %s (%s)", s.Label, time.Since(start).Round(time.Second)))
			idle.Reset(opts.Keepalive)
		case <-done:
			res.TimedOut = true
			done = nil
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			killTimer = time.After(opts.KillGrace)
		case <-killTimer:
			killTimer = nil
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		case err := <-waitCh:
			if res.TimedOut {
				// Finish off anything in the group that ignored SIGTERM.
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			res.ExitCode = exitCode(err)
			drain(lines, out)
			return res, nil
		}
	}
}

func readLines(f *os.File, lines chan<- string, stop <-chan struct{}) {
	defer close(lines)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			select {
			case lines <- strings.TrimRight(line, "\r\n"):
			case <-stop:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func drain(lines <-chan string, out func(string)) {
	if lines == nil {
		return
	}
	timeout := time.After(drainTimeout)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				return
			}
			out(line)
		case <-timeout:
			return
		}
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

package tasks

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Worker runs queued tasks as `<Executable> <kind> <args...>` subprocesses.
type Worker struct {
	Store       *Store
	Executable  string // the devloop binary
	Concurrency int
	Poll        time.Duration
	// Env is added to each task's environment; DEVLOOP_TASK_ID is always set.
	Env []string
}

// Run processes the queue until ctx is canceled. Tasks still running at that
// point are interrupted and marked canceled.
func (w *Worker) Run(ctx context.Context) error {
	if w.Concurrency < 1 {
		w.Concurrency = 1
	}
	if w.Poll == 0 {
		w.Poll = time.Second
	}
	if err := w.Store.RecoverOrphans(ctx, processAlive); err != nil {
		return err
	}

	slots := make(chan struct{}, w.Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	tick := time.NewTicker(w.Poll)
	defer tick.Stop()
	for {
		// Fill free slots with queued tasks.
		for len(slots) < cap(slots) {
			t, err := w.Store.Claim(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			if t == nil {
				break
			}
			slots <- struct{}{}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				w.runTask(ctx, t)
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

func (w *Worker) runTask(ctx context.Context, t *Task) {
	// Use a background context for bookkeeping so a shutdown still records the outcome.
	bg := context.Background()
	status, summary, errMsg := w.execute(ctx, t)
	if err := w.Store.Finish(bg, t.ID, status, summary, errMsg); err != nil {
		fmt.Fprintf(os.Stderr, "task %d: recording result: %v\n", t.ID, err)
	}
}

func (w *Worker) execute(ctx context.Context, t *Task) (Status, string, string) {
	logFile, err := os.Create(w.Store.LogPath(t.ID))
	if err != nil {
		return Failed, "", err.Error()
	}
	defer logFile.Close()
	outFile, err := os.Create(w.Store.OutputPath(t.ID))
	if err != nil {
		return Failed, "", err.Error()
	}
	defer outFile.Close()

	fmt.Fprintf(logFile, "$ cd %s && devloop %s\n", t.Dir, t.Title())
	cmd := exec.Command(w.Executable, append([]string{t.Kind}, t.Args...)...)
	cmd.Dir = t.Dir
	cmd.Env = append(append(os.Environ(), w.Env...), fmt.Sprintf("DEVLOOP_TASK_ID=%d", t.ID))
	cmd.Stdin = nil // tasks never prompt; e.g. implement -push without -yes won't push
	cmd.Stdout = io.MultiWriter(outFile, logFile)
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // so we can stop the whole tree
	if err := cmd.Start(); err != nil {
		return Failed, "", err.Error()
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	check := time.NewTicker(time.Second)
	defer check.Stop()
	canceled := false
	for {
		select {
		case err := <-done:
			summary := summarize(w.Store.OutputPath(t.ID))
			switch {
			case canceled:
				return Canceled, summary, "canceled"
			case err != nil:
				return Failed, summary, lastError(w.Store.LogPath(t.ID), err)
			default:
				return Succeeded, summary, ""
			}
		case <-ctx.Done():
			if !canceled {
				canceled = true
				stop(cmd)
			}
		case <-check.C:
			if !canceled && w.Store.IsCancelRequested(context.Background(), t.ID) {
				canceled = true
				stop(cmd)
			}
		}
	}
}

// stop interrupts the task's process group, and kills it if it hasn't exited in 10s.
func stop(cmd *exec.Cmd) {
	pgid := -cmd.Process.Pid
	syscall.Kill(pgid, syscall.SIGINT)
	go func() {
		time.Sleep(10 * time.Second)
		syscall.Kill(pgid, syscall.SIGKILL)
	}()
}

// summarize returns the first heading or line of a task's output.
func summarize(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(strings.TrimLeft(sc.Text(), "#")); line != "" {
			return truncate(line, 200)
		}
	}
	return ""
}

// lastError returns the command's error message from the log: the last line
// starting with "error: " (devloop's error prefix), else the last non-empty line.
func lastError(path string, fallback error) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return fallback.Error()
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if rest, ok := strings.CutPrefix(lines[i], "error: "); ok {
			return truncate(strings.TrimSpace(rest), 300)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return truncate(l, 300)
		}
	}
	return fallback.Error()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

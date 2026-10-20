package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/enricoconvento98/devloop/internal/tasks"
	"github.com/enricoconvento98/devloop/internal/tui"
	"github.com/enricoconvento98/devloop/internal/web"
)

func openStore() (*tasks.Store, error) {
	home, err := tasks.Home()
	if err != nil {
		return nil, err
	}
	return tasks.Open(home)
}

func newWorker(store *tasks.Store, concurrency int) (*tasks.Worker, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return &tasks.Worker{Store: store, Executable: exe, Concurrency: concurrency}, nil
}

// uiCmd opens the TUI; it's what `devloop` with no arguments runs in a terminal.
func uiCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	noWorker := fs.Bool("no-worker", false, "only monitor; don't run queued tasks in this window")
	concurrency := fs.Int("concurrency", 2, "tasks to run at once")
	parseFlags(fs, args)
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	var w *tasks.Worker
	if !*noWorker {
		if w, err = newWorker(store, *concurrency); err != nil {
			return err
		}
	}
	cwd, _ := os.Getwd()
	return tui.Run(ctx, store, cwd, w)
}

func serveCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "address to listen on (loopback only)")
	noWorker := fs.Bool("no-worker", false, "only serve the dashboard; don't run queued tasks")
	concurrency := fs.Int("concurrency", 2, "tasks to run at once")
	parseFlags(fs, args)

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing to listen on %s: the dashboard can run commands with your credentials, so it only listens on loopback", *addr)
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerDone := make(chan error, 1)
	if *noWorker {
		close(workerDone)
	} else {
		w, err := newWorker(store, *concurrency)
		if err != nil {
			return err
		}
		go func() { workerDone <- w.Run(ctx) }()
	}
	cwd, _ := os.Getwd()
	fmt.Fprintf(os.Stderr, "devloop dashboard on http://%s (tasks run in %s; ctrl+c to stop)\n", *addr, cwd)
	err = web.New(store, cwd).ListenAndServe(ctx, *addr)
	cancel()
	if werr := <-workerDone; err == nil {
		err = werr
	}
	return err
}

func workerCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ExitOnError)
	concurrency := fs.Int("concurrency", 2, "tasks to run at once")
	parseFlags(fs, args)
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	w, err := newWorker(store, *concurrency)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "worker running %d at a time; ctrl+c to stop\n", *concurrency)
	return w.Run(ctx)
}

// addCmd queues a task: `devloop add review owner/repo 12`.
func addCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: devloop add <%s> [args...]", strings.Join(tasks.Kinds, "|"))
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	cwd, _ := os.Getwd()
	id, err := store.Add(ctx, args[0], args[1:], cwd)
	if err != nil {
		return err
	}
	fmt.Println(id)
	fmt.Fprintf(os.Stderr, "queued task %d; watch it with `devloop` or `devloop logs -f %d`\n", id, id)
	return nil
}

func tasksCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tasks", flag.ExitOnError)
	limit := fs.Int("n", 20, "number of tasks to show")
	watch := fs.Bool("w", false, "refresh every 2s")
	parseFlags(fs, args)
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	for {
		list, err := store.List(ctx, *limit)
		if err != nil {
			return err
		}
		if *watch {
			fmt.Print("\033[H\033[2J") // clear screen
		}
		printTasks(os.Stdout, list)
		if !*watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func printTasks(w io.Writer, list []tasks.Task) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tTIME\tTASK\tRESULT")
	for _, t := range list {
		note := t.Summary
		if t.Error != "" {
			note = t.Error
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", t.ID, t.Status, t.Elapsed(), fitText(t.Title(), 60), fitText(note, 70))
	}
	tw.Flush()
	if len(list) == 0 {
		fmt.Fprintln(w, "no tasks yet; queue one with `devloop add review owner/repo 12`")
	}
}

func fitText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func logsCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	follow := fs.Bool("f", false, "keep printing output until the task finishes")
	parseFlags(fs, args)
	id, err := taskArg(fs.Args())
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.Get(ctx, id); err != nil {
		return err
	}

	var offset int64
	for {
		if f, err := os.Open(store.LogPath(id)); err == nil {
			f.Seek(offset, io.SeekStart)
			n, _ := io.Copy(os.Stdout, f)
			offset += n
			f.Close()
		}
		t, err := store.Get(ctx, id)
		if err != nil {
			return err
		}
		if !*follow || t.Status.Done() {
			if t.Status.Done() && *follow {
				fmt.Fprintf(os.Stderr, "[task %d %s]\n", id, t.Status)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func cancelCmd(ctx context.Context, args []string) error {
	id, err := taskArg(args)
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	return store.Cancel(ctx, id)
}

func retryCmd(ctx context.Context, args []string) error {
	id, err := taskArg(args)
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	newID, err := store.Retry(ctx, id)
	if err != nil {
		return err
	}
	fmt.Println(newID)
	return nil
}

func taskArg(args []string) (int64, error) {
	if len(args) != 1 {
		return 0, errors.New("expected one task id")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad task id %q", args[0])
	}
	return id, nil
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// parseFlags parses args allowing flags after positional arguments
// (`triage owner/repo -limit 5`), which the flag package alone stops at.
func parseFlags(fs *flag.FlagSet, args []string) {
	var positional []string
	for {
		fs.Parse(args)
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		if args[0] == "--" {
			positional = append(positional, args[1:]...)
			break
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
	fs.Parse(append([]string{"--"}, positional...))
}

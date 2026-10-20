package tui

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/enricoconvento98/devloop/internal/tasks"
)

func seeded(t *testing.T) (*tasks.Store, Model) {
	t.Helper()
	store, err := tasks.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	done, _ := store.Add(ctx, "review", []string{"acme/shop", "12"}, "")
	store.Claim(ctx)
	store.Finish(ctx, done, tasks.Succeeded, "Review of #12 — request changes", "")
	store.Add(ctx, "implement", []string{"-plan", "plan-42.json", "-test", "go test ./..."}, "")
	os.WriteFile(store.LogPath(done), []byte("$ devloop review acme/shop 12\n## Review of #12\nline two\n"), 0o644)

	m := New(store, "/work", true)
	var model tea.Model = m
	model, _ = model.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	model, _ = model.Update(m.loadTasks())
	return store, model.(Model)
}

func press(m Model, keys ...string) Model {
	var model tea.Model = m
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		var cmd tea.Cmd
		model, cmd = model.Update(msg)
		// Run immediate follow-up commands (loads, cancel/retry) like the runtime
		// would; skip timed ones such as the cursor blink.
		if out := runQuick(cmd); out != nil {
			if _, isBatch := out.(tea.BatchMsg); !isBatch {
				model, _ = model.Update(out)
			}
		}
	}
	return model.(Model)
}

func runQuick(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case msg := <-ch:
		return msg
	case <-time.After(50 * time.Millisecond):
		return nil
	}
}

func TestListView(t *testing.T) {
	_, m := seeded(t)
	view := m.View()
	for _, want := range []string{"devloop", "1 queued", "1 done", "worker on", `implement -plan plan-42.json -test "go test ./..."`, "review acme/shop 12", "n new task"} {
		if !strings.Contains(view, want) {
			t.Errorf("view missing %q:\n%s", want, view)
		}
	}
	if os.Getenv("SHOW_TUI") != "" {
		t.Log("\n" + view)
	}
}

func TestQueueFromInput(t *testing.T) {
	store, m := seeded(t)
	m = press(m, "n")
	for _, r := range "triage acme/shop -limit 5" {
		m = press(m, string(r))
	}
	m = press(m, "enter")
	if m.mode != listMode || !strings.Contains(m.flash, "queued task 3") {
		t.Fatalf("mode=%v flash=%q", m.mode, m.flash)
	}
	got, err := store.Get(context.Background(), 3)
	if err != nil || got.Kind != "triage" || strings.Join(got.Args, " ") != "acme/shop -limit 5" || got.Dir != "/work" {
		t.Fatalf("queued task = %+v, %v", got, err)
	}

	m = press(m, "n")
	for _, r := range "rm -rf /" {
		m = press(m, string(r))
	}
	m = press(m, "enter")
	if m.mode != inputMode || !strings.Contains(m.flash, "unknown command") {
		t.Fatalf("bad command accepted: mode=%v flash=%q", m.mode, m.flash)
	}
}

func TestLogViewAndActions(t *testing.T) {
	store, m := seeded(t)
	m = press(m, "down", "enter") // second row: the finished review
	if m.mode != logMode || m.logID != 1 {
		t.Fatalf("mode=%v logID=%d", m.mode, m.logID)
	}
	if view := m.View(); !strings.Contains(view, "line two") || !strings.Contains(view, "succeeded") {
		t.Errorf("log view:\n%s", view)
	}
	m = press(m, "r") // retry from the log view
	if !strings.Contains(m.flash, "retry of 1") {
		t.Errorf("flash = %q", m.flash)
	}
	m = press(m, "esc")
	if m.mode != listMode {
		t.Fatal("esc did not return to the list")
	}
	m = press(m, "c") // cursor still on the finished review
	if !strings.Contains(m.flash, "already succeeded") {
		t.Errorf("canceling a finished task: flash = %q", m.flash)
	}
	m = press(m, "k", "c") // up to the queued implement task
	if got, _ := store.Get(context.Background(), 2); got.Status != tasks.Canceled {
		t.Errorf("task 2 = %s, want canceled", got.Status)
	}
}

func TestQuitAsksWhenTasksRun(t *testing.T) {
	store, m := seeded(t)
	store.Claim(context.Background()) // implement task now running
	var model tea.Model = m
	model, _ = model.Update(m.loadTasks())
	m = press(model.(Model), "q")
	if m.mode != quitConfirmMode || !strings.Contains(m.View(), "Quit? (y/N)") {
		t.Fatalf("no quit confirmation: mode=%v", m.mode)
	}
	m = press(m, "n")
	if m.mode != listMode {
		t.Fatal("declining quit did not return to the list")
	}
}

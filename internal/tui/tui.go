// Package tui is devloop's interactive terminal app: a live task list with
// logs, and keys to queue, cancel and retry tasks.
package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/enricoconvento98/devloop/internal/tasks"
)

type mode int

const (
	listMode mode = iota
	inputMode
	logMode
	quitConfirmMode
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	mutedStyle  = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "245", Dark: "243"})
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "160", Dark: "203"})
	cursorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "111"})
	statusStyle = map[tasks.Status]lipgloss.Style{
		tasks.Queued:          mutedStyle,
		tasks.Running:         lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "111"}),
		tasks.CancelRequested: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "136", Dark: "221"}),
		tasks.Succeeded:       lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "78"}),
		tasks.Failed:          errStyle,
		tasks.Canceled:        lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "136", Dark: "221"}),
	}
	statusIcon = map[tasks.Status]string{
		tasks.Queued: "○", tasks.Running: "●", tasks.CancelRequested: "◌",
		tasks.Succeeded: "✓", tasks.Failed: "✗", tasks.Canceled: "–",
	}
)

// Model is the Bubble Tea model. Exported for tests.
type Model struct {
	store   *tasks.Store
	dir     string // where queued tasks run
	worker  bool   // whether this process runs a worker
	mode    mode
	tasks   []tasks.Task
	cursor  int
	logID   int64
	follow  bool
	input   textinput.Model
	logView viewport.Model
	flash   string
	flashOK bool
	width   int
	height  int
}

func New(store *tasks.Store, dir string, runsWorker bool) Model {
	in := textinput.New()
	in.Placeholder = `review owner/repo 12   ·   implement -plan plan-42.json -test "go test ./..."`
	in.Prompt = "devloop "
	in.CharLimit = 2000
	return Model{store: store, dir: dir, worker: runsWorker, input: in, logView: viewport.New(80, 20), follow: true}
}

type tickMsg time.Time
type tasksMsg []tasks.Task
type logMsg string
type flashMsg struct {
	text string
	ok   bool
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) loadTasks() tea.Msg {
	list, err := m.store.List(context.Background(), 200)
	if err != nil {
		return flashMsg{text: err.Error()}
	}
	return tasksMsg(list)
}

func (m Model) loadLog() tea.Msg {
	b, err := os.ReadFile(m.store.LogPath(m.logID))
	if err != nil {
		return logMsg("(no output yet)")
	}
	return logMsg(b)
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.loadTasks, tick()) }

func (m Model) selected() *tasks.Task {
	if m.cursor < 0 || m.cursor >= len(m.tasks) {
		return nil
	}
	return &m.tasks[m.cursor]
}

func (m Model) running() int {
	n := 0
	for _, t := range m.tasks {
		if t.Status == tasks.Running || t.Status == tasks.CancelRequested {
			n++
		}
	}
	return n
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.logView.Width, m.logView.Height = msg.Width, max(msg.Height-4, 3)
		m.input.Width = max(msg.Width-10, 10)
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{m.loadTasks, tick()}
		if m.mode == logMode {
			cmds = append(cmds, m.loadLog)
		}
		return m, tea.Batch(cmds...)
	case tasksMsg:
		var selID int64
		if t := m.selected(); t != nil {
			selID = t.ID
		}
		m.tasks = msg
		m.cursor = min(m.cursor, max(len(m.tasks)-1, 0))
		for i, t := range m.tasks { // keep the cursor on the same task as new ones arrive
			if t.ID == selID {
				m.cursor = i
			}
		}
		return m, nil
	case logMsg:
		m.logView.SetContent(string(msg))
		if m.follow {
			m.logView.GotoBottom()
		}
		return m, nil
	case flashMsg:
		m.flash, m.flashOK = msg.text, msg.ok
		return m, nil
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case inputMode:
		switch msg.String() {
		case "esc":
			m.mode = listMode
			m.input.Blur()
			return m, nil
		case "enter":
			line := m.input.Value()
			kind, args, err := tasks.Parse(line)
			if err != nil {
				m.flash, m.flashOK = err.Error(), false
				return m, nil
			}
			id, err := m.store.Add(context.Background(), kind, args, m.dir)
			if err != nil {
				m.flash, m.flashOK = err.Error(), false
				return m, nil
			}
			m.input.Reset()
			m.input.Blur()
			m.mode = listMode
			m.cursor = 0
			m.flash, m.flashOK = fmt.Sprintf("queued task %d", id), true
			return m, m.loadTasks
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return m, cmd

	case logMode:
		switch msg.String() {
		case "esc", "q", "left", "h":
			m.mode = listMode
			return m, nil
		case "f":
			m.follow = !m.follow
			if m.follow {
				m.logView.GotoBottom()
			}
			return m, nil
		case "c":
			return m, m.cancel(m.logID)
		case "r":
			return m, m.retry(m.logID)
		}
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		if m.logView.AtBottom() {
			m.follow = true
		} else if msg.String() == "up" || msg.String() == "k" || msg.String() == "pgup" {
			m.follow = false
		}
		return m, cmd

	case quitConfirmMode:
		if msg.String() == "y" {
			return m, tea.Quit
		}
		m.mode = listMode
		return m, nil
	}

	// listMode
	switch msg.String() {
	case "q", "ctrl+c":
		if m.worker && m.running() > 0 {
			m.mode = quitConfirmMode
			return m, nil
		}
		return m, tea.Quit
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, max(len(m.tasks)-1, 0))
	case "n", "a":
		m.mode = inputMode
		m.flash = ""
		return m, m.input.Focus()
	case "enter", "l", "right":
		if t := m.selected(); t != nil {
			m.mode, m.logID, m.follow = logMode, t.ID, true
			m.logView.SetContent("")
			return m, m.loadLog
		}
	case "c":
		if t := m.selected(); t != nil {
			return m, m.cancel(t.ID)
		}
	case "r":
		if t := m.selected(); t != nil {
			return m, m.retry(t.ID)
		}
	}
	return m, nil
}

func (m Model) cancel(id int64) tea.Cmd {
	return func() tea.Msg {
		if err := m.store.Cancel(context.Background(), id); err != nil {
			return flashMsg{text: err.Error()}
		}
		return flashMsg{text: fmt.Sprintf("canceling task %d", id), ok: true}
	}
}

func (m Model) retry(id int64) tea.Cmd {
	return func() tea.Msg {
		newID, err := m.store.Retry(context.Background(), id)
		if err != nil {
			return flashMsg{text: err.Error()}
		}
		return flashMsg{text: fmt.Sprintf("queued task %d (retry of %d)", newID, id), ok: true}
	}
}

func (m Model) View() string {
	if m.mode == logMode {
		return m.logScreen()
	}
	var b strings.Builder
	b.WriteString(m.header() + "\n\n")

	if len(m.tasks) == 0 {
		b.WriteString(mutedStyle.Render("  No tasks yet. Press n to queue one, e.g. review owner/repo 12") + "\n")
	}
	// Show a window of rows around the cursor that fits the screen.
	rows := max(m.height-8, 5)
	start := max(0, min(m.cursor-rows/2, len(m.tasks)-rows))
	end := min(len(m.tasks), start+rows)
	width := max(m.width, 60)
	for i := start; i < end; i++ {
		t := m.tasks[i]
		pointer := "  "
		if i == m.cursor {
			pointer = cursorStyle.Render("› ")
		}
		st := statusStyle[t.Status]
		status := st.Render(fmt.Sprintf("%s %-10s", statusIcon[t.Status], strings.ReplaceAll(string(t.Status), "_", " ")))
		dur := t.Elapsed()
		note := t.Summary
		if t.Error != "" {
			note = errStyle.Render(t.Error)
		}
		line := fmt.Sprintf("%s%4d  %s  %-7s  %s", pointer, t.ID, status, dur, fit(t.Title(), width-36))
		b.WriteString(line + "\n")
		if note != "" && i == m.cursor {
			b.WriteString(mutedStyle.Render("        ") + fit(note, width-8) + "\n")
		}
	}

	b.WriteString("\n")
	switch m.mode {
	case inputMode:
		b.WriteString(m.input.View() + "\n")
		b.WriteString(mutedStyle.Render("enter queue · esc cancel · commands: "+strings.Join(tasks.Kinds, ", ")) + "\n")
	case quitConfirmMode:
		b.WriteString(errStyle.Render(fmt.Sprintf("%d task(s) running in this window will be stopped. Quit? (y/N)", m.running())) + "\n")
	default:
		if m.flash != "" {
			style := errStyle
			if m.flashOK {
				style = mutedStyle
			}
			b.WriteString(style.Render(m.flash) + "\n")
		}
		b.WriteString(mutedStyle.Render("↑/↓ select · enter logs · n new task · c cancel · r retry · q quit") + "\n")
	}
	return b.String()
}

func (m Model) header() string {
	count := func(s tasks.Status) int {
		n := 0
		for _, t := range m.tasks {
			if t.Status == s {
				n++
			}
		}
		return n
	}
	worker := "worker off (run `devloop worker`)"
	if m.worker {
		worker = "worker on"
	}
	return titleStyle.Render("devloop") + mutedStyle.Render(fmt.Sprintf("  %d running · %d queued · %d done · %d failed · %s",
		m.running(), count(tasks.Queued), count(tasks.Succeeded), count(tasks.Failed), worker))
}

func (m Model) logScreen() string {
	head := fmt.Sprintf("task %d", m.logID)
	for _, t := range m.tasks {
		if t.ID == m.logID {
			head = fmt.Sprintf("%s %d  %s  %s", statusIcon[t.Status], t.ID, statusStyle[t.Status].Render(string(t.Status)), t.Title())
		}
	}
	follow := "follow on"
	if !m.follow {
		follow = "follow off"
	}
	help := mutedStyle.Render(fmt.Sprintf("esc back · ↑/↓ pgup/pgdn scroll · f %s · c cancel · r retry", follow))
	return titleStyle.Render(fit(head, max(m.width, 40))) + "\n" + m.logView.View() + "\n" + help
}

// fit truncates s to n display columns.
func fit(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if n <= 1 || lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r)) > n-1 {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// Run starts the TUI, with a worker in the background when worker is non-nil.
func Run(ctx context.Context, store *tasks.Store, dir string, worker *tasks.Worker) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	workerDone := make(chan error, 1)
	if worker != nil {
		go func() { workerDone <- worker.Run(ctx) }()
	} else {
		close(workerDone)
	}
	_, err := tea.NewProgram(New(store, dir, worker != nil), tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	cancel()
	if werr := <-workerDone; werr != nil && err == nil {
		err = werr
	}
	if err != nil && ctx.Err() != nil && strings.Contains(err.Error(), "context canceled") {
		return nil
	}
	return err
}

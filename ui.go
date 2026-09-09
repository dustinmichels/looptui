package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const maxOutputLines = 5000

type spinMsg time.Time
type nextRunMsg struct{}

type editorFinishedMsg struct {
	err error
}
type modelDetectedMsg struct {
	model string
}

func detectDefaultModelCmd(ompPath string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, ompPath, "config", "get", "modelRoles")
		out, err := cmd.Output()
		if err != nil {
			return nil
		}
		var roles map[string]string
		if err := json.Unmarshal(out, &roles); err == nil {
			if def, ok := roles["default"]; ok && def != "" {
				return modelDetectedMsg{model: def}
			}
		}
		return nil
	}
}


type uiMode byte

const (
	modeRunner uiMode = iota
	modeReview
	modeInput
)

type model struct {
	cfg config
	doc document

	width  int
	height int

	mode uiMode
	modelName string


	current  section
	run      *agentRun
	starting bool
	running  bool
	waiting  bool
	autoRun  bool

	iteration       int
	stall           int
	exitCode        int
	status          string
	spinFrame       int
	runStartPending int

	output           []string
	partial          string
	hasPartial       bool
	scrollFromBottom int

	blockedSelected int
	textInput       textinput.Model
	inputAction     taskState
}

var (
	green     = lipgloss.Color("#73C991")
	yellow    = lipgloss.Color("#E5C07B")
	red       = lipgloss.Color("#E06C75")
	blue      = lipgloss.Color("#61AFEF")
	muted     = lipgloss.Color("#7F848E")
	panelEdge = lipgloss.Color("#3E4451")

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(green)
	mutedStyle  = lipgloss.NewStyle().Foreground(muted)
	activeStyle = lipgloss.NewStyle().Bold(true).Foreground(blue)
	doneStyle   = lipgloss.NewStyle().Foreground(green)
	warnStyle   = lipgloss.NewStyle().Foreground(yellow)
	errorStyle  = lipgloss.NewStyle().Foreground(red)
)

func newModel(cfg config) (model, error) {
	doc, err := readDocument(cfg.document)
	if err != nil {
		return model{}, err
	}
	ti := textinput.New()
	ti.Placeholder = "Type answer / response for the agent and press Enter..."
	ti.CharLimit = 1000

	m := model{
		cfg:       cfg,
		doc:       doc,
		width:     100,
		height:    30,
		autoRun:   true,
		status:    "Ready",
		textInput: ti,
		modelName: cfg.model,
	}
	m.setTerminalState()
	if doc.pending > 0 {
		m.prepareRun()
	}
	return m, nil
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.starting {
		cmds = append(cmds, startAgent(m.cfg, m.current), spinTick())
	}
	if m.modelName == "" && m.cfg.ompPath != "" {
		cmds = append(cmds, detectDefaultModelCmd(m.cfg.ompPath))
	}
	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}
	return nil
}

func (m model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case modelDetectedMsg:
		if m.modelName == "" && msg.model != "" {
			m.modelName = msg.model
		}
		return m, nil

	case editorFinishedMsg:
		if msg.err != nil {
			m.status = "Editor error: " + msg.err.Error()
		}
		_ = m.reloadDocument()
		if m.doc.blocked == 0 && (m.mode == modeReview || m.mode == modeInput) {
			m.mode = modeRunner
		}
		if m.autoRun && !m.running && !m.starting && m.doc.pending > 0 {
			return m.startNextRun()
		}
		m.setTerminalState()
		return m, nil

	case tea.KeyMsg:
		if m.mode == modeInput {
			switch msg.String() {
			case "enter":
				if m.running || m.starting {
					m.status = "Agent is active; press space to pause before editing tasks"
					m.mode = modeReview
					m.textInput.Blur()
					return m, nil
				}
				answer := strings.TrimSpace(m.textInput.Value())
				blocked := m.doc.blockedTasks()
				if len(blocked) > 0 && m.blockedSelected >= 0 && m.blockedSelected < len(blocked) {
					task := blocked[m.blockedSelected]
					if err := updateTaskInFile(m.cfg.document, task, m.inputAction, answer); err != nil {
						m.status = err.Error()
					} else {
						m.status = fmt.Sprintf("Updated task at line %d", task.line)
					}
					_ = m.reloadDocument()
				}
				m.mode = modeReview
				m.textInput.Blur()
				if m.doc.blocked == 0 {
					m.mode = modeRunner
				}
				if m.autoRun && !m.running && !m.starting && m.doc.pending > 0 {
					return m.startNextRun()
				}
				m.setTerminalState()
				return m, nil
			case "esc":
				m.mode = modeReview
				m.textInput.Blur()
				return m, nil
			default:
				var cmd tea.Cmd
				m.textInput, cmd = m.textInput.Update(msg)
				return m, cmd
			}
		}

		if m.mode == modeReview {
			blocked := m.doc.blockedTasks()
			switch msg.String() {
			case "q", "ctrl+c":
				if m.run != nil {
					m.run.cancel()
					m.exitCode = 130
				}
				return m, tea.Quit
			case "esc", "i", "tab":
				m.mode = modeRunner
				return m, nil
			case "up", "k":
				if m.blockedSelected > 0 {
					m.blockedSelected--
				}
				return m, nil
			case "down", "j":
				if m.blockedSelected < len(blocked)-1 {
					m.blockedSelected++
				}
				return m, nil
			case "a", "enter":
				if m.running || m.starting {
					m.status = "Agent is active; press space to pause before editing tasks"
					return m, nil
				}
				if len(blocked) > 0 {
					m.mode = modeInput
					m.inputAction = taskPending
					m.textInput.SetValue("")
					m.textInput.Focus()
					return m, textinput.Blink
				}
				return m, nil
			case "x":
				if m.running || m.starting {
					m.status = "Agent is active; press space to pause before editing tasks"
					return m, nil
				}
				if len(blocked) > 0 && m.blockedSelected >= 0 && m.blockedSelected < len(blocked) {
					task := blocked[m.blockedSelected]
					if err := updateTaskInFile(m.cfg.document, task, taskDone, ""); err != nil {
						m.status = err.Error()
					} else {
						m.status = fmt.Sprintf("Marked line %d as done", task.line)
					}
					_ = m.reloadDocument()
					blocked = m.doc.blockedTasks()
					if m.blockedSelected >= len(blocked) && len(blocked) > 0 {
						m.blockedSelected = len(blocked) - 1
					}
					if len(blocked) == 0 {
						m.mode = modeRunner
					}
					if m.autoRun && !m.running && !m.starting && m.doc.pending > 0 {
						return m.startNextRun()
					}
					m.setTerminalState()
				}
				return m, nil
			case "u":
				if m.running || m.starting {
					m.status = "Agent is active; press space to pause before editing tasks"
					return m, nil
				}
				if len(blocked) > 0 && m.blockedSelected >= 0 && m.blockedSelected < len(blocked) {
					task := blocked[m.blockedSelected]
					if err := updateTaskInFile(m.cfg.document, task, taskPending, ""); err != nil {
						m.status = err.Error()
					} else {
						m.status = fmt.Sprintf("Unblocked line %d", task.line)
					}
					_ = m.reloadDocument()
					blocked = m.doc.blockedTasks()
					if m.blockedSelected >= len(blocked) && len(blocked) > 0 {
						m.blockedSelected = len(blocked) - 1
					}
					if len(blocked) == 0 {
						m.mode = modeRunner
					}
					if m.autoRun && !m.running && !m.starting && m.doc.pending > 0 {
						return m.startNextRun()
					}
					m.setTerminalState()
				}
				return m, nil
			case "e":
				if m.running || m.starting {
					m.status = "Agent is active; press space to pause before opening editor"
					return m, nil
				}
				if len(blocked) > 0 && m.blockedSelected >= 0 && m.blockedSelected < len(blocked) {
					task := blocked[m.blockedSelected]
					return m, openEditor(m.cfg.document, task.line)
				}
				return m, openEditor(m.cfg.document, 1)
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "ctrl+c":
			if m.run != nil {
				m.run.cancel()
				m.exitCode = 130
			}
			return m, tea.Quit
		case "i", "tab":
			m.mode = modeReview
			m.blockedSelected = 0
			return m, nil
		case "e":
			if m.running || m.starting {
				m.status = "Agent is active; press space to pause before opening editor"
				return m, nil
			}
			line := 1
			if m.current.line > 0 {
				line = m.current.line
			}
			return m, openEditor(m.cfg.document, line)
		case " ":
			m.autoRun = !m.autoRun
			if m.autoRun && !m.running && !m.starting && !m.waiting && m.doc.pending > 0 {
				return m.startNextRun()
			}
			if !m.autoRun && m.waiting {
				m.waiting = false
				m.status = "Paused"
			}
			return m, nil
		case "r":
			if !m.running && !m.starting {
				if err := m.reloadDocument(); err != nil {
					m.status = err.Error()
					m.exitCode = 1
					return m, nil
				}
				if m.doc.pending > 0 {
					m.waiting = false
					m.exitCode = 0
					return m.startNextRun()
				}
				m.setTerminalState()
			}
			return m, nil
		case "up", "k":
			m.scrollFromBottom += 1
			return m, nil
		case "down", "j":
			if m.scrollFromBottom > 0 {
				m.scrollFromBottom--
			}
			return m, nil
		case "pgup":
			m.scrollFromBottom += max(1, m.height-8)
			return m, nil
		case "pgdown":
			m.scrollFromBottom = max(0, m.scrollFromBottom-max(1, m.height-8))
			return m, nil
		case "g":
			m.scrollFromBottom = len(m.output) + 1
			return m, nil
		case "G", "end":
			m.scrollFromBottom = 0
			return m, nil
		}

	case agentStartedMsg:
		m.starting = false
		if msg.err != nil {
			m.running = false
			m.status = msg.err.Error()
			m.exitCode = 1
			m.appendLine("! " + msg.err.Error())
			return m, nil
		}
		m.run = msg.run
		m.running = true
		m.status = fmt.Sprintf("Running section: %s", m.current.title)
		m.appendLine(fmt.Sprintf("--- Run %d: %s ---", m.iteration+1, m.current.title))
		return m, waitForAgent(m.run.events)

	case agentOutputMsg:
		if msg.output.kind == outputDone {
			m.flushPartial()
			m.run = nil
			m.running = false
			m.iteration++
			return m.finishRun(msg.output.exitCode)
		}
		if msg.output.kind == outputModel {
			if msg.output.text != "" {
				m.modelName = msg.output.text
			}
			if m.run != nil {
				return m, waitForAgent(m.run.events)
			}
			return m, nil
		}
		if msg.output.kind == outputDelta {
			m.appendDelta(msg.output.text)
		} else {
			m.appendLine(msg.output.text)
		}
		if m.run != nil {
			return m, waitForAgent(m.run.events)
		}
		return m, nil

	case nextRunMsg:
		if !m.autoRun || m.running || m.starting {
			m.waiting = false
			return m, nil
		}
		m.waiting = false
		if err := m.reloadDocument(); err != nil {
			m.status = err.Error()
			m.exitCode = 1
			return m, nil
		}
		if m.doc.pending == 0 {
			m.setTerminalState()
			return m, nil
		}
		return m.startNextRun()

	case spinMsg:
		if m.running || m.starting || m.waiting {
			m.spinFrame++
			if m.running && m.spinFrame%5 == 0 {
				_ = m.reloadDocument()
			}
			return m, spinTick()
		}
	}
	return m, nil
}

func (m model) View() string {
	width := max(60, m.width)
	height := max(16, m.height)

	if m.mode == modeReview || m.mode == modeInput {
		return m.reviewView(width, height)
	}

	bodyHeight := height - 2
	leftWidth := max(27, width*34/100)
	if leftWidth > width-30 {
		leftWidth = width - 30
	}
	rightWidth := width - leftWidth

	header := m.headerView(width)
	left := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(panelEdge).
		Width(max(1, leftWidth-2)).
		Height(max(1, bodyHeight-2)).
		Render(m.progressView(max(1, leftWidth-4), max(1, bodyHeight-4)))
	right := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(panelEdge).
		Width(max(1, rightWidth-2)).
		Height(max(1, bodyHeight-2)).
		Render(m.agentView(max(1, rightWidth-4), max(1, bodyHeight-4)))
	footer := m.footerView(width)
	return header + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, left, right) + "\n" + footer
}

func (m *model) prepareRun() {
	target, ok := m.doc.nextSection()
	if !ok {
		m.setTerminalState()
		return
	}
	m.current = target
	m.runStartPending = m.doc.pending
	m.starting = true
	m.status = fmt.Sprintf("Starting section: %s", target.title)
}

func (m model) startNextRun() (tea.Model, tea.Cmd) {
	m.prepareRun()
	if !m.starting {
		return m, nil
	}
	return m, tea.Batch(startAgent(m.cfg, m.current), spinTick())
}

func (m model) finishRun(exitCode int) (tea.Model, tea.Cmd) {
	before := m.runStartPending
	if err := m.reloadDocument(); err != nil {
		m.status = err.Error()
		m.exitCode = 1
		return m, nil
	}
	completed := before - m.doc.pending

	if exitCode != 0 {
		m.exitCode = exitCode
		m.status = fmt.Sprintf("omp exited with status %d", exitCode)
		return m, nil
	}
	if m.doc.pending == 0 {
		m.setTerminalState()
		return m, nil
	}
	if completed > 0 {
		m.stall = 0
		m.status = fmt.Sprintf("Completed %d task(s); %d remain", completed, m.doc.pending)
		if m.doc.blocked > 0 {
			m.status += fmt.Sprintf(" (%d blocked)", m.doc.blocked)
		}
	} else {
		m.stall++
		if m.stall >= m.cfg.stallLimit {
			m.exitCode = 3
			m.status = fmt.Sprintf("Stopped after %d runs without checkbox progress", m.stall)
			return m, nil
		}
		m.status = fmt.Sprintf("No checkbox progress (%d/%d)", m.stall, m.cfg.stallLimit)
	}
	if m.cfg.maxIterations > 0 && m.iteration >= m.cfg.maxIterations {
		m.exitCode = 2
		m.status = fmt.Sprintf("Reached maximum of %d runs", m.cfg.maxIterations)
		return m, nil
	}
	if !m.autoRun {
		m.status += " · paused"
		return m, nil
	}
	m.waiting = true
	return m, tea.Tick(m.cfg.sleep, func(time.Time) tea.Msg { return nextRunMsg{} })
}

func (m *model) reloadDocument() error {
	doc, err := readDocument(m.cfg.document)
	if err != nil {
		return err
	}
	m.doc = doc
	return nil
}

func (m *model) setTerminalState() {
	if m.doc.pending == 0 {
		if m.doc.blocked > 0 {
			m.exitCode = 0
			m.status = fmt.Sprintf("All pending tasks complete · %d blocked task(s) need input (press 'i' to review)", m.doc.blocked)
		} else {
			m.exitCode = 0
			m.status = "All tasks complete"
		}
	}
}

func (m *model) appendLine(line string) {
	m.flushPartial()
	m.output = append(m.output, line)
	m.trimOutput()
}

func (m *model) appendDelta(delta string) {
	delta = strings.ReplaceAll(delta, "\r", "")
	parts := strings.Split(delta, "\n")
	for index, part := range parts {
		if !m.hasPartial {
			m.partial = ""
			m.hasPartial = true
		}
		m.partial += part
		if index < len(parts)-1 {
			m.output = append(m.output, m.partial)
			m.partial = ""
			m.hasPartial = false
		}
	}
	m.trimOutput()
}

func (m *model) flushPartial() {
	if !m.hasPartial {
		return
	}
	m.output = append(m.output, m.partial)
	m.partial = ""
	m.hasPartial = false
	m.trimOutput()
}

func (m *model) trimOutput() {
	if len(m.output) <= maxOutputLines {
		return
	}
	drop := len(m.output) - maxOutputLines
	copy(m.output, m.output[drop:])
	m.output = m.output[:maxOutputLines]
}

func spinTick() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(now time.Time) tea.Msg { return spinMsg(now) })
}

func openEditor(filePath string, line int) tea.Cmd {
	editor := os.Getenv("EDITOR")
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		for _, fallback := range []string{"vim", "nano", "vi", "code"} {
			if path, err := exec.LookPath(fallback); err == nil {
				editor = path
				break
			}
		}
	}
	if editor == "" {
		editor = "vi"
	}

	var args []string
	base := filepath.Base(editor)
	switch {
	case strings.HasPrefix(base, "vim"), strings.HasPrefix(base, "nvim"), strings.HasPrefix(base, "vi"), strings.HasPrefix(base, "nano"):
		if line > 0 {
			args = []string{fmt.Sprintf("+%d", line), filePath}
		} else {
			args = []string{filePath}
		}
	case base == "code":
		if line > 0 {
			args = []string{"--goto", fmt.Sprintf("%s:%d", filePath, line), "-w"}
		} else {
			args = []string{"-w", filePath}
		}
	default:
		args = []string{filePath}
	}

	cmd := exec.Command(editor, args...)
	return tea.ExecProcess(cmd, func(err error) tea.Msg {
		return editorFinishedMsg{err: err}
	})
}

func (m model) headerView(width int) string {
	right := doneStyle.Render(fmt.Sprintf("%d/%d complete", m.doc.done, m.doc.total))
	left := m.renderHeaderTitle(" LOOP "+filepath.Base(m.cfg.document), width-lipgloss.Width(right)-1)
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m model) renderHeaderTitle(base string, availWidth int) string {
	if m.modelName == "" {
		if len(base) > availWidth && availWidth > 0 {
			return headerStyle.Render(truncate(base, availWidth))
		}
		return headerStyle.Render(base)
	}

	baseWidth := len(base)
	dotWidth := 3 // " · "
	remain := availWidth - baseWidth - dotWidth
	if remain <= 0 {
		if len(base) > availWidth && availWidth > 0 {
			return headerStyle.Render(truncate(base, availWidth))
		}
		return headerStyle.Render(base)
	}

	modelDisp := m.modelName
	if len(modelDisp) > remain && strings.Contains(modelDisp, "/") {
		parts := strings.SplitN(modelDisp, "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			modelDisp = parts[1]
		}
	}
	if len(modelDisp) > remain {
		if remain > 4 {
			modelDisp = truncate(modelDisp, remain)
		} else {
			modelDisp = ""
		}
	}

	if modelDisp != "" {
		return headerStyle.Render(base) + mutedStyle.Render(" · ") + activeStyle.Render(modelDisp)
	}
	if len(base) > availWidth && availWidth > 0 {
		return headerStyle.Render(truncate(base, availWidth))
	}
	return headerStyle.Render(base)
}

func (m model) progressView(width, height int) string {
	var view strings.Builder
	view.WriteString(headerStyle.Render("Document progress"))
	view.WriteByte('\n')
	percentage := 0
	if m.doc.total > 0 {
		percentage = m.doc.done * 100 / m.doc.total
	}
	barWidth := max(8, width-9)
	filled := 0
	if m.doc.total > 0 {
		filled = barWidth * m.doc.done / m.doc.total
	}
	view.WriteString(doneStyle.Render(strings.Repeat("=", filled)))
	view.WriteString(mutedStyle.Render(strings.Repeat("-", barWidth-filled)))
	view.WriteString(fmt.Sprintf(" %3d%%", percentage))
	view.WriteByte('\n')
	view.WriteString(fmt.Sprintf("%d pending", m.doc.pending))
	if m.doc.blocked > 0 {
		view.WriteString(errorStyle.Render(fmt.Sprintf(" · %d blocked", m.doc.blocked)))
	}
	view.WriteString("\n\n")

	available := max(1, height-6)
	sections := m.visibleSections(available)
	for _, section := range sections {
		symbol := "[ ]"
		style := mutedStyle
		switch {
		case section.blocked() > 0:
			symbol = "[!]"
			style = errorStyle
		case section.pending() == 0:
			symbol = "[x]"
			style = doneStyle
		case section.line == m.current.line:
			symbol = "[>]"
			style = activeStyle
		}
		labelWidth := max(4, width-12)
		label := truncate(section.title, labelWidth)
		line := fmt.Sprintf("%s %-*s %d/%d", symbol, labelWidth, label, section.done(), len(section.tasks))
		view.WriteString(style.Render(line))
		view.WriteByte('\n')
	}
	return strings.TrimSuffix(view.String(), "\n")
}

func (m model) visibleSections(limit int) []section {
	sections := make([]section, 0, len(m.doc.sections))
	for _, section := range m.doc.sections {
		if len(section.tasks) > 0 {
			sections = append(sections, section)
		}
	}
	if len(sections) <= limit {
		return sections
	}
	active := 0
	for index, section := range sections {
		if section.line == m.current.line {
			active = index
			break
		}
	}
	start := max(0, active-limit/2)
	if start+limit > len(sections) {
		start = len(sections) - limit
	}
	return sections[start : start+limit]
}

func (m model) agentView(width, height int) string {
	title := "Agent output"
	if m.current.title != "" {
		title += " · " + truncate(m.current.title, max(8, width-len("Agent output · ")))
	}
	lines := append([]string(nil), m.output...)
	if m.hasPartial {
		lines = append(lines, m.partial)
	}
	if len(lines) == 0 {
		lines = []string{"Waiting for agent output..."}
	}

	wrapped := make([]string, 0, len(lines))
	wrapStyle := lipgloss.NewStyle().Width(width)
	for _, line := range lines {
		wrapped = append(wrapped, strings.Split(wrapStyle.Render(line), "\n")...)
	}
	bodyHeight := max(1, height-2)
	maxScroll := max(0, len(wrapped)-bodyHeight)
	scroll := min(m.scrollFromBottom, maxScroll)
	end := len(wrapped) - scroll
	start := max(0, end-bodyHeight)
	body := strings.Join(wrapped[start:end], "\n")

	indicator := ""
	if scroll > 0 {
		indicator = warnStyle.Render(fmt.Sprintf(" · %d line(s) below", scroll))
	}
	return activeStyle.Render(title) + indicator + "\n\n" + body
}

func (m model) footerView(width int) string {
	spinner := ""
	if m.running || m.starting || m.waiting {
		frames := []string{"|", "/", "-", "\\"}
		spinner = frames[m.spinFrame%len(frames)] + " "
	}
	controlsText := "space pause/resume · r retry"
	if m.doc.blocked > 0 {
		controlsText += fmt.Sprintf(" · i review (%d blocked)", m.doc.blocked)
	} else {
		controlsText += " · i review"
	}
	controlsText += " · e edit · j/k scroll · q quit"
	controls := mutedStyle.Render(controlsText)
	status := truncate(spinner+m.status, max(1, width-lipgloss.Width(controls)-1))
	gap := max(1, width-lipgloss.Width(status)-lipgloss.Width(controls))
	return status + strings.Repeat(" ", gap) + controls
}

func (m model) reviewView(width, height int) string {
	blocked := m.doc.blockedTasks()
	if len(blocked) == 0 {
		header := m.reviewHeaderView(width, 0)
		body := "\n  No blocked tasks requiring input.\n\n  Press 'i' or 'esc' to return to runner.\n"
		footer := m.reviewFooter(width)
		return header + body + footer
	}

	header := m.reviewHeaderView(width, len(blocked))
	bodyHeight := height - 2
	leftWidth := max(30, width*38/100)
	rightWidth := width - leftWidth

	leftContent := m.reviewList(blocked, max(1, leftWidth-4), max(1, bodyHeight-4))
	rightContent := m.reviewDetail(blocked, max(1, rightWidth-4), max(1, bodyHeight-4))

	left := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(panelEdge).
		Width(max(1, leftWidth-2)).
		Height(max(1, bodyHeight-2)).
		Render(leftContent)

	right := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(panelEdge).
		Width(max(1, rightWidth-2)).
		Height(max(1, bodyHeight-2)).
		Render(rightContent)

	footer := m.reviewFooter(width)
	return header + "\n" + lipgloss.JoinHorizontal(lipgloss.Top, left, right) + "\n" + footer
}

func (m model) reviewHeaderView(width int, count int) string {
	right := warnStyle.Render(fmt.Sprintf("%d task(s) need input", count))
	left := m.renderHeaderTitle(" LOOP "+filepath.Base(m.cfg.document)+" · Review Mode", width-lipgloss.Width(right)-1)
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m model) reviewList(blocked []taskItem, width, height int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Blocked Tasks / Needs Input"))
	b.WriteString("\n\n")

	for i, task := range blocked {
		sel := "  "
		style := mutedStyle
		if i == m.blockedSelected {
			sel = "> "
			style = activeStyle
		}
		taskPart, _ := splitTaskAndIssue(task.text)
		lineTitle := fmt.Sprintf("[%s] L%d: %s", truncate(task.sectionTitle, 12), task.line, truncate(taskPart, max(4, width-18)))
		b.WriteString(style.Render(sel + lineTitle))
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

func (m model) reviewDetail(blocked []taskItem, width, height int) string {
	if m.blockedSelected < 0 || m.blockedSelected >= len(blocked) {
		return ""
	}
	task := blocked[m.blockedSelected]
	taskPart, issuePart := splitTaskAndIssue(task.text)

	var b strings.Builder
	b.WriteString(activeStyle.Render(fmt.Sprintf("Section: %s (line %d)", task.sectionTitle, task.line)))
	b.WriteString("\n\n")

	b.WriteString(lipgloss.NewStyle().Bold(true).Render("Task:"))
	b.WriteString("\n")
	wrapStyle := lipgloss.NewStyle().Width(width)
	b.WriteString(wrapStyle.Render(taskPart))
	b.WriteString("\n\n")

	if issuePart != "" {
		b.WriteString(warnStyle.Bold(true).Render("Reason / User Input Needed:"))
		b.WriteString("\n")
		issueBox := lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(yellow).
			Padding(0, 1).
			Width(max(10, width-2)).
			Render(wrapStyle.Render(issuePart))
		b.WriteString(issueBox)
		b.WriteString("\n\n")
	}

	if m.mode == modeInput {
		b.WriteString(activeStyle.Bold(true).Render("Enter your answer / instruction for the agent:"))
		b.WriteString("\n")
		b.WriteString(m.textInput.View())
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("Press [Enter] to submit & re-queue task · [Esc] to cancel"))
	} else if m.running || m.starting {
		b.WriteString(warnStyle.Render("Agent is currently active.\nPress [space] in runner to pause before answering or editing."))
		b.WriteString("\n\n")
		b.WriteString(mutedStyle.Render("  [esc / i] Back to runner"))
	} else {
		b.WriteString(mutedStyle.Render("Actions on selected task:"))
		b.WriteString("\n")
		b.WriteString(doneStyle.Render("  [a / enter] Provide answer & re-queue (- [ ])\n"))
		b.WriteString(doneStyle.Render("  [x]         Mark as done / verified (- [x])\n"))
		b.WriteString(warnStyle.Render("  [u]         Unblock / retry as pending (- [ ])\n"))
		b.WriteString(activeStyle.Render("  [e]         Open document in $EDITOR\n"))
		b.WriteString(mutedStyle.Render("  [esc / i]   Back to runner"))
	}

	return strings.TrimSuffix(b.String(), "\n")
}

func (m model) reviewFooter(width int) string {
	controls := ""
	if m.mode == modeInput {
		controls = mutedStyle.Render("enter submit · esc cancel")
	} else if m.running || m.starting {
		controls = mutedStyle.Render("read-only (running) · esc/i back · q quit")
	} else {
		controls = mutedStyle.Render("a/enter answer · x mark done · u unblock · e edit · esc/i back · q quit")
	}
	status := truncate(m.status, max(1, width-lipgloss.Width(controls)-1))
	gap := max(1, width-lipgloss.Width(status)-lipgloss.Width(controls))
	return status + strings.Repeat(" ", gap) + controls
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

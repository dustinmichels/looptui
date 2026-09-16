package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	modeSelect
)

type model struct {
	cfg config
	doc document

	width  int
	height int

	mode          uiMode
	modelName     string
	contextTokens int
	discoveredDocs []checklistDoc
	selectedDocIdx int
	selectScroll   int
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

func (m model) numSelectableDocs() int {
	count := 0
	for _, d := range m.discoveredDocs {
		if !d.isComplete() {
			count++
		}
	}
	return count
}

func (m model) totalSelectRows() int {
	n := len(m.discoveredDocs)
	numSelectable := m.numSelectableDocs()
	if numSelectable > 0 && numSelectable < n {
		return n + 1
	}
	return n
}

func (m model) selectRowAt(r int) (checklistDoc, int, bool) {
	numSelectable := m.numSelectableDocs()
	hasDivider := numSelectable > 0 && numSelectable < len(m.discoveredDocs)
	if hasDivider {
		if r < numSelectable {
			return m.discoveredDocs[r], r, false
		}
		if r == numSelectable {
			return checklistDoc{}, -1, true
		}
		return m.discoveredDocs[r-1], r - 1, false
	}
	return m.discoveredDocs[r], r, false
}

func (m model) selectVisibleHeight() int {
	bodyHeight := max(6, m.height-2)
	listHeight := max(1, bodyHeight-2)
	return max(1, listHeight-4)
}

func (m model) selectMaxScroll() int {
	totalRows := m.totalSelectRows()
	visibleHeight := m.selectVisibleHeight()
	if totalRows <= visibleHeight {
		return 0
	}
	capacity := max(1, visibleHeight-1)
	return max(0, totalRows-capacity)
}

func (m model) clampSelectScroll() int {
	maxScroll := m.selectMaxScroll()
	if m.selectScroll > maxScroll {
		return maxScroll
	}
	if m.selectScroll < 0 {
		return 0
	}
	return m.selectScroll
}

func (m *model) setClampedSelectScroll() {
	m.selectScroll = m.clampSelectScroll()
}

func (m model) selectVisibleRange() (int, int) {
	visibleHeight := m.selectVisibleHeight()
	totalRows := m.totalSelectRows()
	scroll := m.clampSelectScroll()

	start := scroll
	availRows := visibleHeight
	if start > 0 {
		availRows--
	}
	if start+availRows < totalRows {
		availRows--
	}
	availRows = max(1, availRows)
	end := min(totalRows, start+availRows)
	return start, end
}

func (m *model) revealSelectedDoc() {
	if m.selectedDocIdx < 0 {
		m.setClampedSelectScroll()
		return
	}
	cursorRow := m.selectedDocIdx
	for cursorRow < m.selectScroll && m.selectScroll > 0 {
		m.selectScroll--
	}
	maxScroll := m.selectMaxScroll()
	for {
		start, end := m.selectVisibleRange()
		if (cursorRow >= start && cursorRow < end) || m.selectScroll >= maxScroll {
			break
		}
		m.selectScroll++
	}
	m.setClampedSelectScroll()
}

func (m model) isSelectedDocVisible() bool {
	if m.selectedDocIdx < 0 {
		return false
	}
	start, end := m.selectVisibleRange()
	cursorRow := m.selectedDocIdx
	return cursorRow >= start && cursorRow < end
}

func newSelectModel(cfg config, docs []checklistDoc) model {
	ti := textinput.New()
	ti.Placeholder = "Type answer / response for the agent and press Enter..."
	ti.CharLimit = 1000

	initialIdx := -1
	for i, d := range docs {
		if !d.isComplete() {
			initialIdx = i
			break
		}
	}

	status := "Select a document to run"
	if initialIdx == -1 && len(docs) > 0 {
		status = "All documents are complete"
	}

	return model{
		cfg:            cfg,
		width:          100,
		height:         30,
		autoRun:        true,
		status:         status,
		textInput:      ti,
		modelName:      cfg.model,
		mode:           modeSelect,
		discoveredDocs: docs,
		selectedDocIdx: initialIdx,
	}
}

func (m model) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.mode == modeSelect {
		if m.modelName == "" && m.cfg.ompPath != "" {
			cmds = append(cmds, detectDefaultModelCmd(m.cfg.ompPath))
		}
		if len(cmds) > 0 {
			return tea.Batch(cmds...)
		}
		return nil
	}
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
		if m.mode == modeSelect {
			m.setClampedSelectScroll()
		}
		return m, nil

	case modelDetectedMsg:
		if m.modelName == "" && msg.model != "" {
			m.modelName = msg.model
		}
		return m, nil

	case editorFinishedMsg:
		if m.mode == modeSelect {
			var selectedAbsPath string
			if m.selectedDocIdx >= 0 && m.selectedDocIdx < len(m.discoveredDocs) {
				selectedAbsPath = m.discoveredDocs[m.selectedDocIdx].absPath
			}
			if docs, err := findChecklistDocuments("."); err == nil && len(docs) > 0 {
				m.discoveredDocs = docs
				selectableCount := m.numSelectableDocs()
				found := false
				if selectedAbsPath != "" {
					for i := range selectableCount {
						if m.discoveredDocs[i].absPath == selectedAbsPath {
							m.selectedDocIdx = i
							found = true
							break
						}
					}
				}
				if !found {
					if selectableCount > 0 {
						if m.selectedDocIdx >= selectableCount {
							m.selectedDocIdx = selectableCount - 1
						}
						if m.selectedDocIdx < 0 {
							m.selectedDocIdx = 0
						}
						if m.status == "All documents are complete" {
							m.status = "Select a document to run"
						}
					} else {
						m.selectedDocIdx = -1
						m.status = "All documents are complete"
					}
				}
				m.revealSelectedDoc()
			}
			return m, nil
		}
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
		if m.mode == modeSelect {
			selectableCount := m.numSelectableDocs()
			visibleHeight := m.selectVisibleHeight()
			maxScroll := m.selectMaxScroll()

			switch msg.String() {
			case "q", "ctrl+c":
				m.exitCode = 0
				return m, tea.Quit
			case "up", "k":
				if m.selectScroll > 0 && (selectableCount == 0 || m.selectScroll > m.selectedDocIdx) {
					m.selectScroll--
				} else if m.selectedDocIdx > 0 {
					m.selectedDocIdx--
					m.revealSelectedDoc()
				}
				return m, nil
			case "down", "j":
				if selectableCount > 0 && m.selectedDocIdx < selectableCount-1 {
					m.selectedDocIdx++
					m.revealSelectedDoc()
				} else if m.selectScroll < maxScroll {
					m.selectScroll++
				}
				return m, nil
			case "pgdown", "ctrl+d":
				m.selectScroll = min(maxScroll, m.selectScroll+visibleHeight)
				return m, nil
			case "pgup", "ctrl+u":
				m.selectScroll = max(0, m.selectScroll-visibleHeight)
				return m, nil
			case "home", "g":
				if selectableCount > 0 {
					m.selectedDocIdx = 0
				}
				m.selectScroll = 0
				return m, nil
			case "end", "G":
				if selectableCount > 0 {
					m.selectedDocIdx = selectableCount - 1
				}
				m.selectScroll = maxScroll
				return m, nil
			case "1", "2", "3", "4", "5", "6", "7", "8", "9":
				idx := int(msg.String()[0] - '1')
				if idx < selectableCount {
					m.selectedDocIdx = idx
					m.revealSelectedDoc()
				}
				return m, nil
			case "e":
				if m.selectedDocIdx >= 0 && m.selectedDocIdx < selectableCount && m.isSelectedDocVisible() {
					selected := m.discoveredDocs[m.selectedDocIdx]
					return m, openEditor(selected.absPath, 0)
				}
				return m, nil
			case "enter":
				if m.selectedDocIdx < 0 || m.selectedDocIdx >= selectableCount || !m.isSelectedDocVisible() {
					return m, nil
				}
				selected := m.discoveredDocs[m.selectedDocIdx]
				doc, err := readDocument(selected.absPath)
				if err != nil {
					m.status = fmt.Sprintf("Error reading %s: %v", selected.path, err)
					return m, nil
				}
				m.cfg.document = selected.absPath
				m.doc = doc
				m.mode = modeRunner
				m.setTerminalState()
				if m.doc.pending > 0 {
					m.prepareRun()
				}
				var cmds []tea.Cmd
				if m.starting {
					cmds = append(cmds, startAgent(m.cfg, m.current), spinTick())
				}
				if m.modelName == "" && m.cfg.ompPath != "" {
					cmds = append(cmds, detectDefaultModelCmd(m.cfg.ompPath))
				}
				if len(cmds) > 0 {
					return m, tea.Batch(cmds...)
				}
				return m, nil
			}
			return m, nil
		}

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
		if msg.output.kind == outputUsage {
			active := msg.output.usage.ActiveContext()
			if active > 0 {
				m.contextTokens = active
			}
			if m.run != nil {
				return m, waitForAgent(m.run.events)
			}
			return m, nil
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

	if m.mode == modeSelect {
		return m.selectView(width, height)
	}

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
	m.contextTokens = 0
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
	ctxText := ""
	var ctxStyled string
	if m.contextTokens > 0 {
		ctxText = fmt.Sprintf("%s ctx", formatTokens(m.contextTokens))
		ctxStyled = mutedStyle.Render(ctxText)
	}

	modelDisp := m.modelName
	dot := mutedStyle.Render(" · ")
	dotLen := 3

	// First attempt: base + full model + ctxText
	totalNeeded := len(base)
	if modelDisp != "" {
		totalNeeded += dotLen + len(modelDisp)
	}
	if ctxText != "" {
		totalNeeded += dotLen + len(ctxText)
	}

	if totalNeeded <= availWidth {
		res := headerStyle.Render(base)
		if modelDisp != "" {
			res += dot + activeStyle.Render(modelDisp)
		}
		if ctxText != "" {
			res += dot + ctxStyled
		}
		return res
	}

	// If it doesn't fit with full model, try stripping provider prefix from model:
	strippedModel := modelDisp
	if strings.Contains(strippedModel, "/") {
		parts := strings.SplitN(strippedModel, "/", 2)
		if len(parts) == 2 && parts[1] != "" {
			strippedModel = parts[1]
		}
	}

	totalWithStripped := len(base)
	if strippedModel != "" {
		totalWithStripped += dotLen + len(strippedModel)
	}
	if ctxText != "" {
		totalWithStripped += dotLen + len(ctxText)
	}

	if totalWithStripped <= availWidth {
		res := headerStyle.Render(base)
		if strippedModel != "" {
			res += dot + activeStyle.Render(strippedModel)
		}
		if ctxText != "" {
			res += dot + ctxStyled
		}
		return res
	}

	// Next attempt: base + full model (without ctxText)
	neededFullModelOnly := len(base)
	if modelDisp != "" {
		neededFullModelOnly += dotLen + len(modelDisp)
	}
	if neededFullModelOnly <= availWidth {
		res := headerStyle.Render(base)
		if modelDisp != "" {
			res += dot + activeStyle.Render(modelDisp)
		}
		return res
	}

	// Next attempt: base + stripped model (without ctxText)
	neededStrippedModelOnly := len(base)
	if strippedModel != "" {
		neededStrippedModelOnly += dotLen + len(strippedModel)
	}
	if neededStrippedModelOnly <= availWidth {
		res := headerStyle.Render(base)
		if strippedModel != "" {
			res += dot + activeStyle.Render(strippedModel)
		}
		return res
	}

	// Next: truncate strippedModel if space allows
	remainForModel := availWidth - len(base) - dotLen
	if strippedModel != "" && remainForModel > 4 {
		shortModel := truncate(strippedModel, remainForModel)
		return headerStyle.Render(base) + dot + activeStyle.Render(shortModel)
	}

	// Fallback to base
	if len(base) > availWidth && availWidth > 0 {
		return headerStyle.Render(truncate(base, availWidth))
	}
	return headerStyle.Render(base)
}

func formatTokens(n int) string {
	if n <= 0 {
		return "0"
	}
	if n < 1000 {
		return strconv.Itoa(n)
	}
	if n < 10000 {
		val := float64(n) / 1000.0
		return fmt.Sprintf("%.1fk", val)
	}
	if n < 1000000 {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	val := float64(n) / 1000000.0
	return fmt.Sprintf("%.1fm", val)
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

func (m model) selectView(width, height int) string {
	header := m.selectHeaderView(width)
	footer := m.selectFooter(width)
	bodyHeight := max(6, height-2)

	content := m.selectList(max(1, width-4), max(1, bodyHeight-2))

	panel := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(panelEdge).
		Width(max(1, width-2)).
		Height(max(1, bodyHeight-2)).
		Render(content)

	return header + "\n" + panel + "\n" + footer
}

func (m model) selectHeaderView(width int) string {
	count := len(m.discoveredDocs)
	docWord := "documents"
	if count == 1 {
		docWord = "document"
	}
	right := mutedStyle.Render(fmt.Sprintf("%d %s with checklists", count, docWord))
	left := m.renderHeaderTitle(" LOOP · Select Document", width-lipgloss.Width(right)-1)
	gap := max(1, width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m model) selectFooter(width int) string {
	controls := mutedStyle.Render("↑/↓ or j/k navigate · 1-9 jump · enter select · e edit · q quit")
	status := truncate(m.status, max(1, width-lipgloss.Width(controls)-1))
	gap := max(1, width-lipgloss.Width(status)-lipgloss.Width(controls))
	return status + strings.Repeat(" ", gap) + controls
}

func (m model) selectList(width, height int) string {
	var b strings.Builder
	b.WriteString(headerStyle.Render("Discovered Markdown Documents with Checklists"))
	b.WriteString("\n\n")

	if len(m.discoveredDocs) == 0 {
		b.WriteString(mutedStyle.Render("No markdown files with checklists found in current directory."))
		return b.String()
	}

	start, end := m.selectVisibleRange()
	numSelectable := m.numSelectableDocs()
	hasDivider := numSelectable > 0 && numSelectable < len(m.discoveredDocs)
	totalRows := m.totalSelectRows()

	if start > 0 {
		docsAbove := start
		if hasDivider && start > numSelectable {
			docsAbove = start - 1
		}
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ↑ %d more above", docsAbove)))
		b.WriteByte('\n')
	}

	for r := start; r < end; r++ {
		doc, docIdx, isDivider := m.selectRowAt(r)
		if isDivider {
			b.WriteByte('\n')
			continue
		}

		isComplete := doc.isComplete()
		isSelected := !isComplete && docIdx == m.selectedDocIdx

		sel := "  "
		itemStyle := lipgloss.NewStyle()
		if isSelected {
			sel = "> "
			itemStyle = activeStyle
		}

		pct := 0
		if doc.doc.total > 0 {
			pct = (doc.doc.done * 100) / doc.doc.total
		}

		var compText string
		var numPrefix string

		if isComplete {
			itemStyle = mutedStyle
			numPrefix = "      "
			compText = mutedStyle.Render(fmt.Sprintf("[%3d%%] %d/%d completed", pct, doc.doc.done, doc.doc.total))
		} else {
			var pctBadge string
			if doc.doc.blocked > 0 && doc.doc.pending == 0 {
				pctBadge = warnStyle.Render(fmt.Sprintf("[%3d%%]", pct))
			} else {
				pctBadge = activeStyle.Render(fmt.Sprintf("[%3d%%]", pct))
			}

			var detailParts []string
			if doc.doc.done > 0 {
				detailParts = append(detailParts, doneStyle.Render(fmt.Sprintf("%d done", doc.doc.done)))
			}
			if doc.doc.pending > 0 {
				detailParts = append(detailParts, fmt.Sprintf("%d pending", doc.doc.pending))
			}
			if doc.doc.blocked > 0 {
				detailParts = append(detailParts, warnStyle.Render(fmt.Sprintf("%d blocked", doc.doc.blocked)))
			}
			details := strings.Join(detailParts, ", ")
			if details != "" {
				details = " (" + details + ")"
			}

			compText = fmt.Sprintf("%s %d/%d completed%s", pctBadge, doc.doc.done, doc.doc.total, details)
			numPrefix = fmt.Sprintf("%s%2d. ", sel, docIdx+1)
		}

		compWidth := lipgloss.Width(compText)
		if !isComplete && width-compWidth-len(numPrefix)-1 < 15 && strings.Contains(compText, "(") {
			// Drop detailed parenthetical if space is constrained
			var pctBadge string
			if doc.doc.blocked > 0 && doc.doc.pending == 0 {
				pctBadge = warnStyle.Render(fmt.Sprintf("[%3d%%]", pct))
			} else {
				pctBadge = activeStyle.Render(fmt.Sprintf("[%3d%%]", pct))
			}
			compText = fmt.Sprintf("%s %d/%d completed", pctBadge, doc.doc.done, doc.doc.total)
			compWidth = lipgloss.Width(compText)
		}

		availPath := max(10, width-compWidth-len(numPrefix)-1)
		displayPath := doc.path
		if lipgloss.Width(displayPath) > availPath {
			displayPath = truncate(displayPath, availPath)
		}

		pathText := itemStyle.Render(numPrefix + displayPath)
		pathWidth := lipgloss.Width(pathText)

		gapLen := max(1, width-pathWidth-compWidth)
		b.WriteString(pathText)
		b.WriteString(strings.Repeat(" ", gapLen))
		b.WriteString(compText)
		b.WriteByte('\n')
	}

	if end < totalRows {
		docsRendered := end
		if hasDivider && end > numSelectable {
			docsRendered = end - 1
		}
		remainingDocs := len(m.discoveredDocs) - docsRendered
		b.WriteString(mutedStyle.Render(fmt.Sprintf("  ↓ %d more below", remainingDocs)))
		b.WriteByte('\n')
	}

	return strings.TrimSuffix(b.String(), "\n")
}

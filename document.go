package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

type taskState byte

const (
	taskPending taskState = ' '
	taskDone    taskState = 'x'
	taskBlocked taskState = '!'
)

type taskItem struct {
	line         int
	text         string
	state        taskState
	sectionTitle string
}

type section struct {
	title string
	line  int
	tasks []taskItem
}

func (s section) pending() int {
	count := 0
	for _, task := range s.tasks {
		if task.state == taskPending {
			count++
		}
	}
	return count
}

func (s section) done() int {
	count := 0
	for _, task := range s.tasks {
		if task.state == taskDone {
			count++
		}
	}
	return count
}

func (s section) blocked() int {
	count := 0
	for _, task := range s.tasks {
		if task.state == taskBlocked {
			count++
		}
	}
	return count
}

type document struct {
	sections []section
	total    int
	done     int
	pending  int
	blocked  int
}

func readDocument(path string) (document, error) {
	file, err := os.Open(path)
	if err != nil {
		return document{}, fmt.Errorf("read document: %w", err)
	}
	defer file.Close()

	var doc document
	current := -1
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := scanner.Text()
		if strings.HasPrefix(line, "## ") {
			doc.sections = append(doc.sections, section{
				title: strings.TrimSpace(strings.TrimPrefix(line, "## ")),
				line:  lineNumber,
			})
			current = len(doc.sections) - 1
			continue
		}

		state, text, ok := parseCheckbox(line)
		if !ok {
			continue
		}
		if current < 0 {
			doc.sections = append(doc.sections, section{title: "Tasks", line: lineNumber})
			current = 0
		}
		doc.sections[current].tasks = append(doc.sections[current].tasks, taskItem{
			line:         lineNumber,
			text:         text,
			state:        state,
			sectionTitle: doc.sections[current].title,
		})
		doc.total++
		switch state {
		case taskDone:
			doc.done++
		case taskBlocked:
			doc.blocked++
		case taskPending:
			doc.pending++
		}
	}
	if err := scanner.Err(); err != nil {
		return document{}, fmt.Errorf("scan document: %w", err)
	}
	return doc, nil
}

func parseCheckbox(line string) (taskState, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if len(trimmed) < 6 || trimmed[0] != '-' || trimmed[1] != ' ' || trimmed[2] != '[' || trimmed[4] != ']' {
		return 0, "", false
	}
	var state taskState
	switch trimmed[3] {
	case ' ':
		state = taskPending
	case 'x', 'X':
		state = taskDone
	case '!':
		state = taskBlocked
	default:
		return 0, "", false
	}
	return state, strings.TrimSpace(trimmed[5:]), true
}

func (d document) nextSection() (section, bool) {
	for _, section := range d.sections {
		if section.pending() > 0 {
			return section, true
		}
	}
	return section{}, false
}

func (d document) blockedTasks() []taskItem {
	var blocked []taskItem
	for _, s := range d.sections {
		for _, t := range s.tasks {
			if t.state == taskBlocked {
				blocked = append(blocked, t)
			}
		}
	}
	return blocked
}

func splitTaskAndIssue(text string) (string, string) {
	for _, sep := range []string{" - **Issue", " - **Reason", " — **Issue", " - Issue", " — Issue", " - **Needs", " — "} {
		if idx := strings.Index(text, sep); idx != -1 {
			task := strings.TrimSpace(text[:idx])
			issue := strings.TrimSpace(text[idx+len(sep)-len(strings.TrimLeft(sep, " -—")):])
			return task, issue
		}
	}
	return text, ""
}

func updateTaskInFile(filePath string, task taskItem, newState taskState, userNote string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	lines := strings.Split(string(content), "\n")
	targetIdx := -1

	// Check if task.line still points to the task
	if task.line >= 1 && task.line <= len(lines) {
		line := lines[task.line-1]
		_, text, ok := parseCheckbox(line)
		if ok && strings.HasPrefix(text, strings.TrimSpace(task.text)) {
			targetIdx = task.line - 1
		}
	}

	// If line shifted, search for matching task text in document
	if targetIdx == -1 {
		taskPrefix, _ := splitTaskAndIssue(task.text)
		for i, line := range lines {
			_, text, ok := parseCheckbox(line)
			if ok && (text == task.text || strings.HasPrefix(text, taskPrefix)) {
				targetIdx = i
				break
			}
		}
	}

	if targetIdx == -1 {
		return fmt.Errorf("could not find task %q in document", truncate(task.text, 40))
	}

	line := lines[targetIdx]
	trimmed := strings.TrimLeft(line, " \t")
	indentLen := len(line) - len(trimmed)
	indent := line[:indentLen]

	if len(trimmed) < 5 || trimmed[0] != '-' || trimmed[1] != ' ' || trimmed[2] != '[' || trimmed[4] != ']' {
		return fmt.Errorf("line %d is not a checkbox item: %q", targetIdx+1, line)
	}

	var box string
	switch newState {
	case taskDone:
		box = "- [x]"
	case taskBlocked:
		box = "- [!]"
	case taskPending:
		box = "- [ ]"
	default:
		box = "- [ ]"
	}

	rest := strings.TrimSpace(trimmed[5:])
	if userNote != "" {
		rest = rest + " - **User input:** " + strings.TrimSpace(userNote)
	}

	lines[targetIdx] = indent + box + " " + rest
	newContent := strings.Join(lines, "\n")

	return os.WriteFile(filePath, []byte(newContent), 0o644)
}

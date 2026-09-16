package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestReadDocumentTracksSectionsAndCheckboxStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.md")
	contents := `# Plan

## Finished
- [x] first task
- [X] second task

## Current
- [ ] pending task
  - [!] blocked nested task — needs input

### Notes
- not a checkbox

## Future
- [ ] another task
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	doc, err := readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.total != 5 || doc.done != 2 || doc.pending != 2 || doc.blocked != 1 {
		t.Fatalf("unexpected counts: %+v", doc)
	}
	if len(doc.sections) != 3 {
		t.Fatalf("got %d sections, want 3", len(doc.sections))
	}
	next, ok := doc.nextSection()
	if !ok || next.title != "Current" || next.line != 7 {
		t.Fatalf("unexpected next section: %+v, %v", next, ok)
	}
	blocked := doc.blockedTasks()
	if len(blocked) != 1 || blocked[0].sectionTitle != "Current" || !strings.Contains(blocked[0].text, "blocked nested task") {
		t.Fatalf("unexpected blocked tasks: %+v", blocked)
	}
}

func TestBuildOMPArgsScopesRunToOneSection(t *testing.T) {
	cfg := config{
		document:    "/tmp/migration plan.md",
		extraArgs:   []string{"--model", "opus"},
		autoApprove: true,
	}
	target := section{title: "Backend service", line: 42}

	args := buildOMPArgs(cfg, target)
	wantPrefix := []string{"--mode", "json", "-p", "--auto-approve", "@/tmp/migration plan.md"}
	if !reflect.DeepEqual(args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("argument prefix = %#v, want %#v", args[:len(wantPrefix)], wantPrefix)
	}
	prompt := args[len(wantPrefix)]
	if !strings.Contains(prompt, `"Backend service"`) || !strings.Contains(prompt, "line 42") || !strings.Contains(prompt, "continue with the remaining unchecked tasks") {
		t.Fatalf("prompt does not instruct continuing after blocked tasks: %q", prompt)
	}
	if !strings.Contains(prompt, "- **Issue:**") || !strings.Contains(prompt, "- **User input:**") {
		t.Fatalf("prompt missing issue format or user input handling: %q", prompt)
	}
	if !reflect.DeepEqual(args[len(args)-2:], []string{"--model", "opus"}) {
		t.Fatalf("extra arguments not preserved: %#v", args)
	}
}

func TestHelpIsAvailableWithoutDocumentOrOmp(t *testing.T) {
	if !helpRequested([]string{"--help"}) || !helpRequested([]string{"-h"}) || !helpRequested([]string{"help"}) {
		t.Fatal("help aliases were not recognized")
	}
	if helpRequested(nil) || helpRequested([]string{"migration.md"}) {
		t.Fatal("non-help invocations were treated as help")
	}
	if !strings.Contains(usage, "looptui [document.md]") || !strings.Contains(usage, "blocked task") {
		t.Fatalf("usage does not describe standalone document input: %q", usage)
	}
}

func TestStartCaffeinateStartsOrSkips(t *testing.T) {
	cmd, err := startCaffeinate()
	if err != nil {
		t.Fatalf("startCaffeinate failed: %v", err)
	}
	if cmd != nil {
		stopCaffeinate(cmd)
		if cmd.ProcessState == nil {
			t.Fatal("caffeinate process was not stopped and reaped")
		}
	}
}

func TestFindChecklistDocumentsRecursionAndCompleteness(t *testing.T) {
	dir := t.TempDir()

	// 1. Root plan with 3 tasks (1 done, 1 blocked, 1 pending)
	rootPlan := filepath.Join(dir, "root-plan.md")
	if err := os.WriteFile(rootPlan, []byte("## Section 1\n- [x] done\n- [!] blocked\n- [ ] pending\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 2. Nested plan in sub directory (2 tasks, both done)
	subDir := filepath.Join(dir, "sub", "tasks")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	subPlan := filepath.Join(subDir, "nested.md")
	if err := os.WriteFile(subPlan, []byte("## All Done\n- [x] first\n- [x] second\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 3. Markdown file with no checkboxes (should be skipped)
	noChecklist := filepath.Join(dir, "README.md")
	if err := os.WriteFile(noChecklist, []byte("# Hello\nJust some docs.\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 4. Checklist in .git directory (should be skipped)
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "ignored.md"), []byte("- [ ] task\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// 5. Checklist in node_modules directory (should be skipped)
	nodeDir := filepath.Join(dir, "node_modules", "pkg")
	if err := os.MkdirAll(nodeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeDir, "pkg-todo.md"), []byte("- [ ] task\n"), 0644); err != nil {
		t.Fatal(err)
	}

	docs, err := findChecklistDocuments(dir)
	if err != nil {
		t.Fatalf("findChecklistDocuments: %v", err)
	}

	if len(docs) != 2 {
		t.Fatalf("expected 2 documents, got %d: %+v", len(docs), docs)
	}

	// Incomplete documents sorted before complete ones, tie-broken by relative path
	if docs[0].path != "root-plan.md" {
		t.Errorf("expected docs[0].path to be root-plan.md, got %q", docs[0].path)
	}
	if docs[0].doc.total != 3 || docs[0].doc.done != 1 || docs[0].doc.blocked != 1 || docs[0].doc.pending != 1 {
		t.Errorf("unexpected counts for root-plan.md: %+v", docs[0].doc)
	}
	comp0 := docs[0].completenessString()
	if !strings.Contains(comp0, "33%") || !strings.Contains(comp0, "1/3 completed") || !strings.Contains(comp0, "1 done, 1 pending, 1 blocked") {
		t.Errorf("unexpected completeness string: %q", comp0)
	}

	expectedSubPath := filepath.Join("sub", "tasks", "nested.md")
	if docs[1].path != expectedSubPath {
		t.Errorf("expected docs[1].path to be %q, got %q", expectedSubPath, docs[1].path)
	}
	if docs[1].doc.total != 2 || docs[1].doc.done != 2 {
		t.Errorf("unexpected counts for nested.md: %+v", docs[1].doc)
	}
	comp1 := docs[1].completenessString()
	if !strings.Contains(comp1, "100%") || !strings.Contains(comp1, "2/2 completed") {
		t.Errorf("unexpected completeness string: %q", comp1)
	}
}

func TestFindChecklistDocumentsSortsIncompleteBeforeComplete(t *testing.T) {
	dir := t.TempDir()

	files := map[string]string{
		"a_finished.md":      "## Done\n- [x] task 1\n- [x] task 2\n",
		"b_pending.md":       "## Tasks\n- [ ] task 1\n",
		"c_blocked.md":       "## Blocked\n- [!] need feedback\n",
		"d_half_done.md":     "## Half\n- [x] task 1\n- [ ] task 2\n",
		"e_also_finished.md": "## Finished\n- [x] single task\n",
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	docs, err := findChecklistDocuments(dir)
	if err != nil {
		t.Fatalf("findChecklistDocuments: %v", err)
	}

	if len(docs) != len(files) {
		t.Fatalf("expected %d documents, got %d", len(files), len(docs))
	}

	// Incomplete files up top (alphabetical), complete files (100%) at bottom (alphabetical)
	wantPaths := []string{
		"b_pending.md",
		"c_blocked.md",
		"d_half_done.md",
		"a_finished.md",
		"e_also_finished.md",
	}

	for i, want := range wantPaths {
		if docs[i].path != want {
			t.Errorf("docs[%d].path = %q, want %q", i, docs[i].path, want)
		}
	}

	// Verify completeness flags
	for i, doc := range docs[:3] {
		if doc.isComplete() {
			t.Errorf("expected docs[%d] (%s) to be incomplete", i, doc.path)
		}
	}
	for i, doc := range docs[3:] {
		if !doc.isComplete() {
			t.Errorf("expected docs[%d] (%s) to be complete", i+3, doc.path)
		}
	}
}

func TestLoadConfigOmitsDocumentWhenNotSupplied(t *testing.T) {
	// Clear DOC env
	oldDoc := os.Getenv("DOC")
	defer os.Setenv("DOC", oldDoc)
	os.Unsetenv("DOC")

	cfg, err := loadConfig([]string{"--model", "opus"})
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	if cfg.document != "" {
		t.Errorf("expected empty cfg.document, got %q", cfg.document)
	}
	if cfg.model != "opus" {
		t.Errorf("expected model opus, got %q", cfg.model)
	}
}

func TestSelectModelNavigationAndSelection(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "p1.md")
	p2 := filepath.Join(dir, "p2.md")
	p3 := filepath.Join(dir, "p3.md")
	if err := os.WriteFile(p1, []byte("## S1\n- [ ] task 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("## S2\n- [ ] task 2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p3, []byte("## S3\n- [x] task 3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	docs, err := findChecklistDocuments(dir)
	if err != nil || len(docs) != 3 {
		t.Fatalf("failed to find docs: %v, len=%d", err, len(docs))
	}

	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, docs)

	if m.mode != modeSelect {
		t.Fatalf("expected modeSelect, got %v", m.mode)
	}
	if m.selectedDocIdx != 0 {
		t.Fatalf("expected selectedDocIdx 0, got %d", m.selectedDocIdx)
	}

	// Down key moves to index 1 (incomplete p2.md)
	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = newM.(model)
	if m.selectedDocIdx != 1 {
		t.Fatalf("expected selectedDocIdx 1 after down/j, got %d", m.selectedDocIdx)
	}

	// Down key cannot move to index 2 (complete p3.md)
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = newM.(model)
	if m.selectedDocIdx != 1 {
		t.Fatalf("expected selectedDocIdx to stay 1 because p3.md is complete, got %d", m.selectedDocIdx)
	}

	// Up key moves back to index 0
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	m = newM.(model)
	if m.selectedDocIdx != 0 {
		t.Fatalf("expected selectedDocIdx 0 after up/k, got %d", m.selectedDocIdx)
	}

	// '2' key jumps to index 1
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	m = newM.(model)
	if m.selectedDocIdx != 1 {
		t.Fatalf("expected selectedDocIdx 1 after '2', got %d", m.selectedDocIdx)
	}

	// '3' key jumps to complete document -> rejected, remains at index 1
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}})
	m = newM.(model)
	if m.selectedDocIdx != 1 {
		t.Fatalf("expected selectedDocIdx to stay 1 after '3' on complete doc, got %d", m.selectedDocIdx)
	}

	// Enter selects p2.md and transitions to modeRunner
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if m.mode != modeRunner {
		t.Fatalf("expected modeRunner after enter, got %v", m.mode)
	}
	if m.cfg.document != docs[1].absPath {
		t.Fatalf("expected cfg.document %q, got %q", docs[1].absPath, m.cfg.document)
	}
}

func TestSelectModelEditorRefreshPreservesSelectedDocumentAcrossReorder(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	aDoc := filepath.Join(dir, "a_doc.md")
	bDoc := filepath.Join(dir, "b_doc.md")
	zDoc := filepath.Join(dir, "z_done.md")

	if err := os.WriteFile(aDoc, []byte("## S1\n- [ ] task a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bDoc, []byte("## S2\n- [ ] task b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zDoc, []byte("## S3\n- [x] task z\n"), 0644); err != nil {
		t.Fatal(err)
	}

	docs, err := findChecklistDocuments(".")
	if err != nil || len(docs) != 3 {
		t.Fatalf("findChecklistDocuments: %v, len=%d", err, len(docs))
	}

	// Initially: a_doc (incomplete, idx 0), b_doc (incomplete, idx 1), z_done (complete, idx 2)
	if docs[0].path != "a_doc.md" || docs[1].path != "b_doc.md" || docs[2].path != "z_done.md" {
		t.Fatalf("unexpected initial order: %+v", docs)
	}

	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, docs)

	// Currently selecting a_doc.md at idx 0
	if m.selectedDocIdx != 0 {
		t.Fatalf("expected selectedDocIdx 0, got %d", m.selectedDocIdx)
	}

	// Simulate editing a_doc.md so that it becomes 100% complete
	if err := os.WriteFile(aDoc, []byte("## S1\n- [x] task a\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// editorFinishedMsg triggers refresh
	newM, _ := m.Update(editorFinishedMsg{})
	m = newM.(model)

	// In the refreshed list:
	// b_doc.md (incomplete) is at index 0
	// a_doc.md (complete) is at index 1
	// z_done.md (complete) is at index 2
	if m.discoveredDocs[0].path != "b_doc.md" {
		t.Fatalf("expected discoveredDocs[0] to be b_doc.md, got %q", m.discoveredDocs[0].path)
	}
	if m.discoveredDocs[1].path != "a_doc.md" {
		t.Fatalf("expected discoveredDocs[1] to be a_doc.md, got %q", m.discoveredDocs[1].path)
	}

	// a_doc.md is now complete, so it is unselectable. Selection stays on index 0 (b_doc.md).
	if m.selectedDocIdx != 0 {
		t.Fatalf("expected selectedDocIdx to be 0 for b_doc.md, got %d", m.selectedDocIdx)
	}

	// Pressing Enter should now open b_doc.md
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if m.mode != modeRunner {
		t.Fatalf("expected modeRunner, got %v", m.mode)
	}
	if m.cfg.document != docs[1].absPath {
		t.Fatalf("expected cfg.document %q, got %q", docs[1].absPath, m.cfg.document)
	}
}

func TestSelectModelAllComplete(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "done1.md")
	p2 := filepath.Join(dir, "done2.md")
	if err := os.WriteFile(p1, []byte("## S1\n- [x] task 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte("## S2\n- [x] task 2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	docs, err := findChecklistDocuments(dir)
	if err != nil || len(docs) != 2 {
		t.Fatalf("failed to find docs: %v", err)
	}

	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, docs)

	if m.selectedDocIdx != -1 {
		t.Fatalf("expected selectedDocIdx -1 when all docs are complete, got %d", m.selectedDocIdx)
	}
	if m.status != "All documents are complete" {
		t.Fatalf("expected status 'All documents are complete', got %q", m.status)
	}

	// Navigation keys do nothing
	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = newM.(model)
	if m.selectedDocIdx != -1 {
		t.Fatalf("expected selectedDocIdx still -1 after j, got %d", m.selectedDocIdx)
	}

	// Enter does nothing
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if m.mode != modeSelect {
		t.Fatalf("expected modeSelect, got %v", m.mode)
	}
}

func TestSelectModelQuitsOnQ(t *testing.T) {
	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, []checklistDoc{{path: "test.md", absPath: "/tmp/test.md"}})

	newM, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m = newM.(model)
	if m.exitCode != 0 {
		t.Fatalf("expected exitCode 0, got %d", m.exitCode)
	}
	if cmd == nil {
		t.Fatal("expected quit cmd, got nil")
	}
}

func TestSelectViewRendersRelativePathsAndCompleteness(t *testing.T) {
	doc1 := document{total: 10, done: 5, pending: 4, blocked: 1}
	doc2 := document{total: 5, done: 5}
	docs := []checklistDoc{
		{path: "docs/plan.md", absPath: "/tmp/docs/plan.md", doc: doc1},
		{path: "root-tasks.md", absPath: "/tmp/root-tasks.md", doc: doc2},
	}
	m := newSelectModel(config{document: ""}, docs)
	m.width = 100
	m.height = 30

	view := m.View()
	if !strings.Contains(view, "docs/plan.md") {
		t.Errorf("view does not contain relative path docs/plan.md:\n%s", view)
	}
	if !strings.Contains(view, "root-tasks.md") {
		t.Errorf("view does not contain relative path root-tasks.md:\n%s", view)
	}
	if !strings.Contains(view, "50%") || !strings.Contains(view, "5/10 completed") {
		t.Errorf("view does not contain completeness for docs/plan.md:\n%s", view)
	}
	if !strings.Contains(view, "100%") || !strings.Contains(view, "5/5 completed") {
		t.Errorf("view does not contain completeness for root-tasks.md:\n%s", view)
	}
}
func TestSelectViewRendersSpaceAndGreyedOutForCompleteDocs(t *testing.T) {
	doc1 := document{total: 4, done: 2, pending: 2}
	doc2 := document{total: 5, done: 5}
	docs := []checklistDoc{
		{path: "active.md", absPath: "/tmp/active.md", doc: doc1},
		{path: "finished.md", absPath: "/tmp/finished.md", doc: doc2},
	}
	m := newSelectModel(config{document: ""}, docs)
	m.width = 100
	m.height = 30

	view := m.View()
	// Complete doc should not have a number prefix like " 2. finished.md"
	if strings.Contains(view, "2. finished.md") {
		t.Errorf("complete document should not be numbered:\n%s", view)
	}
	if !strings.Contains(view, "finished.md") {
		t.Errorf("complete document should be listed:\n%s", view)
	}

	lines := strings.Split(view, "\n")
	activeLine := -1
	finishedLine := -1
	for i, l := range lines {
		if strings.Contains(l, "active.md") {
			activeLine = i
		}
		if strings.Contains(l, "finished.md") {
			finishedLine = i
		}
	}
	if activeLine == -1 || finishedLine == -1 {
		t.Fatalf("could not find lines for active.md and finished.md:\n%s", view)
	}
	if finishedLine != activeLine+2 {
		t.Fatalf("expected finished.md to be exactly 2 lines after active.md (with 1 blank line between), got active=%d, finished=%d:\n%s", activeLine, finishedLine, view)
	}

	// Verify the intervening line is actually blank (excluding panel borders/spaces)
	intervening := strings.Trim(lines[activeLine+1], "│ ")
	if intervening != "" {
		t.Errorf("expected blank line between active.md and finished.md, got %q", lines[activeLine+1])
	}

	// Verify greyed-out muted styling on completed row
	expectedMutedComp := mutedStyle.Render("[100%] 5/5 completed")
	if !strings.Contains(lines[finishedLine], expectedMutedComp) {
		t.Errorf("expected finished.md line to contain muted completion string %q, line was:\n%s", expectedMutedComp, lines[finishedLine])
	}

	// Verify active row is not wholly muted (contains active badge)
	expectedActiveBadge := activeStyle.Render("[ 50%]")
	if !strings.Contains(lines[activeLine], expectedActiveBadge) {
		t.Errorf("expected active.md line to contain active badge %q, line was:\n%s", expectedActiveBadge, lines[activeLine])
	}
}

func TestSelectModelAllCompleteScroll(t *testing.T) {
	var docs []checklistDoc
	for i := range 15 {
		docs = append(docs, checklistDoc{
			path:    fmt.Sprintf("done_%02d.md", i),
			absPath: fmt.Sprintf("/tmp/done_%02d.md", i),
			doc:     document{total: 2, done: 2},
		})
	}

	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, docs)
	m.width = 100
	m.height = 10 // small height so visibleHeight is ~4 lines

	if m.selectedDocIdx != -1 {
		t.Fatalf("expected selectedDocIdx -1 for all complete, got %d", m.selectedDocIdx)
	}

	view := m.View()
	if !strings.Contains(view, "done_00.md") {
		t.Errorf("initial view should show done_00.md:\n%s", view)
	}
	if !strings.Contains(view, "more below") {
		t.Errorf("initial view should indicate more below:\n%s", view)
	}
	if strings.Contains(view, "done_14.md") {
		t.Errorf("done_14.md should not be visible before scrolling:\n%s", view)
	}
	// Scroll down step-by-step using 'j' to verify repeated j reaches the end
	for range 20 {
		newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = newM.(model)
	}

	viewScrolled := m.View()
	if !strings.Contains(viewScrolled, "done_14.md") {
		t.Errorf("scrolled view after repeated 'j' should show final doc done_14.md:\n%s", viewScrolled)
	}
	if !strings.Contains(viewScrolled, "more above") {
		t.Errorf("scrolled view should indicate more above:\n%s", viewScrolled)
	}

	// Selection should remain -1
	if m.selectedDocIdx != -1 {
		t.Fatalf("selectedDocIdx should remain -1 after scrolling, got %d", m.selectedDocIdx)
	}

	// Enter key should not select or transition
	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if m.mode != modeSelect {
		t.Fatalf("enter should not leave modeSelect when all are complete, got %v", m.mode)
	}
}

func TestSelectModelMixedScrollPastSelectable(t *testing.T) {
	dir := t.TempDir()
	p0 := filepath.Join(dir, "incomplete_0.md")
	p1 := filepath.Join(dir, "incomplete_1.md")
	if err := os.WriteFile(p0, []byte("## S0\n- [ ] task 0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p1, []byte("## S1\n- [ ] task 1\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var docs []checklistDoc
	// 2 incomplete docs
	docs = append(docs, checklistDoc{
		path:    "incomplete_0.md",
		absPath: p0,
		doc:     document{total: 2, done: 1, pending: 1},
	})
	docs = append(docs, checklistDoc{
		path:    "incomplete_1.md",
		absPath: p1,
		doc:     document{total: 2, done: 0, pending: 2},
	})
	// 10 complete docs
	for i := range 10 {
		docs = append(docs, checklistDoc{
			path:    fmt.Sprintf("done_%02d.md", i),
			absPath: filepath.Join(dir, fmt.Sprintf("done_%02d.md", i)),
			doc:     document{total: 3, done: 3},
		})
	}

	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, docs)
	m.width = 100
	m.height = 10 // small height

	if m.selectedDocIdx != 0 {
		t.Fatalf("expected selectedDocIdx 0, got %d", m.selectedDocIdx)
	}

	// Move down to index 1 (second incomplete doc)
	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = newM.(model)
	if m.selectedDocIdx != 1 {
		t.Fatalf("expected selectedDocIdx 1, got %d", m.selectedDocIdx)
	}

	// Pressing 'j' again cannot advance selectedDocIdx (p3 is complete)
	// but it scrolls down
	initScroll := m.selectScroll
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = newM.(model)
	if m.selectedDocIdx != 1 {
		t.Fatalf("selectedDocIdx should stay 1, got %d", m.selectedDocIdx)
	}
	if m.selectScroll <= initScroll {
		t.Fatalf("selectScroll should have incremented to reveal complete docs, was %d, now %d", initScroll, m.selectScroll)
	}

	// Repeatedly press 'j' until bottom scroll is reached
	for range 15 {
		newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
		m = newM.(model)
		if m.selectedDocIdx != 1 {
			t.Fatalf("selectedDocIdx must remain on selectable doc (1), got %d", m.selectedDocIdx)
		}
	}
	finalView := m.View()
	if !strings.Contains(finalView, "done_09.md") {
		t.Errorf("final completed doc done_09.md should be visible after repeated 'j':\n%s", finalView)
	}

	// Incomplete docs are now off-screen: enter and e must not act on off-screen selection
	if strings.Contains(finalView, "incomplete_1.md") {
		t.Fatalf("incomplete_1.md should be off-screen at bottom scroll, but was in view:\n%s", finalView)
	}
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if m.mode != modeSelect {
		t.Fatalf("enter must not launch off-screen document; expected modeSelect, got %v", m.mode)
	}

	newM, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = newM.(model)
	if cmd != nil {
		t.Fatal("e must not open editor on off-screen document")
	}

	// Pressing home ('g') reveals the selected document again
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	m = newM.(model)
	homeView := m.View()
	if !strings.Contains(homeView, "incomplete_0.md") {
		t.Fatalf("homeView should contain incomplete_0.md:\n%s", homeView)
	}

	// Now enter works to launch the visible selected document
	newM, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = newM.(model)
	if m.mode != modeRunner {
		t.Fatalf("expected modeRunner after enter on visible selection, got %v", m.mode)
	}
}

func TestSelectModelWindowResizeClampsScroll(t *testing.T) {
	var docs []checklistDoc
	for i := range 10 {
		docs = append(docs, checklistDoc{
			path:    fmt.Sprintf("done_%02d.md", i),
			absPath: fmt.Sprintf("/tmp/done_%02d.md", i),
			doc:     document{total: 1, done: 1},
		})
	}

	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, docs)
	m.width = 100
	m.height = 10 // small height

	// Scroll to bottom
	newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	m = newM.(model)
	if m.selectScroll == 0 {
		t.Fatal("expected selectScroll > 0 after scrolling to bottom")
	}

	// Enlarge terminal height to 40 so all 10 docs fit without scrolling
	newM, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m = newM.(model)
	if m.selectScroll != 0 {
		t.Fatalf("expected selectScroll clamped to 0 after enlarging window, got %d", m.selectScroll)
	}
}

func TestSelectModelManyIncompleteNavigation(t *testing.T) {
	dir := t.TempDir()
	var docs []checklistDoc
	for i := range 15 {
		p := filepath.Join(dir, fmt.Sprintf("task_%02d.md", i))
		if err := os.WriteFile(p, []byte("## S\n- [ ] task\n"), 0644); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, checklistDoc{
			path:    fmt.Sprintf("task_%02d.md", i),
			absPath: p,
			doc:     document{total: 1, pending: 1},
		})
	}

	cfg := config{ompPath: "/bin/echo"}
	m := newSelectModel(cfg, docs)
	m.width = 100
	m.height = 9 // small height: visibleHeight is ~3-4 lines

	// Step-by-step navigate down through all 15 incomplete items
	for expectedIdx := range 15 {
		if m.selectedDocIdx != expectedIdx {
			t.Fatalf("expected selectedDocIdx %d, got %d", expectedIdx, m.selectedDocIdx)
		}
		if !m.isSelectedDocVisible() {
			t.Fatalf("cursor at idx %d must be visible in viewport, scroll=%d", expectedIdx, m.selectScroll)
		}
		view := m.View()
		expectedName := fmt.Sprintf("task_%02d.md", expectedIdx)
		if !strings.Contains(view, expectedName) {
			t.Fatalf("view at step %d must contain %s:\n%s", expectedIdx, expectedName, view)
		}
		if !strings.Contains(view, "> ") {
			t.Fatalf("view at step %d must show active cursor '> ':\n%s", expectedIdx, view)
		}

		if expectedIdx < 14 {
			newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
			m = newM.(model)
		}
	}

	// Step-by-step navigate back up to 0
	for expectedIdx := 14; expectedIdx >= 0; expectedIdx-- {
		if m.selectedDocIdx != expectedIdx {
			t.Fatalf("expected selectedDocIdx %d while moving up, got %d", expectedIdx, m.selectedDocIdx)
		}
		if !m.isSelectedDocVisible() {
			t.Fatalf("cursor at idx %d must be visible in viewport while moving up, scroll=%d", expectedIdx, m.selectScroll)
		}
		view := m.View()
		expectedName := fmt.Sprintf("task_%02d.md", expectedIdx)
		if !strings.Contains(view, expectedName) {
			t.Fatalf("view at step %d must contain %s while moving up:\n%s", expectedIdx, expectedName, view)
		}
		if expectedIdx > 0 {
			newM, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
			m = newM.(model)
		}
	}
}
func TestStopCaffeinateTerminatesChild(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	stopCaffeinate(cmd)

	if cmd.ProcessState == nil {
		t.Fatal("caffeinate child was not reaped")
	}
}

func TestRenderOMPEvent(t *testing.T) {
	tool := renderOMPEvent([]byte(`{"type":"tool_execution_start","toolName":"read","args":{"i":"Reading service"}}`))
	if len(tool) != 1 || tool[0].kind != outputText || tool[0].text != "> read · Reading service" {
		t.Fatalf("unexpected tool event: %#v", tool)
	}

	delta := renderOMPEvent([]byte(`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"implemented"}}`))
	if len(delta) != 1 || delta[0].kind != outputDelta || delta[0].text != "implemented" {
		t.Fatalf("unexpected delta event: %#v", delta)
	}

	plain := renderOMPEvent([]byte(`not-json`))
	if len(plain) != 1 || plain[0].text != "not-json" {
		t.Fatalf("non-JSON output was lost: %#v", plain)
	}
}

func TestCollectAgentOutputWaitsForReadersBeforeWait(t *testing.T) {
	cmd := exec.Command("sh", "-c", `printf '%s\n' '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}'`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	events := make(chan agentOutput, 8)
	collectAgentOutput(context.Background(), cmd, stdout, stderr, events)

	var sawDelta bool
	var sawDone bool
	for event := range events {
		if strings.Contains(event.text, "could not read") {
			t.Fatalf("successful process emitted pipe read error: %q", event.text)
		}
		if event.kind == outputDelta && event.text == "done" {
			sawDelta = true
		}
		if event.kind == outputDone && event.exitCode == 0 {
			sawDone = true
		}
	}
	if !sawDelta || !sawDone {
		t.Fatalf("missing expected output: delta=%v done=%v", sawDelta, sawDone)
	}
}

func TestFinishRunContinuesWhenPendingTasksRemainWithBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.md")
	content := "## Section 1\n- [ ] task 1\n\n## Section 2\n- [ ] task 2\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	m := model{cfg: config{document: path, stallLimit: 3}, doc: doc, autoRun: true, runStartPending: 2}

	// Section 1 has a blocked task, but Section 2 still has a pending task
	updatedContent := "## Section 1\n- [!] task 1 — needs credentials\n\n## Section 2\n- [ ] task 2\n"
	if err := os.WriteFile(path, []byte(updatedContent), 0o600); err != nil {
		t.Fatal(err)
	}

	updated, cmd := m.finishRun(0)
	got := updated.(model)
	if cmd == nil {
		t.Fatal("loop should continue to next section when pending tasks remain")
	}
	if got.doc.blocked != 1 || got.doc.pending != 1 {
		t.Fatalf("unexpected doc state: blocked=%d pending=%d", got.doc.blocked, got.doc.pending)
	}
	if !strings.Contains(got.status, "1 remain (1 blocked)") {
		t.Fatalf("status did not indicate remaining tasks: %q", got.status)
	}
}

func TestFinishRunSetsNeedsInputWhenAllPendingTasksComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.md")
	content := "## Work\n- [ ] task 1\n- [ ] task 2\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	m := model{cfg: config{document: path, stallLimit: 3}, doc: doc, autoRun: true, runStartPending: 2}

	// All pending tasks are done or blocked
	updatedContent := "## Work\n- [x] task 1\n- [!] task 2 — needs confirmation\n"
	if err := os.WriteFile(path, []byte(updatedContent), 0o600); err != nil {
		t.Fatal(err)
	}

	updated, cmd := m.finishRun(0)
	got := updated.(model)
	if cmd != nil {
		t.Fatal("all pending tasks done should not schedule next run")
	}
	if got.doc.pending != 0 || got.doc.blocked != 1 {
		t.Fatalf("unexpected counts: pending=%d blocked=%d", got.doc.pending, got.doc.blocked)
	}
	if !strings.Contains(got.status, "All pending tasks complete") || !strings.Contains(got.status, "need input") {
		t.Fatalf("status did not indicate pending tasks complete and input needed: %q", got.status)
	}
}

func TestFinishRunUsesStartCountAfterLiveRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte("## Work\n- [x] first\n- [ ] second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveDoc, err := readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	m := model{
		cfg:             config{document: path, stallLimit: 3},
		doc:             liveDoc,
		autoRun:         true,
		runStartPending: 2,
		stall:           2,
	}

	updated, cmd := m.finishRun(0)
	got := updated.(model)
	if cmd == nil {
		t.Fatal("successful progress did not schedule the next run")
	}
	if got.stall != 0 || !strings.Contains(got.status, "Completed 1 task") {
		t.Fatalf("live refresh hid run progress: stall=%d status=%q", got.stall, got.status)
	}
}

func TestFinishRunStaysOpenWhenAllTasksComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte("## Work\n- [ ] task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	m := model{cfg: config{document: path, stallLimit: 3}, doc: doc, autoRun: true, runStartPending: 1}
	if err := os.WriteFile(path, []byte("## Work\n- [x] task\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	updated, cmd := m.finishRun(0)
	got := updated.(model)
	if cmd != nil {
		t.Fatal("completed document should remain open for inspection")
	}
	if got.doc.pending != 0 || got.doc.blocked != 0 || got.status != "All tasks complete" {
		t.Fatalf("completed document not terminal: %+v", got)
	}
}

func TestUpdateTaskInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.md")
	initial := `## Section
- [x] task 1
- [!] task 2 - **Issue:** needs confirmation
- [ ] task 3
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	targetTask := taskItem{line: 3, text: "task 2 - **Issue:** needs confirmation", state: taskBlocked}

	// 1. Mark task 2 as Done (- [x])
	if err := updateTaskInFile(path, targetTask, taskDone, ""); err != nil {
		t.Fatal(err)
	}
	doc, err := readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.done != 2 || doc.blocked != 0 || doc.pending != 1 {
		t.Fatalf("unexpected counts after taskDone: %+v", doc)
	}

	// 2. Mark task 2 as Pending with User Input note (even if line number shifted from 3 to 4)
	shiftedTask := taskItem{line: 99, text: "task 2 - **Issue:** needs confirmation", state: taskBlocked}
	if err := updateTaskInFile(path, shiftedTask, taskPending, "User confirmed it works"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "- [ ] task 2 - **Issue:** needs confirmation - **User input:** User confirmed it works") {
		t.Fatalf("unexpected content after user input update: %s", string(content))
	}
	doc, err = readDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.done != 1 || doc.blocked != 0 || doc.pending != 2 {
		t.Fatalf("unexpected counts after user input: %+v", doc)
	}
}

func TestSplitTaskAndIssue(t *testing.T) {
	tests := []struct {
		input     string
		wantTask  string
		wantIssue string
	}{
		{
			input:     "Verify shell window - **Issue / Needs User Confirmation:** manual test required",
			wantTask:  "Verify shell window",
			wantIssue: "**Issue / Needs User Confirmation:** manual test required",
		},
		{
			input:     "Fix auth — needs API key",
			wantTask:  "Fix auth",
			wantIssue: "needs API key",
		},
		{
			input:     "Plain task without issue",
			wantTask:  "Plain task without issue",
			wantIssue: "",
		},
	}

	for _, tt := range tests {
		gotTask, gotIssue := splitTaskAndIssue(tt.input)
		if gotTask != tt.wantTask || gotIssue != tt.wantIssue {
			t.Errorf("splitTaskAndIssue(%q) = (%q, %q), want (%q, %q)", tt.input, gotTask, gotIssue, tt.wantTask, tt.wantIssue)
		}
	}
}
func TestParseModelFromArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "space separated long flag",
			args: []string{"--model", "opus"},
			want: "opus",
		},
		{
			name: "equal separated long flag",
			args: []string{"--model=claude-3-7-sonnet"},
			want: "claude-3-7-sonnet",
		},
		{
			name: "space separated short flag",
			args: []string{"-m", "gpt-4o"},
			want: "gpt-4o",
		},
		{
			name: "equal separated short flag",
			args: []string{"-m=gemini-2.5"},
			want: "gemini-2.5",
		},
		{
			name: "multiple flags uses last",
			args: []string{"--model", "opus", "-m=sonnet"},
			want: "sonnet",
		},
		{
			name: "no model flag",
			args: []string{"--auto-approve", "extra"},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseModelFromArgs(tt.args)
			if got != tt.want {
				t.Errorf("parseModelFromArgs(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseDefaultModelFromYAML(t *testing.T) {
	yamlWithRoles := []byte(`
setupVersion: 2
modelRoles:
  smol: google-antigravity/gemini-3.5-flash-lite
  default: google-antigravity/gemini-3.8-flash:high
  advisor: anthropic/claude-opus-5
`)
	if got := parseDefaultModelFromYAML(yamlWithRoles); got != "google-antigravity/gemini-3.8-flash:high" {
		t.Errorf("parseDefaultModelFromYAML(yamlWithRoles) = %q, want %q", got, "google-antigravity/gemini-3.8-flash:high")
	}

	yamlWithTopLevel := []byte(`
model: "anthropic/claude-3-5-sonnet"
`)
	if got := parseDefaultModelFromYAML(yamlWithTopLevel); got != "anthropic/claude-3-5-sonnet" {
		t.Errorf("parseDefaultModelFromYAML(yamlWithTopLevel) = %q, want %q", got, "anthropic/claude-3-5-sonnet")
	}

	yamlEmpty := []byte(``)
	if got := parseDefaultModelFromYAML(yamlEmpty); got != "" {
		t.Errorf("parseDefaultModelFromYAML(empty) = %q, want empty", got)
	}
}

func TestRenderOMPEventExtractsModel(t *testing.T) {
	modelChange := renderOMPEvent([]byte(`{"type":"model_change","model":"google-antigravity/gemini-3.8-flash"}`))
	if len(modelChange) != 1 || modelChange[0].kind != outputModel || modelChange[0].text != "google-antigravity/gemini-3.8-flash" {
		t.Fatalf("unexpected model_change event: %#v", modelChange)
	}

	msgStart := renderOMPEvent([]byte(`{"type":"message_start","message":{"role":"assistant","model":"gemini-3.8-flash"}}`))
	if len(msgStart) != 1 || msgStart[0].kind != outputModel || msgStart[0].text != "gemini-3.8-flash" {
		t.Fatalf("unexpected message_start event: %#v", msgStart)
	}

	combined := renderOMPEvent([]byte(`{"type":"message_update","model":"gemini-3.8-flash","assistantMessageEvent":{"type":"text_delta","delta":"done"}}`))
	if len(combined) != 2 {
		t.Fatalf("expected 2 outputs for combined event, got %d: %#v", len(combined), combined)
	}
	if combined[0].kind != outputModel || combined[0].text != "gemini-3.8-flash" {
		t.Errorf("unexpected first output: %#v", combined[0])
	}
	if combined[1].kind != outputDelta || combined[1].text != "done" {
		t.Errorf("unexpected second output: %#v", combined[1])
	}
}

func TestRenderHeaderTitle(t *testing.T) {
	m := model{modelName: "google-antigravity/gemini-3.8-flash:high"}
	base := " LOOP test.md"

	// Generous width: shows full model
	wide := m.renderHeaderTitle(base, 80)
	if !strings.Contains(wide, "google-antigravity/gemini-3.8-flash:high") {
		t.Errorf("expected full model in wide header, got %q", wide)
	}

	// Medium width: strips provider prefix
	medium := m.renderHeaderTitle(base, 45)
	if !strings.Contains(medium, "gemini-3.8-flash:high") || strings.Contains(medium, "google-antigravity") {
		t.Errorf("expected stripped provider in medium header, got %q", medium)
	}

	// Very tight width: drops model rather than overflowing base
	tiny := m.renderHeaderTitle(base, 15)
	if strings.Contains(tiny, "gemini") {
		t.Errorf("expected model dropped in tiny header, got %q", tiny)
	}
	if !strings.Contains(tiny, "LOOP") {
		t.Errorf("expected base title preserved in tiny header, got %q", tiny)
	}

	// Empty modelName: renders base
	mEmpty := model{}
	empty := mEmpty.renderHeaderTitle(base, 80)
	if !strings.Contains(empty, "LOOP test.md") || strings.Contains(empty, "·") {
		t.Errorf("expected bare base title when model empty, got %q", empty)
	}
}

func TestHeaderViewIncludesModel(t *testing.T) {
	m := model{
		cfg:       config{document: "/tmp/migration.md"},
		modelName: "gemini-3.8-flash",
		doc:       document{done: 2, total: 5},
	}

	runnerHeader := m.headerView(80)
	if !strings.Contains(runnerHeader, "migration.md") {
		t.Errorf("expected document in header, got %q", runnerHeader)
	}
	if !strings.Contains(runnerHeader, "gemini-3.8-flash") {
		t.Errorf("expected model in runner header, got %q", runnerHeader)
	}
	if !strings.Contains(runnerHeader, "2/5 complete") {
		t.Errorf("expected progress in runner header, got %q", runnerHeader)
	}

	reviewHeader := m.reviewHeaderView(80, 1)
	if !strings.Contains(reviewHeader, "Review Mode") {
		t.Errorf("expected Review Mode in header, got %q", reviewHeader)
	}
	if !strings.Contains(reviewHeader, "gemini-3.8-flash") {
		t.Errorf("expected model in review header, got %q", reviewHeader)
	}
}
func TestLoadConfigWithModel(t *testing.T) {
	tempDoc := filepath.Join(t.TempDir(), "test-plan.md")
	if err := os.WriteFile(tempDoc, []byte("# Test\n## S1\n- [ ] task\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig([]string{tempDoc, "--model", "opus"})
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}

	if cfg.model != "opus" {
		t.Errorf("cfg.model = %q, want %q", cfg.model, "opus")
	}
	if !reflect.DeepEqual(cfg.extraArgs, []string{"--model", "opus"}) {
		t.Errorf("cfg.extraArgs = %#v, want %#v", cfg.extraArgs, []string{"--model", "opus"})
	}

	cfgPlain, err := loadConfig([]string{tempDoc})
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}
	// Verify extraArgs does NOT contain --model injected
	for _, arg := range cfgPlain.extraArgs {
		if arg == "--model" || arg == "-m" {
			t.Errorf("unexpected model flag injected into extraArgs: %#v", cfgPlain.extraArgs)
		}
	}
}
func TestViewRendersModelInRunnerAndReviewModes(t *testing.T) {
	tempDoc := filepath.Join(t.TempDir(), "test-view.md")
	content := `# Project
## Section 1
- [ ] pending task
- [!] blocked task — need user confirmation
`
	if err := os.WriteFile(tempDoc, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		document:  tempDoc,
		model:     "opus",
		extraArgs: []string{"--model", "opus"},
	}

	m, err := newModel(cfg)
	if err != nil {
		t.Fatalf("newModel failed: %v", err)
	}
	m.width = 100
	m.height = 30

	runnerView := m.View()
	if !strings.Contains(runnerView, "test-view.md") {
		t.Errorf("runner view missing document: %q", runnerView)
	}
	if !strings.Contains(runnerView, "opus") {
		t.Errorf("runner view missing model: %q", runnerView)
	}

	m.mode = modeReview
	reviewView := m.View()
	if !strings.Contains(reviewView, "test-view.md") {
		t.Errorf("review view missing document: %q", reviewView)
	}
	if !strings.Contains(reviewView, "Review Mode") {
		t.Errorf("review view missing Review Mode: %q", reviewView)
	}
	if !strings.Contains(reviewView, "opus") {
		t.Errorf("review view missing model: %q", reviewView)
	}
}

func TestDemoDocumentParsing(t *testing.T) {
	doc, err := readDocument("demo.md")
	if err != nil {
		t.Fatalf("failed to read demo.md: %v", err)
	}
	if doc.total != 15 {
		t.Errorf("expected 15 total tasks, got %d", doc.total)
	}
	if doc.done != 2 {
		t.Errorf("expected 2 done tasks, got %d", doc.done)
	}
	if doc.blocked != 1 {
		t.Errorf("expected 1 blocked task, got %d", doc.blocked)
	}
	if doc.pending != 12 {
		t.Errorf("expected 12 pending tasks, got %d", doc.pending)
	}
	if len(doc.sections) != 4 {
		t.Errorf("expected 4 sections, got %d", len(doc.sections))
	}
	next, ok := doc.nextSection()
	if !ok || next.title != "Phase 1: Database & Storage Engine" {
		t.Errorf("expected next section to be Phase 1, got %+v", next)
	}
	blocked := doc.blockedTasks()
	if len(blocked) != 1 {
		t.Fatalf("expected 1 blocked task, got %d", len(blocked))
	}
	taskPart, issuePart := splitTaskAndIssue(blocked[0].text)
	if !strings.Contains(taskPart, "Configure production SendGrid API key") {
		t.Errorf("unexpected task part: %q", taskPart)
	}
	if !strings.Contains(issuePart, "Requires administrator to provision production credential from vault") {
		t.Errorf("unexpected issue part: %q", issuePart)
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{0, "0"},
		{-5, "0"},
		{500, "500"},
		{1200, "1.2k"},
		{9800, "9.8k"},
		{45000, "45k"},
		{100000, "100k"},
		{1500000, "1.5m"},
	}

	for _, tc := range tests {
		if got := formatTokens(tc.input); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestRenderOMPEventExtractsUsage(t *testing.T) {
	jsonLine := []byte(`{"type":"turn_end","message":{"role":"assistant","usage":{"input":4158,"cacheRead":16312,"output":350,"totalTokens":20820}}}`)
	outputs := renderOMPEvent(jsonLine)
	var foundUsage *contextUsage
	for _, out := range outputs {
		if out.kind == outputUsage {
			u := out.usage
			foundUsage = &u
			break
		}
	}
	if foundUsage == nil {
		t.Fatal("expected outputUsage event from turn_end with usage")
	}
	if foundUsage.Input != 4158 || foundUsage.CacheRead != 16312 || foundUsage.Output != 350 || foundUsage.TotalTokens != 20820 {
		t.Fatalf("unexpected usage: %+v", foundUsage)
	}
	if got := foundUsage.ActiveContext(); got != 4158+16312 {
		t.Fatalf("ActiveContext() = %d, want %d", got, 4158+16312)
	}
}

func TestRenderHeaderTitleIncludesContextUsage(t *testing.T) {
	m := model{
		cfg:           config{},
		modelName:     "gemini-3.8-flash",
		contextTokens: 45000,
	}
	base := " LOOP tasks.md"
	rendered := m.renderHeaderTitle(base, 100)
	if !strings.Contains(rendered, "45k ctx") {
		t.Errorf("expected header to contain context info '45k ctx', got %q", rendered)
	}
	if !strings.Contains(rendered, "gemini-3.8-flash") {
		t.Errorf("expected header to contain model name, got %q", rendered)
	}
}



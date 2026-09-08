package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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

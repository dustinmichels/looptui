package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

type outputKind byte

const (
	outputText outputKind = iota
	outputDelta
	outputDone
)

type agentOutput struct {
	kind     outputKind
	text     string
	exitCode int
}

type agentRun struct {
	events <-chan agentOutput
	cancel context.CancelFunc
}

type agentStartedMsg struct {
	run *agentRun
	err error
}

type agentOutputMsg struct {
	output agentOutput
}

type ompEvent struct {
	Type                  string          `json:"type"`
	ToolName              string          `json:"toolName,omitempty"`
	Intent                string          `json:"intent,omitempty"`
	Args                  json.RawMessage `json:"args,omitempty"`
	IsError               bool            `json:"isError,omitempty"`
	AssistantMessageEvent *struct {
		Type  string `json:"type"`
		Delta string `json:"delta,omitempty"`
	} `json:"assistantMessageEvent,omitempty"`
}

func startAgent(cfg config, target section) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithCancel(context.Background())
		args := buildOMPArgs(cfg, target)
		cmd := exec.CommandContext(ctx, cfg.ompPath, args...)

		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			return agentStartedMsg{err: fmt.Errorf("capture omp output: %w", err)}
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			cancel()
			return agentStartedMsg{err: fmt.Errorf("capture omp errors: %w", err)}
		}
		if err := cmd.Start(); err != nil {
			cancel()
			return agentStartedMsg{err: fmt.Errorf("start omp: %w", err)}
		}

		events := make(chan agentOutput, 256)
		go func() {
			defer cancel()
			collectAgentOutput(ctx, cmd, stdout, stderr, events)
		}()
		return agentStartedMsg{run: &agentRun{events: events, cancel: cancel}}
	}
}

func buildOMPArgs(cfg config, target section) []string {
	args := []string{"--mode", "json", "-p"}
	if cfg.autoApprove {
		args = append(args, "--auto-approve")
	}
	args = append(args, "@"+cfg.document, sectionPrompt(cfg.document, target))
	return append(args, cfg.extraArgs...)
}

func sectionPrompt(documentPath string, target section) string {
	return fmt.Sprintf(`Work only on the next incomplete document section: %q (starts at line %d in %s).
Implement its unchecked tasks in order and update each completed checkbox to '- [x]' in the document as you go. Do not start a later section in this run. If a task cannot be completed or requires user input, rewrite its checkbox as '- [!]' with the blocking issue inline on the same bullet, then continue with the remaining unchecked tasks in this section. Never leave '- [ ]' on work you already completed or determined is blocked.`, target.title, target.line, documentPath)
}

func collectAgentOutput(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.ReadCloser, events chan<- agentOutput) {
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		readJSONStream(stdout, events)
	}()
	go func() {
		defer readers.Done()
		readPlainStream(stderr, events)
	}()

	readers.Wait()
	err := cmd.Wait()
	exitCode := 0
	if err != nil {
		exitCode = 1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			events <- agentOutput{kind: outputText, text: fmt.Sprintf("omp failed: %v", err)}
		}
	}
	events <- agentOutput{kind: outputDone, exitCode: exitCode}
	close(events)
}

func readJSONStream(reader io.Reader, events chan<- agentOutput) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 256*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		outputs := renderOMPEvent(line)
		for _, output := range outputs {
			events <- output
		}
	}
	if err := scanner.Err(); err != nil {
		events <- agentOutput{kind: outputText, text: fmt.Sprintf("could not read omp output: %v", err)}
	}
}

func readPlainStream(reader io.Reader, events chan<- agentOutput) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			events <- agentOutput{kind: outputText, text: "! " + line}
		}
	}
	if err := scanner.Err(); err != nil {
		events <- agentOutput{kind: outputText, text: fmt.Sprintf("could not read omp errors: %v", err)}
	}
}

func renderOMPEvent(line []byte) []agentOutput {
	var event ompEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return []agentOutput{{kind: outputText, text: string(line)}}
	}

	switch event.Type {
	case "tool_execution_start":
		intent := event.Intent
		if intent == "" {
			var args struct {
				Intent string `json:"i"`
			}
			_ = json.Unmarshal(event.Args, &args)
			intent = args.Intent
		}
		if intent == "" {
			intent = "running"
		}
		return []agentOutput{{kind: outputText, text: fmt.Sprintf("> %s · %s", event.ToolName, intent)}}
	case "tool_execution_end":
		marker := "+"
		if event.IsError {
			marker = "x"
		}
		return []agentOutput{{kind: outputText, text: fmt.Sprintf("%s %s", marker, event.ToolName)}}
	case "message_update":
		if event.AssistantMessageEvent != nil && event.AssistantMessageEvent.Type == "text_delta" && event.AssistantMessageEvent.Delta != "" {
			return []agentOutput{{kind: outputDelta, text: event.AssistantMessageEvent.Delta}}
		}
	}
	return nil
}

func waitForAgent(events <-chan agentOutput) tea.Cmd {
	return func() tea.Msg {
		output, ok := <-events
		if !ok {
			return agentOutputMsg{output: agentOutput{kind: outputDone, exitCode: 1}}
		}
		return agentOutputMsg{output: output}
	}
}

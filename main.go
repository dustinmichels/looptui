package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type config struct {
	document      string
	ompPath       string
	extraArgs     []string
	maxIterations int
	stallLimit    int
	sleep         time.Duration
	autoApprove   bool
}

const usage = `Usage: looptui [document.md] [omp args...]

Run omp repeatedly over markdown sections with unchecked tasks.

Document format:
  ## Section title
  - [ ] pending task
  - [x] completed task
  - [!] blocked task (skipped during run; saved for user input)

If document is omitted, looptui uses $DOC, then requirements.md if present, otherwise migration.md.

Controls:
  space        pause / resume automatic runs
  r            retry or start next run
  i / tab      review and resolve blocked tasks needing user input
  e            open document in $EDITOR
  j/k, up/down scroll agent output (or navigate blocked tasks in review mode)
  q, ctrl+c    quit

Environment:
  AUTO_APPROVE=0       omit --auto-approve when launching omp
  OMP_BIN=/path/omp    use a specific omp binary
  MAX_ITERATIONS=N     stop after N omp runs; 0 means unlimited
  STALL_LIMIT=N        stop after N consecutive runs with no checkbox progress; default 3
  SLEEP_SECONDS=N      delay between automatic runs; default 2
  DOC=path/to/file.md  default document when no document argument is supplied
`

func main() {
	if helpRequested(os.Args[1:]) {
		fmt.Print(usage)
		return
	}

	cfg, err := loadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "looptui:", err)
		os.Exit(1)
	}

	app, err := newModel(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "looptui:", err)
		os.Exit(1)
	}

	program := tea.NewProgram(app, tea.WithAltScreen())
	final, err := program.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "looptui:", err)
		os.Exit(1)
	}
	if m, ok := final.(model); ok && m.exitCode != 0 {
		os.Exit(m.exitCode)
	}
}

func helpRequested(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "-h", "--help", "help":
		return true
	default:
		return false
	}
}

func loadConfig(args []string) (config, error) {
	cfg := config{
		stallLimit:  3,
		sleep:       2 * time.Second,
		autoApprove: os.Getenv("AUTO_APPROVE") != "0",
	}

	if value := os.Getenv("STALL_LIMIT"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return cfg, fmt.Errorf("STALL_LIMIT must be a positive integer")
		}
		cfg.stallLimit = n
	}
	if value := os.Getenv("MAX_ITERATIONS"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("MAX_ITERATIONS must be a non-negative integer")
		}
		cfg.maxIterations = n
	}
	if value := os.Getenv("SLEEP_SECONDS"); value != "" {
		seconds, err := strconv.ParseFloat(value, 64)
		if err != nil || seconds < 0 {
			return cfg, fmt.Errorf("SLEEP_SECONDS must be a non-negative number")
		}
		cfg.sleep = time.Duration(seconds * float64(time.Second))
	}

	if len(args) > 0 && (strings.HasSuffix(strings.ToLower(args[0]), ".md") || fileExists(args[0])) {
		cfg.document = args[0]
		cfg.extraArgs = append([]string(nil), args[1:]...)
	} else {
		cfg.document = os.Getenv("DOC")
		cfg.extraArgs = append([]string(nil), args...)
	}
	if cfg.document == "" {
		if fileExists("requirements.md") {
			cfg.document = "requirements.md"
		} else {
			cfg.document = "migration.md"
		}
	}

	absolute, err := filepath.Abs(cfg.document)
	if err != nil {
		return cfg, fmt.Errorf("resolve document: %w", err)
	}
	if !fileExists(absolute) {
		return cfg, fmt.Errorf("document %q not found", cfg.document)
	}
	cfg.document = absolute

	cfg.ompPath = os.Getenv("OMP_BIN")
	if cfg.ompPath == "" {
		cfg.ompPath, err = exec.LookPath("omp")
		if err != nil {
			return cfg, errors.New("'omp' command not found in PATH")
		}
	}
	return cfg, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

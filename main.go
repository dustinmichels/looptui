package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type config struct {
	document      string
	ompPath       string
	model         string
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

If document is omitted, looptui scans the current directory and subdirectories for markdown files with checklists and presents an interactive selection list.

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
	if exitCode := run(os.Args[1:], os.Stdin, os.Stdout); exitCode != 0 {
		os.Exit(exitCode)
	}
}

func run(args []string, input io.Reader, output io.Writer) int {
	if helpRequested(args) {
		fmt.Fprint(output, usage)
		return 0
	}

	cfg, err := loadConfig(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "looptui:", err)
		return 1
	}

	caffeinate, err := startCaffeinate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "looptui: caffeinate:", err)
	} else if caffeinate != nil {
		defer stopCaffeinate(caffeinate)
	}

	var app tea.Model
	if cfg.document == "" {
		checklistDocs, err := findChecklistDocuments(".")
		if err != nil {
			fmt.Fprintln(os.Stderr, "looptui:", err)
			return 1
		}
		if len(checklistDocs) == 0 {
			fmt.Fprintln(os.Stderr, "looptui: no markdown files with checklists found in current directory or subdirectories")
			return 1
		}
		app = newSelectModel(cfg, checklistDocs)
	} else {
		m, err := newModel(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "looptui:", err)
			return 1
		}
		app = m
	}

	program := tea.NewProgram(app, tea.WithAltScreen())
	final, err := program.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "looptui:", err)
		return 1
	}
	if m, ok := final.(model); ok {
		return m.exitCode
	}
	return 0
}

func startCaffeinate() (*exec.Cmd, error) {
	path, err := exec.LookPath("caffeinate")
	if err != nil {
		return nil, nil
	}
	cmd := exec.Command(path, "-d", "-i", "-w", strconv.Itoa(os.Getpid()))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start caffeinate: %w", err)
	}
	return cmd, nil
}

func stopCaffeinate(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
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
	if cfg.document != "" {
		absolute, err := filepath.Abs(cfg.document)
		if err != nil {
			return cfg, fmt.Errorf("resolve document: %w", err)
		}
		if !fileExists(absolute) {
			return cfg, fmt.Errorf("document %q not found", cfg.document)
		}
		cfg.document = absolute
	}

	cfg.ompPath = os.Getenv("OMP_BIN")
	if cfg.ompPath == "" {
		path, err := exec.LookPath("omp")
		if err != nil {
			return cfg, errors.New("'omp' command not found in PATH")
		}
		cfg.ompPath = path
	}

	cfg.model = parseModelFromArgs(cfg.extraArgs)
	if cfg.model == "" {
		cfg.model = detectModelFromEnv()
	}
	if cfg.model == "" {
		cfg.model = detectModelFromConfig()
	}

	return cfg, nil
}

func parseModelFromArgs(args []string) string {
	model := ""
	skipNext := false
	for i, arg := range args {
		if skipNext {
			skipNext = false
			continue
		}
		if (arg == "--model" || arg == "-m") && i+1 < len(args) {
			model = args[i+1]
			skipNext = true
		} else if strings.HasPrefix(arg, "--model=") {
			model = strings.TrimPrefix(arg, "--model=")
		} else if strings.HasPrefix(arg, "-m=") {
			model = strings.TrimPrefix(arg, "-m=")
		}
	}
	return strings.TrimSpace(model)
}

func detectModelFromEnv() string {
	for _, env := range []string{"OMP_MODEL", "PI_MODEL", "MODEL"} {
		if val := strings.TrimSpace(os.Getenv(env)); val != "" {
			return val
		}
	}
	return ""
}

func detectModelFromConfig() string {
	agentDir := os.Getenv("PI_CODING_AGENT_DIR")
	if agentDir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			agentDir = filepath.Join(home, ".omp", "agent")
		}
	}
	if agentDir != "" {
		configPath := filepath.Join(agentDir, "config.yml")
		if model := parseDefaultModelFromFile(configPath); model != "" {
			return model
		}
	}
	home, err := os.UserHomeDir()
	if err == nil {
		configPath := filepath.Join(home, ".config", "omp", "agent", "config.yml")
		if model := parseDefaultModelFromFile(configPath); model != "" {
			return model
		}
	}
	return ""
}

func parseDefaultModelFromFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return parseDefaultModelFromYAML(data)
}

func parseDefaultModelFromYAML(data []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	inModelRoles := false
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(line, "modelRoles:") {
			inModelRoles = true
			continue
		}
		if inModelRoles {
			if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
				inModelRoles = false
				continue
			}
			if strings.HasPrefix(trimmed, "default:") {
				val := strings.TrimSpace(strings.TrimPrefix(trimmed, "default:"))
				val = strings.Trim(val, `"'`)
				if val != "" {
					return val
				}
			}
		} else if strings.HasPrefix(trimmed, "model:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "model:"))
			val = strings.Trim(val, `"'`)
			if val != "" {
				return val
			}
		}
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

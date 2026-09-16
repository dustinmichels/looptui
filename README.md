# looptui

`looptui` is a terminal loop runner for markdown task documents. It reads a markdown file, finds the next `##` section with unchecked `- [ ]` items, runs `omp` on that section, and refreshes progress after each run.

Blocked tasks requiring user input (`- [!]`) do not halt the loop: `looptui` continues executing remaining pending tasks across all sections and saves items needing user input for interactive review.

## Document format

```md
# Any title

## First section

- [ ] pending task
- [x] completed task
- [!] blocked task with question or reason

## Second section

- [ ] another task
```

`looptui` processes the first `##` section with pending tasks (`- [ ]`). `###` and deeper headings stay inside the current `##` section. Tasks marked `- [!]` (blocked / needs input) are skipped by the runner so the loop can continue to other tasks.

## Build & Install

Build an in-place binary:

```sh
go build -o looptui .
```

Or install it to `$HOME/dev/bin/looptui` with the helper script:

```sh
./build.sh
```

Add `$HOME/dev/bin` to `PATH` if you use the helper script and want to run `looptui` by name.

## Run

```sh
looptui path/to/migration.md
```

If you built in-place:

```sh
./looptui path/to/migration.md
```

Extra arguments after the document are forwarded to `omp`:

```sh
looptui path/to/migration.md --model opus
```

The active model and live context usage are displayed in the top header (e.g. `LOOP migration.md · opus · 42k/100k ctx`) in runner mode. The model is detected from `--model` CLI arguments, OMP configuration (`~/.omp/agent/config.yml`), or dynamically updated from `omp` runtime events. Context token metrics (`input + cacheRead`) update live with each turn.

### Context evaluation & agent rotation

To prevent agents from getting bogged down in large contexts when executing multiple tasks in a section:
- The prompt directs the agent to evaluate remaining context after each task completion. If context has grown large, it checks off the task (`- [x]`) and stops cleanly.
- `looptui` automatically detects the task progress, starts a new run with a fresh agent targeting the remaining tasks, and resets the context window.
- As a safety guard, `looptui` also monitors active token usage: if context reaches `CONTEXT_LIMIT` after at least one task is completed, it rotates to a fresh agent automatically.
If no document is passed, `looptui` uses `$DOC` if set; otherwise it automatically scans the current directory and subdirectories for markdown files with checklists and presents an interactive selection list noting the relative path and completion status for each file, sorting incomplete files up top and complete ones (100%) at the bottom.

`looptui` automatically runs macOS `caffeinate` (`caffeinate -d -i -w <looptui-pid>`) in the background along with the TUI without prompting, keeping your computer awake while the loop is active. It is stopped and reaped when `looptui` exits, including via `q` or `ctrl+c`.
Show standalone usage without requiring a document or `omp`:

```sh
looptui --help
```

## Runtime controls
### Document selection mode

When no document is specified, `looptui` lists all discovered markdown files that contain checklists:

- `j`/`k` or `down`/`up`: navigate between documents.
- `1`–`9`: jump directly to a document number.
- `enter`: select the document and start running.
- `e`: open the selected document in `$EDITOR`.
- `q` or `ctrl+c`: quit.


### Runner mode

- `space`: pause or resume automatic runs.
- `r`: retry or start the next run when paused/stopped.
- `i` or `tab`: open the **Review / Needs Input** panel to inspect and resolve blocked tasks.
- `e`: open document in `$EDITOR` (or `vim`/`nano`/`code`).
- `j`/`k`, `down`/`up`, `pgdown`/`pgup`, `g`, `G`/`end`: scroll output.
- `q` or `ctrl+c`: quit; an active `omp` run is cancelled.

### Review / Needs Input mode (`i`)

When one or more tasks are marked `- [!]`, press `i` to enter review mode:

- `j`/`k` or `down`/`up`: navigate between blocked tasks.
- `a` or `enter`: type an answer / instructions in an inline prompt; appends your response to the markdown file and re-queues the task as `- [ ]`.
- `x`: mark the selected task as completed `- [x]`.
- `u`: unblock the task as `- [ ]` without notes.
- `e`: open document in `$EDITOR` jump-positioned at the selected task line.
- `esc` or `i`: return to runner mode.

## Environment

- `AUTO_APPROVE=0`: omit `--auto-approve` when launching `omp`; by default it is included.
- `OMP_BIN=/path/to/omp`: use a specific `omp` binary.
- `CONTEXT_LIMIT=N`: max context tokens before rotating to a fresh agent (e.g. `80k`, `100000`; `0` disables; default `100000`).
- `MAX_ITERATIONS=N`: stop after `N` `omp` runs; `0` means unlimited.
- `STALL_LIMIT=N`: stop after `N` consecutive runs with no checkbox progress; default `3`.
- `SLEEP_SECONDS=N`: delay between automatic runs; default `2`.
- `DOC=path/to/file.md`: default document when no document argument is supplied.
## Exit codes

- `0`: all tasks complete or user exited normally.
- `1`: setup, document, or `omp` startup error.
- `2`: `MAX_ITERATIONS` reached.
- `3`: `STALL_LIMIT` reached.
- `130`: interrupted by quit or signal while an `omp` run is active.

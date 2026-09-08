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

If no document is passed, `looptui` uses `$DOC`, then `requirements.md` if present, otherwise `migration.md`.

Show standalone usage without requiring a document or `omp`:

```sh
looptui --help
```

## Runtime controls

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

# osenv.io: the jev-hybrid as one binary

Point your Claude at `SKILL.md`. It becomes the **desk**, and steers **hybrids** through one route: `POST /v1 {"do": ...}`.

A hybrid is ONE agent made of four models, for ONE task:
- **muse** does the work (cheap) and keeps the momentum.
- **deepseek** reviews and fixes code, **qwen** judges anything visual, and the **Opus corrector** fixes what muse is stuck on (and every 6th run). Each of those is one run, then straight back to muse.
- **Jev** (TypeSafe) scores every action before it runs, routes each step to the right model, keeps reviewer feedback in its lane, and runs the learned loop.
- When the desk passes the task, the hybrid **retires**: its record is kept and its working folder deleted. Only the lessons carry on.

## The learned loop (Jev reasons, it doesn't keyword-match)
- **Intake.** A correction (from the Opus corrector's `learn.add`, or a reviewer finding muse could repeat) is judged by Jev in one call:
  - *lesson or route*: a lesson first. It becomes a route (that kind of step goes to deepseek, qwen or opus) only when muse can't follow it as a written rule.
  - *general or noise*: a way of working, including a rule about the tools or OS, for every future hybrid; or a fact about this task's own files and values, which dies with it.
  - *repeat*: the same mistake as an existing lesson? Then that lesson is sharpened, and escalates to a kick after 2 escapes, instead of a copy piling up.
- **On every action.** Jev checks the action against every lesson the hybrid can see (up to 128; past that, the ones closest to the action first). A reviewer's actions are checked against the owner rules only. A lesson hit of 0.8 or more on a write of project code sends it back once, with the known mistake leading the reason.
- **At run start.** Jev reasons which lessons matter for muse's next steps and puts those in its brief as reminders.
- **Retiring noise.** Lessons Jev keeps calling relevant but that never catch anything retire.
- **Only what was asked.** On every write, Jev sees what's being written, not just the file name, and sends back anything the job didn't ask for: a new feature, visual element or effect, check, confirmation or approval step. muse asks the desk instead (`ASK DESK: may I add ...?`). Measured on the real Jev: a fake shadow, a confirmation dialog and an unrequested chart scored 0.79-0.96; asked-for changes, a handler, a helper and a test scored 0.10-0.48.
- **Owner rules.** The owner's hard "nevers" (no fake shadows, no gates between the owner and their tools, no unasked features) go in through the desk as `learn.add source=owner`. Each one is a hard block from day one, checked on every action, shown to muse at every run start, and never retired as noise. Only the desk can make one.
- **Simulated against the real Jev** (`osenv sim learn -n 300`, 28 labelled cases). These are simulation numbers, not a field result:
  - 94.6% of mistakes caught before they happened, 2% false alarms. The first design reached only 69%: it checked only the lessons judged relevant from the orders, and the orders can't foresee the action. The data changed the design.
  - Intake (0.2, 15 labelled corrections x 3 passes): 91% right on general vs noise, 89% on lesson vs route, 80% of repeats merged. The 3 real corrections 0.1 misfiled on Windows now come out right 9 times out of 9.
- **In the field.** On the first Windows run (0.1), the hooks never fired: all three engines run hooks through cmd.exe, and osenv's quoted path broke there. Jev scored nothing all night, and nobody was told. 0.2 calls the binary unquoted on Windows (checked live on muse, qwen and claude). It also adds a tripwire: a run of a minute or more that sends osenv no hook call blocks its task and says why. On 0.2, Windows task 7 passed with Jev scoring every action (77 hook calls). No lesson fired, because muse didn't repeat an old mistake. 0.3.0's video-editor bench proved one in the field: on Windows, a lesson from the imported Windows pack caught a bare PowerShell `>` that would have written UTF-16 (Jev 0.90), and muse's next action wrote UTF-8.

## Parse, don't dump
Every model call re-sends everything the agent has read, so osenv parses before anything is read:
- `osenv view <file>`: a small file as is; a big log as its errors plus the tail; big code as an outline; JSON as its shape.
- `osenv view <file> --q "question"`: Jev reads the file in chunks and hands back only the parts that answer the question.
- `osenv view <image> [--crop x,y,w,h]`: a 768 px JPEG.

The hook sends a raw read of a big file or a full-size image back once, with the exact view line to use instead.

## The tool library
When a hybrid solves a step with a script another task could reuse (a screenshot, a proof capture, a data check), it saves it to `exec/` with a `usage:` line near the top. The folder is the registry (`osenv do tool.list`). At every muse run, Jev lists the tools that fit the job in muse's brief. When muse starts doing a listed tool's step by hand, the action goes through with a note: there's a tool for that. Measured on the real Jev: fitting tools scored 0.53-0.74 and others 0.34 or less; an action doing a listed tool's step scored 0.45-0.78 and others 0.16 or less.

## The loop, proven
- `osenv sim loop -n 20000`: the engine picker with a stubbed Jev, no tokens. It checks that:
  - every remedy hands back to muse
  - there are never two Opus runs in a row
  - a desk FAIL goes straight to Opus
  - the Opus review comes at least every 6 muse runs
  - served asks are cleared
  - a second remedy crash is marked BLOCKED

  Result: 160,000 picks, 0 violations. This caught one real bug (routed asks were delaying the 6th-run review) before shipping.
- An end-to-end run with the real server and Jev, and scripted engines: muse, then a code review routed to deepseek, then lane-checked feedback, then muse parks, then a desk FAIL goes to Opus, then muse, then PASS retires it.

## What it's good at, and what's untried
- **Proven:** small-to-medium Python projects built from scratch (command-line tools, a stdlib web API over SQLite, a data dashboard, a terminal game, a refactor, a hunt for planted bugs): every finished task passed across the Windows and Linux ladders. Three hybrids at once on one server, both from scratch and on an existing Go codebase with planted bugs, where each hybrid changed only its own files, even with two bugs failing the same test. It works best when the job has a gate the desk can check: tests, output files, screenshots.
- **Weak spots seen:** platform traps muse doesn't know yet (until a lesson forms), visual work (slow, and proof screenshots go wrong), interactive programs (hard to verify beyond tests), and subtle edge cases that slip past muse and the reviewer. The desk's own checks are the safety net.
- **Not tried yet:** big existing codebases (thousands of files), JavaScript or Rust, multi-day features (the desk cuts them into tasks, since only lessons carry over), work without a checkable gate (research, writing), and single runs over 45 minutes. Hybrids share the project's files, so give parallel hybrids jobs that don't touch the same files; osenv doesn't lock them.

## Install
1. Put `osenv` (Linux) or `osenv.exe` (Windows) somewhere on PATH, or next to your project.
2. The engines: `muse` (logged in), `qwen` (Qwen Code CLI; it runs deepseek and qwen over one OpenAI-compatible endpoint), and `claude` (Claude Code, for the Opus corrector). Plus the project's own toolchain: for a Python project, Python and pytest (on Windows from python.org; the Microsoft Store stub doesn't count). `osenv do status` checks python, git, node and go. After installing anything, restart `osenv serve`: it keeps the PATH it started with.
   - On Windows, keep `osenv.exe` in a folder without spaces if 8.3 short names are off on that drive (`status` says so under `hooks`).
   - **Privacy:** the default muse model, `muse-spark-1.3-contributor`, is Meta's cheap contributor tier ($0.10 in, $0.20 out per million tokens), and Meta may use its prompts and outputs, meaning your code, to train future models. To opt out, set `muse.model` to `muse-spark-1.3` in `.osenv/config.json`; its standard tier costs $1.25 in and $4.25 out per million tokens (Meta's pricing page, 2026-09-23).
3. Keys go in `.osenv/keys/` (relative key paths resolve inside `.osenv/`), or use env vars:
   - Jev: `.osenv/keys/jev.key` or `OSENV_JEV_KEY`
   - the model endpoint: `.osenv/keys/plan.key` or `OSENV_PLAN_KEY`
4. In the project root: `osenv serve`. The first run writes `.osenv/config.json` (engines, models, cadence, port 8811, parallel 3) and `.osenv/PROJECT.md`.
5. `osenv do status`, then point your Claude at `SKILL.md`.

## Files
- `api.go`: the one route, every verb, batch and help
- `task.go`: one hybrid per task, verdicts, retirement
- `run.go`: the supervisor, the engines and the lane filter
- `pick.go`: which model runs next
- `hook.go`: Jev on every action, kickbacks, lessons caught in the act, the desk-only guard
- `learn.go`: the learned loop
- `view.go`: parse, don't dump
- `jev.go`: the TypeSafe client
- `board.go`: the event log and `wait`
- `store.go`: `.osenv/`, the config and the NOTES markers
- `sim.go`: both simulations
- `prompts/`: muse's brief, the routed step, the corrector, the model profiles

Go standard library only. `go build -o osenv .`, `go test ./...`.

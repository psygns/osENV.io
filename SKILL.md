---
name: osenv
description: Run coding and build tasks on the jev-hybrid through the osenv binary. You are the DESK - you write each task, watch it, judge the result and pass or fail it; the hybrid (muse doing the work, deepseek/qwen/Opus stepping in for one run each, Jev checking every action) does the work cheaply. Use when the user wants work done "on the hybrid", "with osenv", or delegated cheaply with quality gates.
---

# osenv: you are the desk

One task = one hybrid. muse (a cheap model) does the work and keeps the momentum. When it asks
for a review, the right model steps in for ONE run: qwen for anything visual, deepseek for code,
the Opus corrector when muse is stuck, when you FAIL its work, and on every 6th run. Jev
(TypeSafe) checks every action before it runs and remembers every correction. When you PASS a
task, its hybrid retires and nothing but the lessons carries into the next one.

Your job: write good tasks, wait, LOOK at the result yourself, and pass or fail it. That's all.

## Start (once per project)
The osenv binary sits next to this file (`osenv` on Linux, `osenv.exe` on Windows). In the project root:
- `osenv serve` in the background. The first run creates `.osenv/config.json` and `.osenv/PROJECT.md`.
- Fill in `.osenv/PROJECT.md`: one paragraph on what the project is and the owner's standing calls. Every hybrid reads it.
- Hybrids build only what the job asks. Jev sends back anything unasked (a feature, a visual element or effect, a check, a confirmation or approval step), and muse posts `ASK DESK: may I add <what>? <why>` instead. Answer with `osenv do task.say name=<name> text="yes, add <what>"` or leave it. So write jobs that say what you want, and read those posts.
- If the owner has a standing "never" that the job text won't carry, make it an **owner rule**: a hard block from day one, checked on every action, shown to muse at every run start, never retired, and only the desk can make one. For example:
  `osenv do learn.add source=owner text="<the rule, one sentence>" detect="<the breaking action as it is about to happen>"`
  An owner rule blocks hard, so word its detect narrowly. For a folder rule, say that `$TMPDIR` and every path under the project are inside it, and don't give `/tmp` as an example: a rule worded that way blocked a hybrid writing into its own temp folder.
- `osenv do status`: every engine should be found and both keys true. If Jev isn't `ok`, stop and say so.

## The loop
Every call is `osenv do <verb> key=value ...` (or JSON: `osenv do '{"do":"help"}'`). `osenv do help` lists every verb. A long value, or one with double quotes inside, goes in a file: `text=@say.txt` (PowerShell 5.1 breaks a value with double quotes into pieces).

1. **Post a task.** Write the job to a file first: the goal, the gates (the proof it must show), where its output goes, and what it must not touch. Then:
   `osenv do task.new name=<short-name> job=@job.md`
   Hybrids run at once and share the machine. Give each its own files, and run only one that drives a desktop app at a time: they share one mouse, keyboard and screen, and steal each other's clicks and screenshots.
2. **Wait. Don't poll.** Run this in the background and act when it returns:
   `osenv do wait since=<head> timeout=900`
   It returns when a hybrid posts, parks for you ("waiting"), gets blocked, or learns something. Keep `head` from the last result. To wake only when you're needed, add `kinds=[waiting,blocked]`.
3. **Look before you judge.** `osenv do task.get name=<name>` shows its STATE, open asks and output files. Open the real outputs:
   - `osenv view <file>` for a parsed view (a big log gives its errors and tail, big code an outline)
   - `osenv view <file> --q "<what you're checking>"` to get only the parts that answer your question
   - `osenv view <image>` for a small JPEG, `--crop x,y,w,h` for detail. Check faces and models from the front too, not only 3/4.
   - `osenv proof check .osenv/tasks/<name>/out` checks every proof file at once: a broken one (UTF-16, NUL bytes, not UTF-8), a blank screenshot, a transcript whose exit code isn't 0, or a failed server check is named as such.
   - `osenv proof mutate <out.txt> <source file> -- <test command>` proves the tests catch bugs: it plants small bugs one at a time in a copy of the project, and names every one the tests missed. Put it in a job's gates when the tests are the point.
   Never pass on the hybrid's word, or a reviewer's, alone.
4. **Judge.**
   - PASS: `osenv do task.verdict name=<name> pass=true text="<why>"`. The hybrid retires; its record goes to `.osenv/done/`. Pass only once `task.get` shows `waiting: true`; if the seat is still shutting down, the verdict waits for it.
   - FAIL: `osenv do task.verdict name=<name> pass=false text="<exactly what is wrong, where>"`. The Opus corrector fixes and teaches, then muse carries on.
   - New orders without failing it: `osenv do task.say name=<name> text="<orders>"`.
5. Tell the user in a line or two what passed, what failed and why.

## The tool library (exec/)
A script in the project's `exec/` folder with a `usage: <command line> - <what it does>` line near its top is a tool. The folder is the registry, and `osenv do tool.list` shows it. Hybrids save reusable scripts there on their own; that's always allowed. At each muse run, Jev lists the tools that fit the job in muse's brief. When muse does a listed tool's step by hand, it gets a note: there's a tool for that. Keep exec/ lean: delete a tool that's wrong or unused.

## Take-buckets: the memories you control
A take is one short record of what an agent did: an action and what came of it. Never "someone said", an opinion, a wish or an order. Its tag string is `<project ID>,<bucket>,<bucket>...`: the first term is always the project ID, which owns it, and every other term is a bucket (a topic like 3d or script, or a name). A bucket appears the first time a take uses its tag.
- **Recorded automatically.** Every verdict and cancel records one take (the task, the verdict and the first sentence of your why) under the task's tags. Put what the hybrid did in that first sentence.
- **You control them.** `osenv do take.add text="..." tags=<project ID>,<bucket>` adds one. `osenv do take.add id=T12 tags=<its project ID>,+script,-texture` moves one between buckets. `osenv do take.remove id=T12` deletes it. `osenv do take.list` shows every take in full. Hybrids can't add, move, remove or read takes: each gets only the ones picked for it.
- **You decide what each hybrid remembers.** Spool a hybrid with a tag string: `osenv do task.new name=<name> job=@job.md tags=<project ID>,<bucket>,<bucket>` (and `takes=<x>`, default 5). Jev picks up to x takes from that project, in those buckets. It never picks a take that reads as someone's words, an opinion or a wish, or one that could lead the hybrid against its job or the project, even if that leaves none. Adjectives lower a take's rank heavily. The picked takes go into the hybrid's brief, and `task.new` and `task.get` show them in full. Without `tags`, a hybrid starts with none.
- **Every recalled take costs the hybrid context.** Recall only what the task needs, and prune.

The API talks to you about them inside the responses you already get: while any take exists, every response carries a `take_buckets` line. It lists every bucket in full with its count. This is the owner's wording, word for word, with x, y, z being the bucket list:

> Here are the past history take buckets (project ID + tags): x, y, z. These are optional and will persist your desired memories through the hybrid. You may append or delete them at any time with take.add or take.remove, if you wish.

It ends with a prune reminder:

> Keep your buckets pruned: every take you recall costs the hybrid context.

## Routes that ship with osenv
A fresh project's `status` shows `routes: 2`. Those are the two core routes: **R-visual** sends a visual review (how something looks) to qwen, and **R-code** sends a code review to deepseek. They are the hybrid's design, not something it learned, and they are never swept.

## Rules the owner already paid for
- Look at every output yourself before a PASS. A reviewer's PASS is not yours.
- One task per hybrid. New work is a new task, never more orders onto a finished one.
- Never ask for a fake version of something real (fake shadows, placeholder art presented as final, loosened tests).
- Say what you saw, not what you hope. If it's weak, FAIL it with the exact fault.
- Money, size, going live and taste belong to the user. Everything else, you decide and tell.

## When something is off
- `blocked` event: `osenv do task.get name=<name>`, then read `.osenv/tasks/<name>/run.log` with `osenv view`, fix the cause, then `osenv do task.resume name=<name>`.
- `blocked` with "Jev saw none of its actions": the engine's hooks aren't reaching osenv, so nothing was being checked. Run `osenv do status` and read the `hooks` line. Don't resume until that's fixed.
- A `gate` event means osenv held muse's STEP DONE, because the job requires a review (visual or code) that hasn't happened yet. The review runs first, then muse parks again. Nothing to do.
- `osenv do task.cancel name=<name> text="<why>"` abandons a task without a verdict. It keeps the record, marked cancelled.
- A run did damage: `osenv do task.pause name=<name>`, then `osenv do task.undo name=<name> dry=true` shows what would change; `osenv do task.undo name=<name>` puts back what its latest run changed, byte for byte (`run=<n>` for an earlier one). A file changed again since, or touched by another hybrid during that run, is left alone and named; `force=true` undoes the second kind too. Then fix the job or FAIL it, and resume.
- To check that a lesson catches a mistake, hand-feed one action while its task is live: pipe `{"tool_name":"bash","tool_input":{"command":"<the bad action>"}}` into `osenv hook pre --task <name> --role muse --test`. You get Jev's answer back, and the lesson's record and the hybrid's history stay untouched.
- `osenv do learn.list` shows what the loop has learned, and `osenv do learn.graph` shows it as a graph. `osenv do learn.retire id=<id>` retires a wrong lesson. If Jev filed something wrong (a route that should be a lesson, a general rule scoped to one task), fix it and keep its record: `osenv do learn.edit id=<id> kind=lesson scope=global`. If a lesson became a hard block (kick) by mistake, set it back and correct its miss count: `osenv do learn.edit id=<id> severity=nudge escapes=0`. A false catch: `catches=N`. Changing `detect` replays the lesson's past catches and lists any the new wording misses (`misses_now`). A `learn` event saying CONTRADICTS means two lessons disagree: retire or edit one.
- Lesson packs carry proven lessons between projects. `osenv do learn.export file=<pack.json>` writes the global lessons that caught a real mistake; `osenv do learn.import file=<pack.json>` loads a pack. The kit's `packs/` folder has two: `windows-proven.json`, the two lessons proven on the Windows ladder (saving output on PowerShell, checking every review fix), and `all-lessons-0.3.json`, all 85 lessons the 0.2 and 0.3 runs learned on both machines, most not yet proven in the field.
- `osenv do task.pause name=<name>` stops a hybrid mid-run.

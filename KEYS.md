# Your keys

osenv ships with no keys. It needs three accounts of your own:

1. **Jev (TypeSafe System One)** judges every action and runs the learned loop.
   Put your key in `~/.config/osenv/jev.key`, or set `OSENV_JEV_KEY`.
2. **One OpenAI-compatible endpoint** serving a deepseek model (code reviews) and a qwen model (visual reviews). Both run through the qwen CLI.
   Put your key in `~/.config/osenv/plan.key`, or set `OSENV_PLAN_KEY`.
   The default endpoint and model names are in `.osenv/config.json` under `plan` (url, deepseek, qwen). Change them to match your provider.
3. **muse** (Meta's Muse Code) is logged in on your machine (`muse` asks on first run). osenv links that login into each hybrid.
   Privacy: the default model, `muse-spark-1.3-contributor`, is Meta's cheap tier, and Meta may train on its prompts and outputs. To opt out, set `muse.model` to `muse-spark-1.3` (the standard tier costs more).

Plus **Claude Code**, logged in, for the Opus corrector and for you as the desk.

Per-project keys work too: a relative `key_file` in `.osenv/config.json` resolves inside `.osenv/`, for example `keys/jev.key`.

Check it all with `osenv do status`: every engine found, both keys `true`, and `jev: ok`.

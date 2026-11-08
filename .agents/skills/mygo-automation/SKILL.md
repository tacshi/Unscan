---
name: mygo-automation
description: Use when reading a MyGo app's native UI elements and acting on them, sending pointer, wheel, key and text input, choosing items of the application menu, evaluating scripts in pages, or taking screenshots of a running MyGo app.
---

This file is a discovery stub, not the usage guide. Before running a mygo-automation command, load the actual workflow from the CLI:

```bash
mygo-automation skills # start here
```

Do not probe first with --help or --version. Only if the command is missing, install it from the app's module directory with `go install github.com/egoist/mygo/plugins/automation/cmd/mygo-automation`.

Unscan listens for it only when built with `-tags e2e` and run with `MYGO_AUTOMATION` naming a socket, as `scripts/build-mac-debug.sh` builds it and the README's "Inspecting and driving the app" runs it.

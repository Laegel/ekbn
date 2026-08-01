# ekbn - Embedded Kanban

![Screenshot](screenshot.png)

A lightweight local tool for project management.
You can edit the cards with your favorite text editor or open the web application.
It comes with an agentic mode for planning and implementing.

## Architecture

Each folder represents a column.
1 task = 1 Markdown file in a column.
1 server app reading and writing files, written in Go.
1 front-end app, written in TS with DaisyUI.

It's all bundled in the binary so you don't have anything to install or build.
You can find the binaries (Linux, Windows, macOS) in the Actions tab, in the merge PR workflows.

Requirement: 
- File watcher: check https://github.com/fsnotify/fsnotify for platform support.

## Run

The binary comes with a `serve` command. Careful, if no port is specified (as an environment variable or a config) then a random port is picked: it's convenient but when running the app from a GUI, the process will remain active unless you explicitely kill it.

## Caveats

If running ekbn inside a Docker Container, the agent needs to access your dev tools. I need to add a way to properly sandbox but provide the agent with some capabilities (a proxy? a proxy MCP?).

## Server & UI Capabilities

- Create card
- Update card
- Move card
- Delete card
- Create column
- Move column

There's a file watcher that notifies the front-end whenever a card file is created, modified, deleted or moved. The event is pushed via WebSocket in order to sync the state.

## MCP Support

Your favorite agent is able to interact with this app via MCP. To run the app with MCP support, use the `mcp` command.

## Customization

You can create an `ekbn.config.yml` (or `ekbn.config.yaml`) file next to the binary and set the following properties:

```yml
theme: dark
folder-name: columns
port: 8080
verify: task verify
```

The theme can be configured via a `custom.css` file. ekbn is using DaisyUI under the hood so defining themes is as simple as providing custom variables.
See the `custom.css` file from the repo.

`verify` is the shell command the orchestrator runs, from the project root, after each agent attempt on a ticket. Only a `verify` exit code of 0 lets the ticket advance — the agent process's own exit code is only used for logging, not for deciding completion. If `verify` isn't configured, the orchestrator refuses to advance any card and logs an error explaining why.

`roles` maps a card's `role` field to an agent configuration:

```yml
roles:
  default:
    prompt: "You are a general-purpose implementation agent."
  frontend:
    prompt: "You are a frontend specialist."
    tools:
      - get_card
      - update_card
    skills:
      - react-patterns
```

Each role declares a `prompt` and, informationally, `tools`/`skills` rendered into that prompt. The orchestrator reads a card's `role` and picks the matching entry — this is a config lookup, never a model's judgment call. An unset or unknown role falls back to the `default` entry and logs which card lacked one, rather than failing the run. The `specify` agent assigns `role` (from this same configured set) and `spec` (the spec file it decomposed) to every card it creates.

`security-paths` lists path globs (`**` supported) that trigger an additional, stricter security review after `verify` and the general review both pass:

```yml
security-paths:
  - "**/rng/**"
  - "**/pity/**"
  - "server/trpc/**"
```

Selection is a mechanical glob match against the paths touched in the card's diff — never a model's judgment about whether a change "seems security-relevant." When a match is found, a security reviewer runs in the same kind of isolated session as the general reviewer (no access to the implementer's session, no board tools, no repo write access) and looks specifically for ways a client could influence RNG/drop pools, summon rates and pity, battle-log validation, auth/session handling, or anything reachable via a tRPC procedure. Like the general reviewer, it can only block, never approve — but it also enforces its own bar for what counts: a finding is only accepted if it names a concrete reproduction (the specific request or input sequence that produces the wrong outcome) under a `## Reproduction` heading. Advisory output without one ("consider adding rate limiting") is rejected by the stage itself and does not block the card.

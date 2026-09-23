# Vineyard

Tend your AI coding agents across machines from VS Code. Vineyard shows every Claude Code session on
every machine you own as **Machines › Workspaces › Agents**, with live state (working, thinking,
running a tool, asking you a question, waiting for permission, idle), model and effort, context size,
the last prompt and the conversation title. Open the transcript, jump to a terminal on that machine,
or resume a session from wherever you happen to be sitting.

Only Claude Code is supported today; the model, daemon and tree are provider-agnostic so others can
be added.

## What you can do

* **See every agent** on every machine, grouped Machines › Workspaces › Agents, with live state,
  model, effort, context size, title and last prompt. Each tier's order is yours to choose
  (`vineyard.sort.machines` / `.workspaces` / `.agents`: status, name, recent or attention-first) with
  stable tie-breaks, so rows stay put while agents work. *Vineyard: Settings* opens all of them.
* **See the subagent tree** under each agent: every Agent-tool invocation Claude Code spawned, nested
  to any depth (a subagent's own subagents sit under it), each with its description, agent type,
  model, background/foreground and live state derived from its own transcript. Running subagents are
  always shown; finished ones can be hidden with `vineyard.showFinishedSubagents`. Clicking one opens
  its transcript in a read-only chat view.
* **Open a chat** for any agent: a panel styled like the Claude Code pane that streams the transcript
  as it grows (railway margin with coloured event markers, the current prompt pinned while you
  scroll, thinking collapsed and greyed, IN/OUT command blocks, an activity ticker while the agent is
  busy) with an info strip for model, effort, mode, prompt-cache hit rate, token totals and a map of
  spawned subagents.
* **Message any running agent** from the composer. For sessions you drive elsewhere (the Claude
  extension, a terminal) the text is delivered over Claude Code's cross-session messaging socket and
  read between tool calls or when the agent is idle.
* **Start agents from Vineyard** in any workspace on any machine (*New Agent Here…*), or **resume** an
  existing session under Vineyard's control. These *managed* sessions run as children of that
  machine's daemon over the stream-json control protocol, so permission prompts and questions appear
  as cards in the chat and are answered there. They still write the normal registry and transcript.
  New agents start with no questions asked (the model and effort last chosen in that workspace, or
  Claude Code's defaults the first time; the configured permission mode; no first prompt); the
  composer carries pickers for **model, reasoning effort and permission mode** that change the
  running session in place, like the ones in the Claude Code pane, and are remembered per workspace
  for the next new session there.
* **Resume a past session** (*Resume a Past Session…*, the history button) from the transcripts on
  any machine, newest first with title, first/last prompt, model and branch, whether or not an agent
  is currently attached to it. **Stop / Terminate** ends any live session: managed ones cleanly over
  their control channel, others by terminating the Claude Code process (transcript kept).
* **Rename a session** by clicking its title in the chat (or *Rename Session…* in the tree). Managed
  sessions are renamed through Claude Code's `rename_session` control request; for others Vineyard
  appends the same `custom-title` transcript line `/rename` writes, so Claude Code shows the new name too.
* **Sign a machine in to Claude from wherever you are** (*Sign In to Claude on Machine…*, or the card
  the chat shows when a session reports an expired OAuth session). The daemon there runs
  `claude auth login` without a browser, Vineyard opens the sign-in URL in *your* browser and passes
  the code it shows back over the mesh.
* **Join machines without SSH** with a single-use invite code.
* **Self-updating fleet**: install a newer extension on one machine and it upgrades every daemon
  over the mesh (see *Staying up to date*).

Observed sessions (started outside Vineyard) cannot have their permission prompts or questions
answered from here: Claude Code only accepts those in the originating UI. The chat shows a card
explaining that and offers to open the workspace on that machine.

## Design

```
 ┌──────────────┐        mTLS, fleet cert         ┌──────────────┐
 │ VS Code      │◄──────────────┐                  │ falcon       │
 │  (viewer)    │  localhost    │   subscribe      │  vineyardd   │
 │      │       │──────────────►│◄────────────────►│  (quiet)     │
 │  vineyardd   │               │   snapshots      └──────────────┘
 │  (watching)  │               │                  ┌──────────────┐
 └──────────────┘               └─────────────────►│ magpie …     │
       frogmouth                                    └──────────────┘
```

* **One static Go binary per machine** (`vineyardd`, ~6 MB, no runtime). It runs as a login service
  (launchd / systemd --user / Scheduled Task) and reads Claude Code's own on-disk state:
  `~/.claude/sessions/*.json` (registry: pid, cwd, `busy|shell|idle|waiting`), the tail of the
  session transcript (model, effort, pending tool calls, AskUserQuestion, end of turn), and
  `~/.claude/ide/*.lock` (which folders VS Code has open).
* **No hub.** Every daemon is equal. The daemon on the machine where you open VS Code is your
  orchestrator: it dials the others and subscribes. Any machine can step in at any time.
* **Silent unless watched.** A daemon with no VS Code attached holds no connections, runs no timers
  and does not even read the Claude directory. When a viewer attaches, the local daemon connects to
  peers and subscribes; peers push their snapshot **only on change**, coalesced to one poll per second,
  plus one 30 s ping per connection. When the last viewer leaves (30 s grace for reloads) everything is
  unsubscribed and torn down. There is no gossip and no periodic re-broadcast, so N machines cost at most
  N-1 connections per watching machine and zero bytes when nobody is looking.
* **Sleeping machines are woken.** Daemons report their hardware addresses; when a watched peer stops
  answering, its neighbours send Wake-on-LAN magic packets for those addresses (LAN broadcast plus
  unicast) and open a connection to its SSH port so a Bonjour sleep proxy wakes it, at most once every
  45 s and only while a viewer is attached. *Wake Machine* on an offline machine does it on demand.
  `wakePeers: false` in config.json opts out.
* **A watched Mac stays awake.** A Mac that idle-sleeps only surfaces for ~45 s per Wake on Demand,
  so its link flaps and its agents stall. While anyone is subscribed to a machine, its daemon holds a
  `caffeinate -s` assertion (macOS only, `keepAwakeWhileWatched` in config.json to opt out); the moment
  the last viewer leaves it is released. Nothing is held while nobody is looking.
* **Stateless connections.** A fresh connection carries everything it needs: hello, then subscribe.
  Reconnects use exponential backoff (2 s → 60 s) only while someone is watching. Offline or
  unreachable machines show their last-known snapshot from `~/.vineyard/cache.json`.
* **Security.** All traffic is TLS 1.3 with mutual authentication using one shared fleet certificate
  (ECDSA P-256, self-signed, 100 years) generated on the first machine and copied to the others over
  SSH during setup. Possession of `fleet.key` *is* membership, exactly like a pre-shared key; rotate by
  regenerating and re-running setup. Transcript reads are sandboxed to `~/.claude/projects`.
* **Joining without SSH: invite codes.** Any member can mint a single-use code valid for 15 minutes
  (`vineyardd invite`, or *Vineyard: Create Invite Code*). The code is `vineyard:` + base64url JSON
  carrying the inviter's addresses (advertised name plus its LAN IPs), a 128-bit fingerprint of the
  fleet certificate, and a 128-bit random token. The joiner dials the inviter, pins the fingerprint,
  presents the token over TLS 1.3, and receives the fleet certificate + key and the peer list. The
  inviter accepts certificate-less connections **only while an invite is outstanding**, and such a
  connection may send exactly one `join`. This is how Windows boxes (no SSH server) and machines
  without key-based SSH get in; the extension bundles binaries for every platform so the joiner runs
  its own copy locally. Codes also work as `vscode://peter-dolkens.vineyard/join?code=…` links.
* **SSH is an optional bootstrap.** For machines you *can* SSH to, *Add Machine* copies the binary
  and certificate with `scp` and runs `vineyardd init && vineyardd install` remotely. Day-to-day
  traffic never touches SSH either way.

### Agent state derivation

| Evidence | State |
| --- | --- |
| pid gone | `exited` |
| pending `AskUserQuestion` tool call | `question` |
| pending `ExitPlanMode`, or registry `waiting` | `permission` |
| pending tool call (no result yet) | `tool` |
| last line is a `user` prompt or tool result | `working` |
| last assistant block is `thinking`, turn not ended | `thinking` |
| assistant `stop_reason: end_turn` | `idle` |
| registry `shell` | `shell` |
| subagent whose turn ended, or whose result reached the parent | `done` |

Sessions whose working directory is a Claude Code scratchpad (`…/claude-<uid>/<encoded project>/<session>/scratchpad`)
are shown under the project that spawned them; the encoded name is decoded against the filesystem.

Registry and transcript disagreements are reconciled (e.g. `busy` after `end_turn` = "starting next
turn"; `idle` mid-turn for >30 s = "interrupted"). See `daemon/internal/claude/derive.go` and its tests.

Subagents come from `<projects>/<encoded cwd>/<session>/subagents/agent-<id>.jsonl` plus the
`.meta.json` beside each one (agent type, description, the parent's `toolUseId`, `parentAgentId`
for nested spawns, background or foreground). Each file's tail is derived like a session's, with
sidechain lines counted. A foreground subagent is `done` once the parent has its `tool_result`; a
background one once the parent's tail carries its `<task-notification>`, or its own turn ends. One
mid-turn but silent for 15 minutes is shown as `unknown`. A session contributes at most 60 subagents
to a snapshot (oldest finished dropped first). See `daemon/internal/claude/subagents.go`.

## Repository layout

```
daemon/                 Go module: vineyardd
  cmd/vineyardd         CLI: init | run | install | uninstall | restart | status | probe | peer | invite | join
  internal/claude       collector (reads ~/.claude), state derivation (+ tests), cross-session message sender
  internal/managed      daemon-spawned sessions over stream-json: prompts, permission prompts, questions
  internal/mesh         TLS, framing, demand-driven peer subscriptions, viewer fan-out, request relay, invites
  internal/service      launchd / systemd / Task Scheduler installers (Windows needs no elevation: XML logon task, then HKCU Run key)
src/core                wire types + formatting shared by the extension
src/extension           VS Code extension: daemon client, fleet store, tree, chat panel host, setup over SSH
src/webview             chat panel UI (bundled separately; marked for Markdown)
scripts/build-daemon.sh cross-compiles vineyardd into bin/: macOS arm64/amd64, Linux and Windows arm64/amd64/386
```

## Install

Vineyard is on the Visual Studio Marketplace as
[`peter-dolkens.vineyard`](https://marketplace.visualstudio.com/items?itemName=peter-dolkens.vineyard);
search for "Vineyard" in the Extensions view. VS Code then keeps it up to date on its own, and each
new extension build brings the fleet's daemons along (see *Staying up to date*).

Every tagged release is also on the [Releases page](https://github.com/peter-dolkens/vineyard/releases)
as a `vineyard-<version>.vsix` with the daemon binaries for all platforms bundled inside, plus the
standalone `vineyardd-*` binaries and a `SHA256SUMS.txt`. Install with *Extensions: Install from
VSIX…* or:

```sh
code --install-extension vineyard-<version>.vsix
```

To cut a release: bump `version` in package.json, `git tag v0.3.6 && git push --tags`. The **Release**
workflow cross-compiles the daemon, runs the tests, packages the extension, publishes the GitHub
release and pushes the VSIX to the Marketplace (secret `VSCE_PAT`; skipped for pre-releases). Running the workflow
manually from the Actions tab produces a pre-release named after the commit.

## Staying up to date

Updates flow from the extension outwards, so upgrading one VS Code is enough to upgrade the fleet:

* **Extension.** Installed from the Marketplace, VS Code updates it itself. For VSIX installs the
  extension asks the GitHub Releases API at most once a day whether a newer version exists and offers
  to download and install it (`vineyard.checkForUpdates`, or run *Vineyard: Check for Extension
  Updates*). That single request is the only time Vineyard talks to anything outside your machines.
* **Daemons.** The extension bundles a `vineyardd` build for every platform. When a Vineyard view is
  open and it sees an online machine reporting an older daemon than the bundled one, it streams the
  matching binary to that machine over the existing mesh connection (the `upgrade` request, relayed by
  the local daemon in 512 KB chunks). The receiving daemon stages the file, checks the SHA-256, runs
  `vineyardd version` on it to prove it executes on that platform, then hands over to it: the new
  binary copies itself into `~/.vineyard/bin`, re-registers the login service and restarts. No SSH,
  no polling, and nothing at all happens while every daemon is current. Turn it off with
  `vineyard.autoUpdateDaemons`; *Update Daemon* on a machine or *Update All Daemons* does the same
  by hand. Only the numeric part of a version is compared, so two different dev builds never
  overwrite each other; unparseable versions (`dev`) are never touched. Daemons older than 0.3.0 do
  not understand `upgrade`, so the first update of those goes over SSH (or the local installer) once.

## Build and run

```sh
npm install
npm run build:all        # Go cross-compile into bin/ + esbuild bundle into dist/
npm test                 # node --test + go test
```

Press **F5** ("Run Vineyard") to launch an Extension Development Host. In the Vineyard view:

1. **Set Up This Machine** – writes `~/.vineyard/config.json`, generates the fleet certificate and
   installs the login service. The view connects to the local daemon within a second.
2. **Create Invite Code** on this machine, then on another machine (with the extension installed)
   run **Join Fleet with Invite Code** and paste it. The joiner fetches the certificate and peers from
   the inviter, installs its own service and shows up in both views. This needs only TCP reachability
   on the daemon port, no SSH.
3. **Add Machine** (alternative) – enter an SSH host. The extension detects the platform, copies the matching
   binary and the certificate, runs `init` with the current peer list, installs the service, and
   registers the new peer locally. Repeat for each machine, from any machine.
4. Every daemon learns other peers' addresses from their hellos and persists them, so a machine that
   was set up from `frogmouth` can itself be the orchestrator later.

Useful on any machine:

```sh
~/.vineyard/bin/vineyardd status        # fleet table from the local daemon
~/.vineyard/bin/vineyardd probe | jq    # this machine's snapshot without a daemon
tail -f ~/.vineyard/vineyardd.log
```

## Protocol (newline-delimited JSON over mTLS)

| Message | Direction | Purpose |
| --- | --- | --- |
| `hello {role: peer\|viewer, machineId, listen}` | both | identity + advertised address |
| `subscribe` / `unsubscribe` | peer→peer | "push me your snapshot on change" |
| `snapshot {snapshot}` | peer→subscriber | full self-report (idempotent, newest `at` wins) |
| `ping` / `pong` | outbound side pings | liveness, 30 s |
| `fleet`, `update`, `peerstatus` | daemon→viewer | aggregated view for VS Code |
| `req {id, target, op, args}` / `res` | viewer→daemon→peer | `transcript` (tail or from a byte offset), `send`, `spawn`, `respond`, `interrupt`, `stop`, `configure` (model / effort / permission mode of a managed session, via Claude Code's `set_model`, `apply_flag_settings`, `set_permission_mode` control requests), `login` (relay `claude auth login`: start → URL, code → result), `rename` (custom session title), `wake` (Wake-on-LAN + sleep-proxy nudge for a peer), `sessions` (past transcripts for a workspace or machine), `kill` (terminate an observed session's process), `probe`, `addpeer`, `removepeer`, `invite`, `upgrade` (chunked daemon binary, see *Staying up to date*), `version` |
| `join {token, machineId, listen}` / `joined {cert, key, peers}` | joiner→inviter (no client cert) | one-shot enrolment while an invite is active |

## Managed vs observed sessions

| | Observed (Claude extension, terminal) | Managed (started or resumed from Vineyard) |
| --- | --- | --- |
| How state is read | transcript tail + registry, polled 1/s while watched (stat-cached) | same, plus control events from the process |
| Send a prompt | cross-session messaging socket | stdin (stream-json) |
| Permission prompts | shown; answer in the originating UI | Allow / Allow-and-remember / Deny cards |
| AskUserQuestion | shown; answer in the originating UI | option buttons + free text |
| Interrupt / stop | no | yes |
| Lifetime | independent | child of the daemon; ends if the daemon restarts |

## Known gaps

* Windows daemon and installer are compiled and reasoned about but not yet tested on a real box; the
  Claude project-directory encoding on Windows is a best guess.
* Installing on a Mac over SSH needs the target user to have a GUI login for `launchctl bootstrap`;
  the installer falls back to `launchctl load -w`.
* No multi-hop relay: a machine you cannot reach directly shows last-known state only.
* Only Claude Code is detected.

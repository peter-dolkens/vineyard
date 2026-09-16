# Vineyard

Tend your AI coding agents across machines from VS Code. Vineyard shows every Claude Code session on
every machine you own as **Machines › Workspaces › Agents**, with live state (working, thinking,
running a tool, asking you a question, waiting for permission, idle), model and effort, context size,
the last prompt and the conversation title. Open the transcript, jump to a terminal on that machine,
or resume a session from wherever you happen to be sitting.

Only Claude Code is supported today; the model, daemon and tree are provider-agnostic so others can
be added.

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
  its own copy locally. Codes also work as `vscode://dolkens.vineyard/join?code=…` links.
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

Registry and transcript disagreements are reconciled (e.g. `busy` after `end_turn` = "starting next
turn"; `idle` mid-turn for >30 s = "interrupted"). See `daemon/internal/claude/derive.go` and its tests.

## Repository layout

```
daemon/                 Go module: vineyardd
  cmd/vineyardd         CLI: init | run | install | uninstall | restart | status | probe | peer | invite | join
  internal/claude       collector (reads ~/.claude) + state derivation (+ tests)
  internal/mesh         TLS, framing, demand-driven peer subscriptions, viewer fan-out, request relay, invites
  internal/service      launchd / systemd / schtasks installers
src/core                wire types + formatting shared by the extension
src/extension           VS Code extension: daemon client, fleet store, tree, transcript, setup over SSH
scripts/build-daemon.sh cross-compiles vineyardd into bin/: macOS arm64/amd64, Linux and Windows arm64/amd64/386
```

## Install from GitHub

Every tagged release on the [Releases page](https://github.com/peter-dolkens/vineyard/releases)
carries a `vineyard-<version>.vsix` with the daemon binaries for all platforms bundled inside, plus
the standalone `vineyardd-*` binaries and a `SHA256SUMS.txt`. Install with *Extensions: Install from
VSIX…* or:

```sh
code --install-extension vineyard-<version>.vsix
```

To cut a release: `git tag v0.2.0 && git push --tags`. The **Release** workflow cross-compiles the
daemon, runs the tests, packages the extension and publishes the release. Running the workflow
manually from the Actions tab produces a pre-release named after the commit.

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
| `req {id, target, op, args}` / `res` | viewer→daemon→peer | `transcript`, `probe`, `addpeer`, `removepeer`, `invite` |
| `join {token, machineId, listen}` / `joined {cert, key, peers}` | joiner→inviter (no client cert) | one-shot enrolment while an invite is active |

## Known gaps

* Windows daemon and installer are compiled and reasoned about but not yet tested on a real box; the
  Claude project-directory encoding on Windows is a best guess.
* Installing on a Mac over SSH needs the target user to have a GUI login for `launchctl bootstrap`;
  the installer falls back to `launchctl load -w`.
* No multi-hop relay: a machine you cannot reach directly shows last-known state only.
* Only Claude Code is detected.

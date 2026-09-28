# Vineyard internals

How Vineyard works under the hood: the daemon, the mesh, how agent state is derived, the wire
protocol, and how to build and release. The [README](../README.md) covers what it does and how to
use it.

## Design

```
 ┌──────────────┐     mTLS, machine certs         ┌──────────────┐
 │ VS Code      │◄──────────────┐                  │ build-box    │
 │  (viewer)    │  localhost    │   subscribe      │  vineyardd   │
 │      │       │──────────────►│◄────────────────►│  (quiet)     │
 │  vineyardd   │               │   snapshots      └──────────────┘
 │  (watching)  │               │                  ┌──────────────┐
 └──────────────┘               └─────────────────►│ homelab …    │
       studio                                       └──────────────┘
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
* **Membership spreads through hellos.** The hello that opens every connection carries the sender's
  addresses and every other member it knows, with each one's candidate addresses: up to 10 per machine,
  most recently used first (a successful dial or the machine reporting or presenting an address counts
  as use; addresses heard second-hand queue behind, so they are evicted first). The receiver adds machines it has never heard of, and dials them while watched. Sent once per
  connection, never forwarded, so a machine added over SSH or joined from the CLI reaches every view
  without ever being watched itself.
* **Removal is fleet-wide.** *Remove Machine* records `{machineId, at}` in `removed`, sends a
  `removed` message to every connected peer, and every hello carries the whole record, so it reaches
  all members. Members drop the machine and refuse its hellos. Deliberate adds (the `addpeer` op
  behind *Add Machine*, an invite, `peer add`, `init --peer`) stamp `added`; the later of a removal
  and an add wins everywhere. A joiner learns its own `added` from the invite and states it in its
  hello, so members still holding an older removal let it back in. Candidates
  are dialed happy-eyeballs style, 250 ms apart, first fleet-authenticated connection wins.
* **One-hop relay.** When no candidate answers, the watcher asks each member it holds a direct link to
  (the last relay that worked first) to splice it through: a fresh connection whose first line is
  `tunnel {target}`. The relay dials the target and opens with `tunnel-in`, or, if it cannot, sends
  `tunnel-callback` over a link the target already holds to it and waits for the target to connect
  back with `tunnel-accept`. After `tunnel-ok` the two ends run a second TLS handshake inside the
  splice, so the relay copies ciphertext only, then the ordinary hello and serve loop. Links carry a
  `via` (`direct` or `relay:<id>`), a direct link always replaces a relayed one, relays only use direct
  links so tunnels never chain, and nothing is relayed unless someone watches the target.
* **Healing is event-driven.** A relayed peer's direct addresses are tried again when it (or anyone's
  hello) reports an address we did not have, and when another viewer attaches; a direct link that
  answers replaces the relayed one on both sides. There is no retry timer.
* **Uplinks for machines nobody can connect to.** With `uplink` `auto` (default) a daemon asks a
  member to dial it back (`dialback`; the test connection opens with `probe` and echoes a nonce, so an
  answer from another machine on the same private address does not count) at startup, when its own
  addresses change (a local interface check once a minute, no traffic) and when its uplink drops. If
  nobody can, the link to that member becomes an uplink: hello `uplink: true` or an `uplink` message
  marks it, it is pinged every 4 min, both ends allow 10 min of silence, and it survives the last
  viewer leaving. The member lists its uplinks in its hellos (`uplinks`), watchers ask it first for a
  relay, and it reaches the machine with a `tunnel-callback`. Reconnects back off from 1 to 30 min;
  if no member can answer at all, retries stop until the machine's addresses change or a viewer
  attaches.
  This is the one deliberate exception to "silent unless watched", and only for unreachable machines;
  `uplink: off` disables it.
* **Second-hand state.** A daemon accepting a peer connection sends one `sync` with what it last knew
  of every other machine (seen within 7 days). The receiver, if watching, keeps an entry only for
  machines it has no live link to and only when newer than its own, marked `reported:<id>`, so an
  unreachable machine shows "last seen 10m ago, reported by forge" rather than an older local cache.
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
* **Security.** All traffic is TLS 1.3 with mutual authentication. See [SECURITY.md](../SECURITY.md)
  for the threat model. Transcript reads are sandboxed to `~/.claude/projects`. Viewers (VS Code, the
  CLI, the web app's server) must connect from the same machine over loopback, holding that machine's
  own certificate; a viewer hello from anywhere else is refused.
* **Machine identity.** Every machine has its own key (`machine.key`, ECDSA P-256) and a certificate
  naming it (`machine.crt`): a `vineyard://machine/<id>` URI SAN, the `vineyard` DNS name older daemons
  check, and CA rights so it can sign for machines it invites. The chain runs leaf first through the
  machines that vouched for it to a root in `fleet.crt`. After the handshake a daemon admits a
  connection only if no key in its verified chain is revoked, a peer's hello names the machine its
  certificate names, a dial reached the machine it was aimed at, and a viewer holds this machine's own
  certificate. The keys members present on direct links are kept in `memberKeys`; they are never
  learned second-hand.
* **Joining issues a certificate.** The joiner makes its key and sends only the public half in
  `join`. The inviter signs it with its own key and returns the chain, the roots and the peers. No
  private key leaves either machine, and there is no key anywhere that can mint arbitrary identities:
  `vineyardd init` for a new fleet signs the first machine with a root and throws the root's key away.
  SSH setup does the same through `vineyardd keygen` on the target and the local-only `certify` op.
* **Removal revokes a key.** *Remove Machine* adds the machine's key fingerprint to the removal
  record and to `revokedKeys`, which every hello carries. A revoked key breaks every chain through it,
  so machines that joined through the removed one are cut off too. The removing machine lists them
  (`descendants`) and vouches again for the ones the user keeps: a `vouch` is a new certificate for the
  kept machine's existing key, signed by the remover, delivered on a live link or to the machine's next
  (refused) connection, and passed on in hellos until taken up. A machine cannot remove the machine its
  own chain runs through.
* **Migration from the shared key (0.3.23).** Before 0.3.23 every member held one shared certificate
  and `fleet.key`, whose possession was membership. On first start a 0.3.23 daemon signs itself a
  machine certificate with that key, offline. The result chains to the same root, so older daemons
  accept it. For `legacyUntil` (14 days) it still accepts the shared certificate from older daemons;
  once every known member has presented its own certificate, or the time is up, it refuses the shared
  one and deletes `fleet.key`.
* **Rotation is re-issue.** `rotatekey` (*Rotate Fleet Key…*, `vineyardd rotate-key`) removes and
  revokes any excluded machines, makes a new root, signs with it a certificate for every other
  member's existing key, and discards the root's key. The package (`reissue`: new root, certificates,
  a bridge certificate for the new root signed by the rotating machine, and the rotating machine's
  signature) contains no private key. Members accept it only from a signer whose key they already hold
  on record, add the root, switch to their new certificate and present the bridge behind it, so
  machines still on the old root can verify them and catch up from any hello within the grace period.
  After it only the new root is trusted, which also shuts out anything minted with the pre-0.3.23
  shared key. A lost laptop needs removal, not rotation.
* **Joining without SSH: invite codes.** Any member can mint a single-use code valid for 15 minutes
  (`vineyardd invite`, or *Vineyard: Create Invite Code*). The code is `vineyard:` + base64url JSON
  carrying the inviter's addresses (advertised name plus its LAN IPs), a 128-bit fingerprint of the
  inviter's certificate, and a 128-bit random token. The joiner dials the inviter, pins the
  fingerprint, presents the token and its public key over TLS 1.3, and receives its certificate, the
  roots and the peer list. The inviter accepts certificate-less connections **only while an invite is
  outstanding**, and such a connection may send exactly one `join`. This is how Windows boxes (no SSH
  server) and machines without key-based SSH get in; the extension bundles binaries for every platform
  so the joiner runs its own copy locally. Codes also work as
  `vscode://peter-dolkens.vineyard/join?code=…` links.
* **SSH is an optional bootstrap.** For machines you *can* SSH to, *Add Machine* copies the binary
  with `scp`, runs `vineyardd keygen` there, has the local daemon certify the printed public key, copies
  the certificate and roots back, and runs `vineyardd init && vineyardd install` remotely. Day-to-day
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
  cmd/vineyardd         CLI: init | run | install | uninstall | restart | status | probe | peer | invite | join | web
  internal/claude       collector (reads ~/.claude), state derivation (+ tests), cross-session message sender
  internal/managed      daemon-spawned sessions over stream-json: prompts, permission prompts, questions
  internal/mesh         TLS, framing, demand-driven peer subscriptions, viewer fan-out, request relay, invites
  internal/web          `vineyardd web`: the mobile web app's server and its viewer link to the local daemon
  internal/service      launchd / systemd / Task Scheduler installers (Windows needs no elevation: XML logon task, then HKCU Run key)
src/core                wire types + formatting shared by the extension
src/extension           VS Code extension: daemon client, fleet store, tree, chat panel host, setup over SSH
src/webview             chat panel UI (bundled separately; marked for Markdown)
src/web                 mobile web app: screens, fleet store, chat host, the iframe stand-in for VS Code
scripts/build-daemon.sh cross-compiles vineyardd into bin/: macOS arm64/amd64, Linux and Windows arm64/amd64/386
scripts/screenshots    renders the README screenshots from synthetic data (see its README)
docs/                   internals (this file) and the listing images; not shipped in the .vsix
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
  the local daemon in 512 KB chunks). The receiving daemon stages the file, checks the SHA-256, and
  refuses it unless its release signature (below) is valid, before running anything. It then runs
  `vineyardd version` on it to prove it executes on that platform, then hands over to it: the new
  binary copies itself into `~/.vineyard/bin`, re-registers the login service and restarts. No SSH,
  no polling, and nothing at all happens while every daemon is current. Turn it off with
  `vineyard.autoUpdateDaemons`; *Update Daemon* on a machine or *Update All Daemons* does the same
  by hand. Only the numeric part of a version is compared, so two different dev builds never
  overwrite each other; unparseable versions (`dev`) are never touched. Daemons older than 0.3.0 do
  not understand `upgrade`, so the first update of those goes over SSH (or the local installer) once.
* **Signed builds.** From 0.3.23 a daemon installs or stores a build only with a signature
  (`<binary>.sig`, carried in the final `upgrade` / `stage` chunk) from one of the two Ed25519 release
  keys compiled into it (`daemon/internal/release`), over the build's platform, version and SHA-256.
  The fleet key is not enough to put code on a machine. The release workflow signs every build with
  both keys (`cmd/vineyard-sign`, secrets `VINEYARD_SIGNING_KEY_A` and `_B`) and fails without them.
  Rotation replaces one key at a time (see SECURITY.md). A development machine can opt in to unsigned
  builds with `"allowUnsignedUpgrades": true` in its own config.json; it is never set over the mesh.

## Build and run

```sh
npm install
npm run build:all        # Go cross-compile into bin/ + esbuild bundle into dist/
npm test                 # node --test + go test
```

Press **F5** ("Run Vineyard") to launch an Extension Development Host. In the Vineyard view:

1. **Set Up This Machine** – writes `~/.vineyard/config.json`, starts a fleet (a root that signs this
   machine's certificate and is then discarded) and installs the login service. The view connects to
   the local daemon within a second.
2. **Create Invite Code** on this machine, then on another machine (with the extension installed)
   run **Join Fleet with Invite Code** and paste it. The joiner has its key certified by the inviter,
   gets the peers from it, installs its own service and shows up in both views. This needs only TCP reachability
   on the daemon port, no SSH.
3. **Add Machine** (alternative) – enter an SSH host. The extension detects the platform, copies the matching
   binary, has this machine certify the key the target makes for itself, copies the certificate back,
   runs `init` with the current peer list, installs the service, and
   registers the new peer locally. Repeat for each machine, from any machine.
4. Every daemon learns the other members and their addresses from hellos and persists them, so a
   machine that was set up from `studio` can itself be the orchestrator later.

Useful on any machine:

```sh
~/.vineyard/bin/vineyardd status        # fleet table from the local daemon
~/.vineyard/bin/vineyardd probe | jq    # this machine's snapshot without a daemon
tail -f ~/.vineyard/vineyardd.log
```

## Protocol (newline-delimited JSON over mTLS)

| Message | Direction | Purpose |
| --- | --- | --- |
| `hello {role: peer\|viewer, machineId, listen, addrs, peers, uplink, uplinks, revokedKeys, vouches, reissue}` | both | identity, every address it answers on, every other member it knows, whether this may be an uplink, which machines hold an uplink to the sender, revoked keys, vouches it carries and a re-issue in grace (once per connection) |
| `req dialback {addrs}` → `{reachable}` / `uplink` | peer→peer | "can you connect to me?"; mark this link as an uplink |
| `subscribe` / `unsubscribe` | peer→peer | "push me your snapshot on change" |
| `snapshot {snapshot}` | peer→subscriber | full self-report (idempotent, newest `at` wins) |
| `sync {entries}` | accepting peer→dialer | once per connection: last-known state of other machines |
| `removed {removals}` | peer→peer | a machine was just removed from the fleet |
| `removed {removals}` records carry `keys` | peer→peer | the removed machine's key fingerprints, revoked for good |
| `vouch {vouch}` | peer→peer | a new certificate for a machine whose chain ran through a removed one |
| `reissue {reissue}` | peer→peer | a new root and every member's re-issued certificate (see *Rotation is re-issue*) |
| `req rotatekey {exclude, graceHours}` | viewer→local daemon | re-issue every certificate under a new root |
| `req certify {machineId, publicKey}` | viewer→local daemon only | sign a new machine's key (SSH setup) |
| `req descendants {machineId}` | viewer→daemon | machines whose chain runs through that machine's key |
| `rekey {keys}` | pre-0.3.23 peer→peer | a shared key set; ignored from 0.3.23 |
| `ping` / `pong` | outbound side pings | liveness, 30 s |
| `fleet`, `update`, `peerstatus` | daemon→viewer | aggregated view for VS Code |
| `req {id, target, op, args}` / `res` | viewer→daemon→peer | `transcript` (tail or from a byte offset), `send`, `spawn`, `takeover` (end an observed session's process, wait for it to exit, resume it as a managed child; a pending AskUserQuestion is carried over as a `recovered` pending request whose answer goes in as a prompt), `respond`, `interrupt`, `stop`, `stoptask` (one background command or subagent of a managed session, via Claude Code's `stop_task`), `configure` (model / effort / permission mode of a managed session, via Claude Code's `set_model`, `apply_flag_settings`, `set_permission_mode` control requests; the reply carries the machine's Claude Code settings defaults, which the extension stores with the choice, and a later `spawn` given those as `basis` drops any remembered choice whose default has since changed; the daemon itself sends `get_context_usage` after the handshake, a model switch and a compaction), `login` (relay `claude auth login`: start → URL, code → result), `rename` (custom session title), `wake` (Wake-on-LAN + sleep-proxy nudge for a peer), `sessions` (past transcripts for a workspace or machine), `kill` (terminate an observed session's process), `probe`, `addpeer`, `removepeer`, `invite`, `upgrade` (chunked daemon binary, see *Staying up to date*), `version`, `webapp` / `webpair` / `webdevices` / `webrevoke` (see *The web app*) |
| `join {token, machineId, listen, publicKey}` / `joined {cert, chain, peers}` | joiner→inviter (no client cert) | one-shot enrolment while an invite is active: the roots and the joiner's certificate chain |
| `tunnel` / `tunnel-in` / `tunnel-callback` / `tunnel-accept`, `tunnel-ok` / `tunnel-fail` | requester→relay→target | one-hop relay setup; first line of a fresh connection (callback travels on an existing link) |

## The web app

The daemon serves a mobile web app when `webApp` in its config.json is set (e.g. `":7735"`). The app
is embedded in the binary, built from `src/web` by esbuild into `daemon/internal/web/static/build`.
`vineyardd web` runs the same server in the foreground for development, printing a pairing code.

* **Turned on by a VS Code setting.** `vineyard.webApp.enabled` (off by default, with `.port` and
  `.machines`) is fleet-wide. Turning it on in a VS Code install first shows a modal saying a paired
  phone can drive every agent, the app is plain HTTP, and securing the route is up to the user; until
  they confirm, that install asks no daemon to start it, and cancelling turns the setting back off.
  The choice then goes out as `req webapp {listen, at}` to each online machine whose snapshot
  `webApp` differs from it. `at` is when the setting was changed; a daemon applies a choice only if
  it is later than the one it holds (`webAppAt`), so two windows with different settings cannot turn
  a machine on and off in turns. The daemon starts or stops the server in place and reports
  `webApp {listen, urls, error, at, devices}` in its snapshot, even while off, so an older daemon
  (no `webApp`) is asked at most once per version.
* **Pairing.** Every API call except `info` and `pair` needs a paired device. `webpair` (from VS Code)
  or `POST /api/pair/new` (from a paired browser) mints a single-use code, 8 characters from a
  31-letter alphabet, valid 10 minutes, at most 5 outstanding, carried in links as `#pair=CODE` (a
  fragment, so it never reaches a server log). Ten wrong codes drop every outstanding code and pause
  pairing for a minute. `POST /api/pair` redeems a code and sets `vineyard_device`, a 256-bit random
  credential, as an HttpOnly, SameSite=Strict cookie (Secure over HTTPS), out of reach of any script
  a transcript could inject. The machine keeps only its SHA-256, with the device's name and when it
  was paired and last seen, in `~/.vineyard/web-devices.json` (0600). `webdevices` / `webrevoke`
  and the app's Settings list and sign devices out. Pairing is per machine: a phone pairs with the
  machine whose address it opens, and sees the whole fleet through it.
* **One viewer link, only while watched.** The server connects to its daemon over loopback exactly as
  VS Code does (its machine certificate, `hello {role: viewer}`), and only while at least one paired browser
  holds the event stream. The page closes the stream whenever it is hidden, and the server drops the
  link 15 s after the last stream ends; the daemon then applies its own 30 s grace. Both the server and
  VS Code ping the daemon every 30 s over loopback, inside its 95 s idle limit.
* **Endpoints.** `GET /api/events` is a server-sent event stream: a `state` message (how the server's
  daemon link stands), then `fleet`, `update` and `peerstatus` exactly as the daemon sends them, with a
  comment line every 20 s. A new browser gets the cached fleet at once. `POST /api/req {target, op,
  args, timeoutMs}` relays one `req` and returns its `res`; `upgrade`, `stage`, `dist`, `rotatekey`,
  `addpeer` and the `web*` ops are refused. `/api/log` (the local daemon's log), `POST /api/restart`,
  `/api/devices`, `/api/devices/revoke` and `/api/pair/new` cover the local machine.
* **Other guards.** Acting requests need an `X-Vineyard` header (a cross-site page cannot add one
  without a CORS preflight, which the server never approves) and a matching `Origin`. Every request
  needs a `Host` that is an IP address, `localhost`, a single-label or `.local` name, this machine's
  hostname or one listed in `webAppHosts` (or `--allow-host`), which stops DNS rebinding. None of this
  encrypts anything: on plain HTTP a network observer can read the traffic and replay the cookie.
* **The chat is the VS Code webview.** `src/webview/main.ts` runs in an iframe (`chat.html`) with a
  stand-in for `acquireVsCodeApi` (`src/web/frameHost.ts`), and `src/web/chatHost.ts` does in the page
  what `chatPanel.ts` does in the extension. The few things iOS only allows inside the tap that asked
  for them (the file picker, copying the session id, opening links) happen in the frame. Photos are
  redrawn as JPEG at up to 2048 px when they are HEIC or over the 5 MB image limit. On a touch screen
  Return makes a new line and the button sends; the menu hides what needs VS Code (open workspace,
  terminal, raw transcript).
* **Settings live in the browser.** View, sort and chat settings and the per-workspace model, effort
  and mode memory (`SessionPrefStore`, the extension's class over `localStorage`) are per phone.
* `npm run build:web` builds the app; `vineyardd web --dev daemon/internal/web/static` serves it from
  disk so `node esbuild.mjs --web --watch` output shows on reload. `scripts/build-daemon.sh` builds the
  app before cross-compiling; a daemon built without it serves a page saying so.

## Managed vs observed sessions

| | Observed (Claude extension, terminal) | Managed (started or resumed from Vineyard) |
| --- | --- | --- |
| How state is read | transcript tail + registry, polled 1/s while watched (stat-cached) | same, plus control events from the process |
| Send a prompt | cross-session messaging socket | stdin (stream-json) |
| Permission prompts | shown; answer in the originating UI | Allow / Allow-and-remember / Deny cards |
| AskUserQuestion | shown; answer in the originating UI | option buttons + free text |
| MCP elicitations | answer in the originating UI | form from the server's schema, or a link to open; Decline / Cancel |
| Interrupt / stop | no | yes |
| Lifetime | independent | child of the daemon; ends if the daemon restarts |
| Becoming managed | *Take Over Session*: process ended, same session resumed as a daemon child; a pending question is asked again in the chat | — |

## Taking over a session

Observed sessions (started outside Vineyard) cannot have their permission prompts or questions
answered from here: Claude Code only accepts those in the originating UI. *Take Over Session* (in the
tree, on the card an observed question or permission shows, or in the chat's / menu) is the way through.

* The daemon on that machine ends the session's process, waits for it to exit, and resumes the same
  session id as its own child with no model, effort or mode overrides, so Claude Code restores the
  session's own settings. The pane or terminal it ran in loses the session; the transcript is kept and
  continues in the chat. Claude Code has no way to attach a second controller to a running session
  (cross-session messages cannot approve anything and slash commands in them arrive as text), so a
  takeover is always end-and-resume.
* **A pending question travels along.** When the session was blocked on an `AskUserQuestion`,
  Vineyard captures the question from the transcript before the process ends and shows it again as
  a card once the session is back. The dangling tool call itself is dropped by Claude Code on resume
  and never gets a `tool_result`, so the answer is delivered as a user message worded like the
  tool_result Claude Code writes for an answered question. The resumed Claude still has the
  question in its context and reads the answer as such (an answer like "the second one" resolves
  correctly). Sending a `tool_result` block for the dangling call does not work: Claude Code does
  not pair it and asks again.
* **What is lost.** An idle session or one waiting on a question loses nothing. A session mid-turn
  loses the running tool's output (Claude continues without it), and one waiting for permission
  loses that tool call, so those ask first.
* **`vineyard.chat.observed: "takeOver"`** makes opening the chat of an observed session take it
  over: silently when it is idle or asking a question, with a confirmation (or *Just watch*) when it
  is mid-turn. The default, `"observe"`, keeps the read-mostly chat and leaves takeover to the
  explicit actions.

## Known gaps

* Windows daemon and installer are compiled and reasoned about but not yet tested on a real box; the
  Claude project-directory encoding on Windows is a best guess.
* Installing on a Mac over SSH needs the target user to have a GUI login for `launchctl bootstrap`;
  the installer falls back to `launchctl load -w`.
* Relays are one hop: a machine that no member you reach can reach either shows last-known state only.
* Only Claude Code is detected.

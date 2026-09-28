# Security

## Reporting a vulnerability

Please report security problems privately through
[GitHub's vulnerability reporting](https://github.com/peter-dolkens/vineyard/security/advisories/new)
rather than in a public issue. Include the Vineyard and vineyardd versions (`vineyardd version`) and
what an attacker needs to be able to do first. Fixes go into the next release; only the latest
release is supported.

## What Vineyard protects

Vineyard gives whoever runs it control of Claude Code on every machine in their fleet. That is the
asset: a fleet member can read every transcript, start agents in any folder (including with
permissions bypassed), answer their prompts and end their processes. In effect it can run code as
your user on every machine. Everything below is about keeping that ability inside the fleet.

It also carries transcripts between machines, which can contain source code, secrets pasted into a
chat, and the output of any tool an agent ran.

## Who is trusted

* **Every fleet member, fully.** A machine in the fleet can drive agents on every other. There are no
  per-machine permissions. Add machines you control.
* **Your user account on each machine.** The daemon runs as you, keeps its keys in `~/.vineyard`
  (mode 600, directory 700 on macOS and Linux), and accepts viewers (VS Code, the CLI, the web app's
  server) only over loopback with that machine's own certificate. Other local users are kept out by
  file permissions. On Windows those modes are not mapped to ACLs yet; see *Known gaps*.
* **The release keys** (below), and whoever can run the release workflow.
* **Claude Code**, for what agents are allowed to do. Vineyard shows Claude Code's permission prompts
  and relays your answers; it does not add its own sandbox.

## Who is defended against

| Attacker | What they get |
| --- | --- |
| Someone on the network between two machines | Nothing: every connection is TLS 1.3 with both ends authenticated. Relayed links are encrypted end to end; the relaying member sees only ciphertext. |
| A machine that is not a member | Nothing: it has no certificate that leads to a fleet root. The one exception is a pending invite code (single use, 15 minutes), which admits one machine. |
| A lost or stolen member | Until you remove it, whatever a member can do. After *Remove Machine*, nothing: its key is revoked on every machine, along with every certificate it signed. It cannot mint a new identity, because no machine holds a key that can. |
| A member pretending to be another | Refused: a machine's certificate names it, and its hello must match. |
| A member pushing a malicious daemon build | Refused: daemons install only builds signed with a release key. |
| A web page open in a browser on the same network | Nothing from the web app: acting requests need a header a cross-site page cannot send, a matching Origin, and an allowed Host name (which also stops DNS rebinding). |
| Text in a transcript (tool output, web pages an agent fetched) | Markdown is sanitised before it is shown. The web app's device credential is an HttpOnly cookie that page scripts cannot read. |
| Someone who steals the release signing secrets | They can sign a build every daemon accepts. See *Release keys* for rotation. |

## Mechanisms

### Machine identity

Each machine has its own ECDSA P-256 key and a certificate that names it. A machine that joins gets
its certificate from the member that invites it (or sets it up over SSH), signed with that member's
own key. The joiner sends only its public key; no private key ever travels. A new fleet's root key
signs the first machine's certificate and is then thrown away.

A daemon admits a connection only if the certificate chain leads to a fleet root and contains no
revoked key, the certificate names the machine the hello claims to be (and the machine that was
dialed), and a viewer holds this machine's own certificate.

Removing a machine revokes its key permanently, and every hello passes the revocation on. Machines
that joined through it lose access too, unless the removing machine vouches for them again, which
means signing their existing key itself. VS Code and the web app list those machines and ask.

### Migration from the shared fleet key

Before 0.3.23 every member held the same key (`fleet.key`), and holding it was membership. On first
start, a 0.3.23 daemon uses that key once, offline, to sign itself a machine certificate. For 14 days
it still accepts older daemons presenting the shared certificate. Once every known member has its own
certificate, or the 14 days are up, it refuses the shared certificate and deletes `fleet.key`.

The old key still signs valid certificates for as long as the original root is trusted. If a machine
that held it may be in the wrong hands, run **Rotate Fleet Key** (below) once the fleet has migrated.

### Rotate Fleet Key (re-issue)

Rotation makes a new root, signs a certificate with it for every current member's existing key, and
throws the root's key away. Machines left out are removed first. The package that goes out holds only
public certificates. It is signed by the rotating machine, and members accept it only from a key they
already hold on record for that machine. A bridge certificate lets machines that are still on the old
root verify their peers and catch up within the grace period. After the grace period only the new
root is trusted.

### Signed daemon builds

The fleet updates itself, so a daemon must not install whatever a member sends it. From 0.3.23 a
daemon checks a build's signature before running it. The signature covers the build's platform,
version and SHA-256, and must come from one of the two Ed25519 release keys compiled into the daemon
(`daemon/internal/release/release.go`). The release workflow signs every build with both keys and
fails if either secret is missing. A development machine can accept unsigned builds by setting
`"allowUnsignedUpgrades": true` in its own `config.json`; that setting is never sent over the mesh.

### Release keys

The two keys are `A` and `B`. Every release is signed with both, and a daemon accepts either. They
are rotated one at a time, so a daemon that has not updated yet can always verify the release that
replaces a key.

To replace key A (routine rotation, or A is suspected leaked):

1. Generate a new key A′ (an Ed25519 seed, base64) and store it as the `VINEYARD_SIGNING_KEY_A`
   secret, keeping an offline backup.
2. In `release.go`, replace A's public key with A′'s. Leave B alone.
3. Release. The build is signed with A′ and B. Daemons that still trust {A, B} accept it through B.
   After updating, they trust {A′, B}.
4. Wait until the fleet has updated before rotating B the same way.

If A leaked, a daemon that still trusts A accepts builds signed with A until it updates. Pushing a
build to a daemon also requires fleet membership, so the release that retires A should go out
promptly. If both keys leak, replace one as above in an emergency release, then the other.

### Web app

The web app is off by default (`vineyard.webApp.enabled`), and turning it on asks you to confirm, on
each computer, that securing the route is up to you. A phone must pair with a single-use code, and
then holds a random device credential in an HttpOnly, SameSite=Strict cookie. The daemon keeps only
the credential's SHA-256. The credential lasts a rolling 30 days: every visit renews it, and devices
unseen for 30 days are signed out. The web app cannot push builds, rotate keys, add machines or create
invite codes.

## Known gaps

* **The web app is plain HTTP.** Anyone on the same network can read what it shows and copy a paired
  phone's cookie. Put it behind TLS yourself (Tailscale, a VPN, a reverse proxy) and firewall the port.
* **Membership is all or nothing.** There are no per-machine or per-operation permissions, and no
  extra confirmation for dangerous operations from a paired phone.
* **Windows file permissions** for `~/.vineyard` are not yet set as ACLs, so other local users of a
  Windows machine may be able to read its key.
* **No outside review, no fuzzing** of the wire protocol or the web app's handlers yet.
* **One-hop relays** mean a machine that no reachable member can reach shows last-known state only.
  This is an availability gap, not a security one.

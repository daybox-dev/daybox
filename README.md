# <picture><source media="(prefers-color-scheme: dark)" srcset="web/logo-dark.svg"><img src="web/logo.svg" width="28" alt=""></picture> daybox

Personal ephemeral-compute control plane. A cheap always-on Hetzner VPS (the
**little box**) summons a beefy hourly-billed server (the **big box**) on
demand, and reaps it when idle. Everything that matters survives the reap on a
persistent volume, so the big box itself is disposable.

```
mac laptop ───ssh───▶ little box (always-on)  ───Hetzner API───▶ big box (ephemeral)
daybox (Go CLI)       · daybox CLI                                · ccx33 by default
                      · idle reaper, every 5min                   · billed per hour
                      · production services                       · /work volume mounted
                                                                    (claude auth · repos ·
                                                                     git identity persist)
```

**Why this shape:** Hetzner's 2026 price hikes made a bigger always-on box
unattractive, and running claude on the 4GB little box OOM-killed it. So the
little box stays small and cheap (its real jobs: production services + control
plane), and heavy work runs on a burst box that only bills while it exists.

## Why not just use X?

The differentiator isn't the networking — it's tying **lifecycle** (the box
exists, and bills, only while you're using it) to **memory** (it comes up
already knowing your repos, your auth, your tmux session). Nobody else puts
those on the same plane:

- **Tailscale?** A mesh VPN gets you *into* a box; it doesn't create, bill,
  or reap one, and it has no memory of your workspace. daybox owns the
  lifecycle and *embeds* the same audited WireGuard/tsnet stack for the
  networking — it's built on that layer, not competing with it.
- **GitHub Codespaces?** Runs on GitHub's compute and GitHub's bill, scoped
  to a repo, with an environment that's rebuilt rather than *remembered*.
  daybox runs in **your** cloud account (any repo, one persistent workspace
  that survives across sessions) and bursts to a real machine — 8+ cores,
  32GB+ — at hourly-VPS prices.
- **Coder?** A capable team platform, but it's a server to run and Terraform
  to write. daybox is a single Go binary and a ~€4/mo VPS, personal-first
  and zero-dependency; the whole control plane is `curl … | sh` then
  `init`.
- **fly.io / devbox SaaS?** They resell you compute — their account, their
  margin, their cryptominer-and-stolen-card abuse problem. daybox is **BYO
  cloud**: you bring a scoped credential, your provider bills you directly,
  and we never touch your compute plane. Same shape as Tailscale (charge for
  coordination, the data plane is yours) — for compute.

Honest scope: v1 is **Hetzner today** (AWS-with-spot next; the provider
interface is open), single-user-per-control-plane, and defends *inbound*
while leaving *outbound* wide open on purpose — see [SECURITY.md](SECURITY.md),
which is written to be read before you trust it.

## Quick start

**`daybox init` once, `daybox up` forever.** On your laptop:

```sh
git clone <this repo> && cd daybox
(./cmd/daybox/build.sh)                                # until prebuilt releases exist
install -m 755 dist/daybox-darwin-arm64 ~/bin/daybox   # (or -linux-amd64)

daybox init     # interviews you; provisions a new control-plane VPS from a
                # Hetzner token (or adopts a box you already ssh to), sets up
                # the coordination server + reaper, enrolls this device, and
                # writes ~/.config/daybox/config.local for you
daybox up       # summon and ssh in — auto-reaps ~30min after you leave
```

Everyday verbs, all from the laptop: `up` · `ssh` · `status` · `down` ·
`net`. Attach is plain ssh + tmux — any terminal. `init` is idempotent:
re-run it any time to heal drift.

**Upgrading.** `daybox upgrade` moves an existing deployment to a newer
release: it fetches the signed payload (latest, or `--version vX.Y.Z`),
replaces `~/daybox` on the control plane (previous tree kept at
`~/daybox.prev` for rollback), refreshes the agent binary, and re-runs the
same idempotent setup — no interview, and nothing that *is* your deployment
(config, net, token, volumes, profiles) is touched. New boxes summon at the
new version; a running box keeps the version it was summoned with until
reaped (`daybox-agent version` on a box tells you which). The laptop binary
doesn't self-update: re-run the installer, or `cmd/daybox/build.sh` +
`install.sh` from a checkout.

<details>
<summary>Manual setup (what init automates)</summary>

- Control plane by hand: `./install.sh` on a Linux box, write
  `~/.config/daybox/config.local` with the REQUIRED values (see
  [Configuration](#configuration)), store a project-scoped Hetzner token at
  `~/.config/daybox/token` (mode 600, see [SECURITY.md](SECURITY.md)), drop
  device pubkeys in `~/.config/daybox/keys/`, run `daybox setup`.

</details>

### Phone / anywhere

ssh to the little box (e.g. Termius), then `daybox attach` for the big
box's persistent tmux session (`daybox ssh` gives a plain shell instead).

## Everyday use over the net

Once a device is enrolled (`daybox init` does this; [add another](#the-net)
with `daybox enroll`), the big box is a plain ssh target over your private
net. Put this in `~/.ssh/config` — then `ssh daybox`, `scp`, rsync, and VS
Code Remote all work transparently:

```
Host daybox
  User dev
  ProxyCommand ~/bin/daybox-agent dial -control http://EXAMPLE_IP:8080 %h %p
  UserKnownHostsFile ~/.config/daybox/net_known_hosts
  ForwardAgent yes   # git push from the box without keys on it (see SECURITY.md)
```

The box's public `:22` is dark to the internet; the only ways in are the net
and the control plane (see [The net](#the-net)). The box's host key is the
same over every path — take the `HOSTKEY` line from `daybox up` output, host
field `daybox`.

## How it works

### Roles

- **Laptop (mac)** — `daybox` (Go CLI): asks the control plane to summon,
  then delegates the interactive `ssh`/`attach` back through it (the box's
  public `:22` is dark under ingress lockdown); `enroll`/`dial` put the
  device on the net.
- **Little box (control plane)** — `daybox`: owns the Hetzner API token, the
  summon/reap lifecycle, and the idle reaper. Also runs personal production
  services; heavy compute never runs here (4GB RAM, no swap — claude gets
  OOM-killed, which is why daybox exists).
- **Big box** — ephemeral ccx33 (default), provisioned from scratch by
  cloud-init on every summon. Nothing on its root disk matters.
- **Workspace volume** (one per profile, `daybox-<profile>-vol`, 50GB) — the
  "memory". Mounted at `/work`; holds repos, your git identity, and a
  dedicated GitHub ssh key. Whatever else should survive a reap — a tool's
  auth, its installed versions — is symlinked onto it by that profile's
  `[persist]` block, so a fresh box comes up warm. See
  [Profile seeds](#profile-seeds--what-a-box-carries).

### Summon lifecycle (`daybox up`)

1. If a server named `$SERVER_NAME` already exists: reset idle counters, emit
   connection info, done (idempotent reconnect).
2. Ensure the volume is detached (it can only attach to one server).
3. `POST /servers` with the rendered cloud-init user-data, ssh key names
   resolved at `daybox setup` time, and the volume attached.
4. Poll until `running`, then `wait_ready`: port 22 open → `ssh-keyscan` the
   host key (from inside Hetzner's network, before any client connects) →
   poll until provisioning writes its verdict. A failed step fails the summon
   loudly; there is no "connect anyway".
5. Emit the stdout contract (everything else goes to stderr, so clients can
   parse stdout blindly):

   ```
   IP <address>
   HOSTKEY <known_hosts line, host field rewritten to that address>
   ```

`daybox render` prints the fully rendered cloud-init payload — use it to
sanity-check template changes before a paid summon.

### Idle reaper (`daybox reap`, every 5min via a systemd user timer)

Probes the big box over ssh for (a) established inbound `:22` connections,
excluding the control plane's own probe by `$LITTLEBOX_IP`, and (b) 1-minute
loadavg:

- busy (connections > 0 **or** load ≥ `$LOAD_BUSY`) → reset idle counter
- idle → increment; at `REAP_AFTER_IDLE_MIN` (30min) → `daybox down`
- unreachable → separate counter; at 1h → force reap, because an unreachable
  server still bills

There's a third busy signal for detached agents: **no claude transcript
writes in the last 10min** is part of "idle", so a detached claude keeps
working unreaped even though API-bound work shows near-zero load. `daybox
down` cleanly unmounts `/work`, detaches the volume, then deletes the server.
Counters live in `~/.config/daybox/state/`.

Deliberate consequence: ssh sessions ride plain **sshd** everywhere (no
tailscale-ssh), so the established-`:22` probe stays an accurate liveness
signal.

### The net

The access layer is a self-hosted **Headscale** coordination server on the
control plane plus **`daybox-agent`**, a single static Go binary embedding a
userspace tsnet node. No TUN device, no root, no vendor app, no OS VPN slot —
it coexists with a work Tailscale on macOS and doesn't even count as a VPN on
iOS.

```
mac (daybox-agent dial) ──┐
                          ├──▶ headscale @ littlebox:8080 ──▶ devbox agent
future devices ───────────┘    (coordination only; traffic     (ephemeral node,
                                goes peer-to-peer, e2e         proxies net :22
                                Noise-encrypted)               → local sshd)
```

**Devboxes join automatically.** `daybox up` mints a single-use, 15-minute,
ephemeral pre-auth key, pushes the agent + key + unit over the allowlisted
ssh path, and waits until headscale reports the node online. The box appears
as `daybox` / `100.64.0.x`; after a reap, headscale garbage-collects the node,
so the net table in `daybox status` never shows ghosts. **A failed join is
fatal** — the summon
tears the box down rather than leave a box that isn't on the net.

**Enrolling a device:**

```sh
daybox enroll              # narrated; mints a key over ssh, joins, pins the name
daybox enroll -device mac  # explicit device name
```

`daybox init` runs this automatically. Device keys expire after 30d; a lapsed
device re-enrolls with the same command, and its name and net address stick.

**Ingress model.** Every summoned box is default-deny inbound from the
first moment of boot: a raw netfilter policy laid down in cloud-init's
earliest stage (before packages, before provisioning, before anything can
fail), with host `ufw` taking over as the canonical policy when provisioning
starts — public sshd is **never exposed to the internet**. The only
ways in are the net (the agent proxies net-side `:22` to `127.0.0.1:22` over
loopback, which ufw always permits) and the control plane's public IP (reaper
probe + volume unmount). There is no off-net mode: the net is a
**precondition** of `daybox up`, and a failed join tears the box down — so a
box can never exist with the lockdown but no net path in. Egress is
unrestricted (see [SECURITY.md](SECURITY.md)).

Control traffic is Noise-encrypted (ts2021) over plain http; with the hub's
static public IP, peer traffic connects direct (DERP relays are fallback only,
and still e2e-encrypted). Supply chain: headscale from a pinned,
checksum-verified release; agent deps pinned by `go.sum` in-repo; both bump
deliberately, never auto.

### Profiles

A **profile is a whole daybox** — its own server, its own volume, and its own
single set of credentials (git identity, `gh` account, ssh key, claude auth),
all living on that volume. Profiles summon and reap independently and share
the one net. `work` and `personal` are profiles; bare commands use `default`.

That's the entire model — the unit of separation is the profile. Want
different credentials? Make a different profile. Want several projects to
share one box and account? Clone several repos into that profile's `/work`.

```
daybox profile add <name> [type]   # interview → write profile config + create its volume
daybox profile ls                  # each profile: box up/down, hourly burn, volume size
daybox profile use <name>          # set the profile bare commands resolve to
daybox profile rename <old> <new>  # (box must be down)
daybox profile rm <name> [--purge] # reap box; keep the volume unless --purge

daybox up   -p <name> [type]       # -p is a flag; the positional stays the server type
daybox ssh  -p <name>
daybox down -p <name>
daybox status                      # everything: ALL profiles' boxes + the net table
```

`SERVER_NAME`/`VOLUME_NAME` are **derived** from the profile name
(`daybox-<profile>` / `daybox-<profile>-vol`), never configured. Net
membership is deployment-wide (every profile's box joins the same headscale
under a distinct name), so same net, different boxes, different creds.

### Profile seeds — what a box carries

daybox ships only **substrate**: the volume mount, your git identity and key,
tmux, and the ingress lockdown. Its `packages:` list is exactly `tmux`, `ufw`,
`git` — tmux because `daybox attach` is built on it, ufw because it *is* the
ingress boundary, git because daybox's own machinery assumes it.

Everything else your box carries is declared by that profile, in
`~/.config/daybox/profiles/<name>/profile.toml` on the control plane:

```toml
packages = ["ripgrep", "jq", "build-essential"]     # apt, every boot
repos    = ["git@github.com:you/thing.git"]         # cloned into /work/repos

[persist]                          # symlinked onto the volume; survives reaps
".claude"             = "claude"
".local/share/claude" = "claude-share"

[setup]
once       = ["curl -fsSL https://claude.ai/install.sh | bash"]
every_boot = ["npm install -g pnpm@10"]
```

`daybox profile add` writes one from a template; `daybox profile seed
show|init|path` manages it.

**`once` vs `every_boot`** is the distinction that matters. The root disk is
rebuilt on every summon, so anything installed there must be reinstalled each
boot (`every_boot`). Anything that installs into `/work` persists, so its
installer should run `once` per volume — re-running it every boot is a wasted
download and a repeated supply-chain exposure. Editing a `once` command makes
it run again, because the declaration changed.

**daybox does not vouch for `[setup]`.** Those strings run verbatim as your
login user. A line piping an installer into a shell is your informed choice,
recorded in a file you own — the product itself installs nothing third-party.

**Nothing fails quietly.** Any failing step aborts provisioning, `daybox up`
prints the failing step and exits non-zero, and the box is left running so you
can `daybox ssh` in and look. A box that came up missing what you declared,
while reporting success, is the failure this design exists to prevent.

### State inventory

| Where | What |
|---|---|
| repo (synced via git) | scripts, config template, cloud-init, pubkeys, units |
| `~/.config/daybox/` (deployment) | `token`, `config.local`, `state/ssh_key_names.json`, `state/current_profile`, `profiles/<name>/config` |
| `~/.config/daybox/state/profiles/<name>/` | that profile's `volume_id`, idle/unreachable counters, `known_hosts` |
| volume `/work` (per profile, survives reaps) | repos, claude auth/state, git identity, `gh` auth, GitHub ssh key |
| big box root disk | nothing worth keeping — ever |

## Configuration

Your deployment lives in `~/.config/daybox/config.local` (never committed) —
`daybox init` writes it for you; edit by hand to tweak. Only two things are
required; everything else has a default baked into the tooling.

**Required:**

| Key | Meaning |
|---|---|
| `LITTLEBOX_IP` | The control plane's public IP. The reaper excludes its own probe by it, the box's ingress firewall allowlists it, and the net's control URL derives from it. |
| `GIT_NAME` / `GIT_EMAIL` | The default profile's git identity. (A profile config can override it.) |

**Optional (defaults shown):**

| Key | Default | Meaning |
|---|---|---|
| `PROVIDER` | `hetzner` | Which `providers/<name>.sh` implements the cloud — see [Providers](#providers). |
| `SERVER_TYPE` | `ccx33` | 8 dedicated vCPU / 32GB. Per-summon override: `daybox up ccx43`. |
| `IMAGE` | `ubuntu-24.04` | Big-box base image. |
| `LOCATION` | `hil` | Must match the workspace volume's location. |
| `VOLUME_SIZE_GB` | `50` | Workspace volume size. |
| `REMOTE_USER` | `dev` | Login user created on the big box. |
| `REAP_AFTER_IDLE_MIN` | `30` | Delete after this long with no ssh + low load. |
| `LOAD_BUSY` | `0.40` | 1-min loadavg at/above this counts as busy. |
| *(profile seed)* | — | What the box carries is **not** a config key — see [Profile seeds](#profile-seeds--what-a-box-carries). |
| `MAX_LIFETIME_HOURS` | `12` | Hard cap: a box older than this is reaped **even if busy**. The runaway backstop — see below. `0` disables it. |
| `NET_USER` | `dev` | Headscale user owning the net. |
| `NET_PORT` | `8080` | Headscale control port on the control plane. |
| `NET_CONTROL_URL` | `http://$LITTLEBOX_IP:$NET_PORT` | Override only for a custom control URL. |
| `CONTROL_HOST` | `dev` | ssh host alias of the control plane (the laptop CLI reads this). |

`SERVER_NAME` and `VOLUME_NAME` are **not** knobs — they derive from the
profile. Per-profile overrides live in
`~/.config/daybox/profiles/<name>/config` (same keys, plus that box's git
identity).

## Providers

Everything daybox needs from a cloud is five primitives — **summon / reap /
probe / attach-volume / user_data** — and every cloud API call lives behind
them, in one file: `providers/<name>.sh`. The core control-plane CLI parses
only the contract's normalized server JSON (`{id, name, ip, status, created,
type}`) and never talks to a provider API directly; supporting a new cloud
is writing one new file that implements the contract, selected with
`PROVIDER=<name>` in `config.local` or a profile's config (default:
`hetzner`, the reference implementation).

The full contract — each function's arguments, stdin/stdout, failure
behavior, and the exact environment a provider file may assume (it is
re-sourced per profile, so the reaper's loop probes every profile through
*its* provider; anything a provider persists is namespaced under
`state/providers/<name>/`) — is documented at the top of
[`providers/hetzner.sh`](providers/hetzner.sh). Two suites keep a provider
honest: `scripts/test-provider-select.sh` (free, stubs) proves the right
file is loaded per profile and one profile's choice never leaks into the
next, and `scripts/test-provider-conformance.sh` (a few cents of real cloud
spend) walks the full lifecycle — summon → mount → net join → reap → zero
ghosts → volume purge — on a throwaway profile. Run the conformance suite
before trusting any new provider file.

AWS-with-spot is the intended second implementation: state lives on the
volume and the reaper already treats a vanished box as survivable, so spot
interruption maps onto the existing lifecycle. (The Go CLI's
`init --provision` still speaks Hetzner directly for the one-time
control-plane VPS; folding that into the same contract is queued.)

## Cost model

| Thing | Billing |
|---|---|
| little box | flat monthly (already paying for services) |
| big box (ccx33 default) | hourly, only while it exists |
| workspace volume (50GB) | ~€2.9/mo, always |

The reaper force-reaps after 1h unreachable so a zombie never bills overnight.

**The runaway backstop.** The idle reaper handles the normal case — you stop
working, the box goes ~30 minutes later. It cannot help with the pathological
one: a box that *always looks busy* (a runaway process, a forgotten session)
bills until someone notices. So `MAX_LIFETIME_HOURS` (default 12) force-reaps
a box past that age regardless of every busy signal. `daybox status` always
shows the age, the spend so far, and the time remaining, and the reaper warns
in the last 30 minutes. Your workspace volume survives the reap — what you
lose is the root disk and running processes, not your repos or state. Raise it
if you genuinely run long jobs; set `0` to disable it and rely only on the
idle reaper.

## How machines stay in sync

The clone **is** the installation: `install.sh` symlinks live paths
(`~/.local/bin/daybox`, `~/.tmux.conf`, …) back into the repo, and the
`daybox` CLI resolves its config, cloud-init template, and authorized keys
relative to its own (symlink-resolved) location. So:

- edit on any machine → commit + push → `git pull` elsewhere → done. (Re-run
  `./install.sh` only when systemd units change — those are copied, not
  symlinked. The mac's `~/bin/daybox` is a build artifact: re-run
  `cmd/daybox/build.sh` + `./install.sh` after Go changes.)
- the big box is provisioned from the repo's `remote/` files and `keys/` at
  summon time, so it always matches whatever the control plane has pulled.
- adding a device = drop its pubkey in `keys/`, run `daybox setup` once.

What is **not** in the repo (machine-local, in `~/.config/daybox/`): your
deployment — `config.local`, the Hetzner API `token`, device pubkeys in
`keys/`, the agent binary, and runtime `state/`. Nothing you edit in the repo
can change whose deployment a machine is; that lives in each machine's
`config.local`.

## Layout

```
cmd/daybox/         the Go CLI + daybox-agent net node (tsnet; ./build.sh)
bin/daybox          control-plane CLI (summon/reap/status/net; curl+jq only)
providers/          the provider contract + implementations (hetzner.sh)
headscale/          coordination-server config template (config.template.yaml)
cloud-init/         big-box provisioning template — SUBSTRATE ONLY
profile.default.toml  the seed template a new profile starts from
remote/apply-seed.py  applies a profile's seed on the box
remote/             tmux.conf + devbox-tmux + agent unit installed on boxes
keys/               fallback pubkey dir (deployments use ~/.config/daybox/keys/)
systemd/            idle-reaper service + timer (user units, %h-relative)
install.sh          role-aware installer (mac | linux)
scripts/cut.sh      build release artifacts: cross-compile + checksum (offline half)
scripts/release.sh  sign + publish to R2 + verify the live release (never CI)
README.md           this file — everything a user needs
SECURITY.md         threat model, what's defended, what isn't, and why
```

## Security

daybox runs untrusted-ish code (your dependencies, your agents) on a
throwaway box in your own cloud account, on your own bill. It defends the
**inbound** boundary hard (default-deny from the first moment of boot,
net-only ingress, disposable compute) and leaves **outbound** open on purpose for
v1. Read [SECURITY.md](SECURITY.md) before you point it at a cloud account —
it's written to tell you exactly what you'd be accepting.

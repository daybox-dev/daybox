# Security

This is the honest version, written for someone deciding whether to run a
stranger's tool against their own cloud account. It says what daybox
defends, what it deliberately does **not**, and why each line is where it
is. If something here reads as a weakness, it's because we'd rather you see
it than discover it.

## The one-paragraph threat model

daybox runs untrusted-ish code (your dependencies, your agents) on a
throwaway box **in your own cloud account, on your own bill**. The primary
threat is **supply-chain compromise** — a poisoned npm/pip package that
runs on install (Shai-Hulud-class worms). The defended boundary is
**inbound**: nobody but you reaches the box. The explicitly *undefended*
boundary is **outbound**: it's your box and your bill, and locking down
egress tightly enough to matter without breaking `pnpm`/`git`/agent traffic
is a real project we've scoped for later, not faked for launch. Everything
below is a consequence of those three sentences.

## What's actually in place

- **Default-deny inbound, from the first moment of boot.** Ingress is locked
  down in cloud-init's earliest boot stage — before packages install, before
  provisioning runs, before anything can fail — with a raw netfilter policy
  allowing only the control plane's IP on `:22`; host `ufw` takes over as
  the canonical policy at the start of provisioning. Public sshd is never
  exposed, and there is no way to turn this off. Real access is over your
  private net (a userspace WireGuard mesh — no public listener). The net is
  a **precondition** of `daybox up`: a box that can't join is torn down
  rather than left reachable-but-off-mesh, and an already-running box is
  re-verified (provisioning verdict + net membership) before it is ever
  handed back. The one deliberate exception: a box whose provisioning
  *failed* is kept for inspection — locked down to the control plane only,
  never publicly reachable, never reported as ready, and still force-reaped
  by the lifetime cap.
- **BYO cloud — blast radius is one project, and it's yours.** daybox never
  runs compute on our account; you bring a **project-scoped** cloud
  credential that lives only on *your* control plane. Worst case for a
  leaked token is someone running up hourly bills in an isolated project you
  can cap and revoke — never your production account, never our other users
  (there are none on your plane; it's single-tenant by construction).
- **Disposable compute.** The box is rebuilt from cloud-init on every
  summon. An attacker who lands on it cannot persist on the *machine* — only
  on the volume, which is a much smaller, inspectable surface.
- **A minimal, declared substrate — nothing you didn't ask for.** The
  product installs three packages on a box (`tmux`, `git`, `ufw`);
  everything else arrives only because your profile's seed declares it.
  daybox does not vet, pin, or quarantine what you choose to install —
  the box is yours and what runs on it is your business. The defense
  against a poisoned dependency is not filtering but **scoping and
  disposability**: closed ingress, narrow revocable credentials, and a
  machine that evaporates (see "What a compromise still gets an attacker").
- **Scoped git identity, not your account key.** The volume self-seeds a
  *dedicated* ed25519 key — deliberately not your personal account key.
  Register it as a **read-only deploy key**, or a fine-grained PAT for
  pushing; a compromised box leaks a revocable, narrowly-scoped credential,
  not your GitHub account.
- **Host keys pinned at first scan — no fingerprint prompts.** The control
  plane `ssh-keyscan`s the fresh box seconds after boot and hands your
  laptop an exact `known_hosts` line — there's no "accept this fingerprint?
  (y/n)" prompt to rubber-stamp, and providers recycle IPs within hours, so
  this matters. Honestly named, this is trust-on-first-scan, not the absence
  of TOFU: an attacker in the network path during that first scan window
  could pin their own key. The window is seconds long, from the control
  plane's connection, against a box only it knows exists — but it exists.
- **A box can propose a profile change; only your laptop can approve one.**
  A profile's seed is root-at-boot on every future box, so a writable seed
  would be machine-persistence laundered through provisioning — the seed
  stays one-way. What a box *can* do is submit a proposal to the **agent
  relay** on the control plane: a small daemon (opt-in, disabled by
  default) that listens only on the private net, identifies the caller by
  net identity (WhoIs), binds it to the one profile it was summoned under,
  and stages the proposal as an inert file. Review happens on the laptop as
  a full diff with `[setup]`/`[persist]` changes flagged — the
  supply-chain-bearing lines are impossible to skim past — and nothing
  takes effect until you accept. Honestly said: the relay is the first
  custom resident daemon on the control plane (everything else there is
  headscale + sshd + a timer), and the node→profile binding is only as
  strong as the net identity a root-compromised box holds — which is why
  the relay's ceiling is proposal spam, never an applied change. The human
  diff review is the boundary; the relay just does the paperwork.
- **A hard cost cap (shipping for v1).** Independent of the idle reaper, a
  configurable max-lifetime / spend ceiling force-reaps a box regardless of
  activity, so a runaway process can't quietly bill all weekend. (The idle
  reaper handles the normal case: gone ~30min after you stop, force-reaped
  after 1h unreachable.)

## Deliberate non-goals for v1 (eyes open)

These are choices, not oversights. Each is defensible *because* inbound is
closed and the box is yours.

- **Egress is wide open.** We do not restrict outbound traffic. A poisoned
  dependency that phones home or exfiltrates *can* reach the internet — the
  ingress rules do nothing about that. We judge this acceptable for v1: it's
  your box and your bill, and an egress allowlist tight enough to matter without
  breaking normal `pnpm`/`git`/agent traffic is a genuine design problem we'd
  rather ship *right* than ship as security theater. Tracked as a post-launch
  item (egress story v2). If your threat model requires outbound control
  today, daybox is not yet for you, and we'd rather say so here.
- **Credentials are plaintext on the volume.** Your agent's auth token and
  any keys you seed live unencrypted on the persistent volume. This is
  acceptable *given* the box is single-tenant, inbound-closed, and
  disposable — the realistic path to those bytes is code you already ran on
  your own box, at which point at-rest encryption you also decrypted buys
  little. An age-encrypted, per-session secrets bundle is designed
  (see "How secrets get into a box" below) and is the planned upgrade; v1 documents the risk
  instead of half-solving it. Mitigate today by seeding only scoped,
  spend-capped, revocable keys.
- **No provider-firewall backstop.** We rely on the box's own host firewall
  plus the net, not a cloud-provider firewall (e.g. Hetzner Cloud Firewall)
  layered underneath. Adding one would harden the "box flushes its own ufw"
  case, but bakes a specific provider's API into an interface we intend to
  keep portable across clouds. We chose portability; the honest cost is one
  fewer belt-and-suspenders layer.

## The trust root: the repo, and no CI

The GitHub repo is the trust root — whoever can push to it can run code on
every machine that pulls it. Two consequences we take seriously:

- **The control plane holds no push credential.** Its tree is *pushed to it*
  by `daybox init` over ssh from your device; there is no GitHub write
  credential on the always-on box, so compromising it cannot push tooling
  changes back upstream. The push credential lives only on the trusted
  laptop and is a crown jewel; protect the GitHub account accordingly.
- **No automated release pipeline, ever.** Release binaries are
  cross-compiled and checksummed **locally on the trusted laptop** and
  uploaded deliberately — never built by CI. A CI pipeline with publish
  rights is exactly the Shai-Hulud attack surface we're defending against;
  we refuse to add one.
- **The bootstrap trust root, honestly.** For `curl daybox.dev/install.sh |
  sh`, the trust root is the domain, its TLS, and the credential that can
  write to the artifact store — the served installer and the artifacts it
  pins live in the *same* store, so whoever holds that write credential can
  rewrite both coherently, signing key or no signing key. That is inherent
  to every curl|bash bootstrap; we don't pretend otherwise. What the pinned
  hash and signature *do* buy: the store cannot serve a different payload
  than the installer you can read attests (swap, truncation, rollback at
  `/dl/latest/`), and `daybox init` refuses signed sums that don't attest
  the version they're served under (rollback at a version path). The store
  write credential is therefore a crown jewel on par with the signing key,
  and is treated as such. Skeptics can bypass the store entirely: read
  install.sh first (it's short), or clone the repo and build.

## What a compromise still gets an attacker

No hand-waving: a malicious process on a live box can read anything your
session can — the agent's auth on the volume, your environment, whatever the
scoped git key can reach — and can talk to the internet freely. Our answer
is **scoping and disposability**, not prevention: the box is
yours and isolated, the credentials on it are narrow and revocable, and the
machine evaporates. If that trade isn't right for your workload, that's a
legitimate reason not to run it — and we'd rather you close this file
knowing exactly what you'd be accepting.

## How secrets get into a box

Every summon has to answer: the box is brand new, disposable, and (per the
threat model above) assumed compromisable — so how does it come to know who
you are, hold your work, and act on your behalf? Everything that enters a box
falls into one of four classes, each with a different right answer. Most
secret-handling mistakes come from using a lower class's mechanism for a
higher class's material.

**1. Provisioning identity — minted, single-use, expiring.** What the box
needs to *become a member* of the net: a headscale pre-auth key. The control
plane mints it per summon — single-use, 15-minute expiry, tagged ephemeral —
and pushes it over the allowlisted ssh path (stdin, never argv). Blast radius
of interception: one join to a personal net, revocable in one command.
**Never reuse provisioning identity across summons.**

**2. Durable workspace state — lives on the volume, never travels.** Repos,
claude auth/session state, git identity, tmux layouts. This never "gets into"
the box at all — the volume gets *attached* and the state is simply there at
`/work`. It never leaves the provider, survives every reap, and no copy
exists on the laptop or in the repo. If it's big, mutable, or account-bound
state (not a credential), it belongs on the volume.

**3. Stored secrets — encrypted at rest, plaintext only in a session's RAM.**
API keys, fine-grained GitHub PATs: things a session needs in its
environment. These are the dangerous ones — a supply-chain worm greps for
exactly this. The design (an `age`-encrypted bundle on the volume, decrypted
to memory at attach behind a passphrase prompt) is the planned upgrade; v1
ships with these plaintext on the volume and documents the risk rather than
half-solving it. Every key you seed should be **scoped and capped** — a
spend-limited API key, a per-repo PAT with expiry — so a stolen session means
"revoke and rotate", never "drain the account".

**4. Delegated credentials — never on the box at all.** The best secret is
one that doesn't exist remotely: **ssh agent forwarding** (`ssh -A` from an
enrolled device) for git push/pull during interactive sessions — the key
stays on the laptop, the box only borrows signatures while you're attached;
**read-only deploy keys** (class-2, on the volume) for anything the box must
clone unattended. Prefer delegation over storage whenever a human is present.

| Input | Class | Mechanism | At rest on cloud? |
|---|---|---|---|
| net pre-auth key | 1 | minted per summon, ssh stdin push | 15min, single-use |
| repos, claude auth, tmux | 2 | persistent volume attach | yes (volume only) |
| API keys, PATs | 3 | on the volume (age bundle planned) | v1: plaintext on volume |
| personal ssh key (git) | 4 | agent forwarding from device | never |
| Hetzner API token | — | never enters any box; control plane only | n/a |

Ingress control and secret handling live on different axes: net-only ingress
protects the *box* from the network; short-lived, scoped credentials protect
your *accounts* from the box. You need both — the second is the only defense
that works *after* an invited-in dependency is already executing.

## Reporting a vulnerability

Email **security@daybox.dev**. There is no public issue tracker — the code
is published as signed source drops, not a forge repo — so email is the
channel. Include the version (`daybox version`) and what you observed;
you'll get a reply from a human, and a fix ships as a new signed release,
because that is the only way any code ships.

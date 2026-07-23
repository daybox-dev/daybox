# keys/

Authorized ssh public keys injected into every summoned box — one
`<device>.pub` per device, registered with the provider by `daybox setup`.

**Deployments keep their keys machine-local** at `~/.config/daybox/keys/`
on the control plane (that dir wins when non-empty; this repo dir is the
fallback and ships empty — pubkeys are public material, but they identify
and locate devices, which is deployment, not tool).

Add a device: drop its `.pub` in `~/.config/daybox/keys/`, run
`daybox setup` once.

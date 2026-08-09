package main

// provider.go — the cloud-provider abstraction, expressed as a Go interface
// instead of the sourced providers/<name>.sh files the bash CLI used. Same
// contract semantics (providers/hetzner.sh documents them), same normalized
// server shape — a new cloud is a new type implementing Provider.
//
// Everything in the summon/reap/status/down path talks ONLY to this
// interface and parses only the Server struct below; no cloud-API
// knowledge leaks above this layer (the property the bash `derive_profile`
// + `load_provider` sourcing model enforced).

// Server is the normalized cloud-server shape every provider emits. Fields
// mirror the bash contract's `_hz_norm` JSON exactly:
//
//	id      opaque string the provider understands (Reap takes it back);
//	        stringified even when the provider's native id is numeric
//	name    the server's name (matches what Probe was asked for)
//	ip      public IPv4 the control plane can ssh
//	status  "running" once the machine is usable
//	created RFC3339 creation time ("" if the provider cannot supply it —
//	        never a guess; box_age parses it)
//	type    provider-native size name (ccx33, …)
type Server struct {
	ID      string
	Name    string
	IP      string
	Status  string
	Created string
	Type    string
}

// Provider is the five-primitive contract (README: Providers) plus the
// credentials/ssh-key/price support primitives every provider needs. A
// provider implementation is constructed with machine-local paths
// (config + state dirs) so it can find its credentials and persist
// per-provider state under state/providers/<name>/ — two providers must
// never share a file.
type Provider interface {
	// Name is the provider's identifier ("hetzner"), matching the PROVIDER
	// knob in config.local / a profile's config.
	Name() string

	// HasCredentials is the quiet boolean the reaper's silent no-op and the
	// role gate consult (no setup help, no fatal — just yes/no).
	HasCredentials() bool

	// CheckCredentials dies with provider-specific setup help when the
	// credentials are missing or unreadable. Returns nil when present.
	CheckCredentials() error

	// PrepareSSHKeys registers every *.pub in dir (if new) and caches the
	// resolved Hetzner-side names — a key may exist under a different name,
	// and summon references keys by name, not filename. Idempotent.
	PrepareSSHKeys(dir string) error

	// Summon creates the box with the volume attached and user_data
	// applied, waits until it is running, and returns the normalized
	// server. Any failure is an error (nothing half-created is blessed).
	Summon(name, serverType, image, location, volumeID, userData string) (Server, error)

	// Reap deletes the box; billing stops now.
	Reap(id string) error

	// Probe returns the named box, or (nil, nil) when it does not exist.
	Probe(name string) (*Server, error)

	// VolumeEnsure creates (pre-formatted ext4, or equivalent) or adopts a
	// volume by name; idempotent. Returns the volume id.
	VolumeEnsure(name string, sizeGB int, location string) (string, error)

	// VolumeAttachedTo returns the server id the volume is attached to, or
	// "" when it is free.
	VolumeAttachedTo(id string) (string, error)

	VolumeDetach(id string) error
	VolumeSize(id string) (int, error)
	VolumeRename(id, newName string) error
	VolumeDelete(id string) error

	// UserDataMaxBytes is the provider's cloud-init user_data size cap.
	UserDataMaxBytes() int

	// PriceHourly returns the gross €/h for a type at a location, or "" when
	// unknown (callers print "?" then). Never hardcode a price — they differ
	// per location and change over time.
	PriceHourly(serverType, location string) string
}

// hetznerUserDataCap is Hetzner's documented user_data ceiling (32 KiB).
// bash: provider_user_data_max_bytes.
const hetznerUserDataCap = 32768

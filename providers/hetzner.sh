# providers/hetzner.sh — the Hetzner Cloud implementation of the provider
# contract. Plain REST (curl+jq), no third-party binaries.
#
# THE PROVIDER CONTRACT (README: Providers). Everything a cloud must do for
# daybox is five primitives; a provider is one file in providers/ that
# implements the functions below, and NOTHING above this layer talks to a
# cloud API. bin/daybox's derive_profile sources providers/$PROVIDER.sh
# (config: PROVIDER, default hetzner) after layering that profile's config,
# so a profile can pick its provider per box — including inside the loops
# over profiles (reap, profile ls).
#
# What a provider file may assume, and nothing else:
#   - it is SOURCED by bin/daybox under `set -euo pipefail`, possibly several
#     times per run and possibly OVER another provider (the loops above switch
#     providers by re-sourcing) — top-level statements must be idempotent, and
#     the file must define the complete function set so nothing of the
#     previous provider survives the switch
#   - helpers `log` and `die` exist; die on any failure a primitive cannot
#     honor (the contract lines below say which)
#   - $CONF_DIR (machine-local config), $STATE_DIR (machine-local state) and
#     $REPO_DIR are set. Anything a provider PERSISTS lives under its own
#     $STATE_DIR/providers/<name>/ — two providers must never share a file.
#     Credential location is the provider's choice (hetzner claims
#     $CONF_DIR/token); provider_check_credentials documents it to the user.
#
# Conformance: scripts/test-provider-conformance.sh runs the full acceptance
# checklist against a real deployment (REAL money — about a box-hour). Run it
# before trusting any new provider file.
#
# Core code parses ONLY this normalized server JSON (probe/summon emit it):
#
#   {id, name, ip, status, created, type}
#     id      opaque string the provider understands (reap takes it back)
#     ip      public IPv4 the control plane can ssh
#     status  "running" once the machine is usable
#     created RFC3339 creation time (box_age_min parses it; a provider that
#             cannot supply it must emit null, never a guess)
#     type    provider-native size name (ccx33, ...)
#
# summon        provider_summon NAME TYPE IMAGE LOCATION VOLUME_ID
#                 user_data arrives on STDIN (never argv). Create the box with
#                 the volume attached and user_data applied, wait until it is
#                 running, emit normalized JSON. Any failure dies.
# reap          provider_reap ID
#                 Delete the box. Billing stops now. Dies on API failure.
# probe         provider_probe NAME
#                 Normalized JSON for the named box, or the literal string
#                 `null` when it does not exist.
# attach-volume provider_volume_ensure NAME SIZE_GB LOCATION -> id
#                 Create (pre-formatted ext4, or provider equivalent) or adopt
#                 by name; idempotent.
#               provider_volume_attached_to ID -> server id, or empty if free
#               provider_volume_detach ID       kick an async detach
#               provider_volume_size ID -> GB
#               provider_volume_rename ID NEW_NAME
#               provider_volume_delete ID       gone for good — caller confirms
# user_data     Delivered at summon (stdin above).
#               provider_user_data_max_bytes -> the provider's size cap.
#
# Credentials + support (provider-specific by nature):
#   provider_has_credentials      quiet boolean (the reaper's silent no-op)
#   provider_check_credentials    die with provider-specific setup help
#   provider_prepare_ssh_keys DIR make DIR/*.pub usable by summon (register,
#                                 cache resolved names — deployment-wide,
#                                 under $STATE_DIR/providers/<name>/)
#   provider_price_hourly TYPE LOCATION -> gross €/h, empty if unknown

HETZNER_API="https://api.hetzner.cloud/v1"
TOKEN_FILE="$CONF_DIR/token"

HZ_STATE="$STATE_DIR/providers/hetzner"
mkdir -p "$HZ_STATE"
# pre-namespacing deployments cached the key names flat in state/ — adopt the
# old file so they don't re-register every key on the next setup
if [ -f "$STATE_DIR/ssh_key_names.json" ] && [ ! -f "$HZ_STATE/ssh_key_names.json" ]; then
    mv "$STATE_DIR/ssh_key_names.json" "$HZ_STATE/ssh_key_names.json"
fi

provider_has_credentials() { [ -f "$TOKEN_FILE" ]; }

provider_check_credentials() {
    [ -f "$TOKEN_FILE" ] || die "no API token at $TOKEN_FILE
  Create one: Hetzner Cloud Console > project > Security > API tokens (read/write),
  then:  install -m 600 /dev/stdin $TOKEN_FILE   (paste token, ctrl-d)"
}

api() { # METHOD PATH [json-body] — dies on API-level errors
    local method=$1 path=$2 body=${3:-} resp
    local args=(-sS --connect-timeout 10 --max-time 60
                -X "$method" -H "Authorization: Bearer $(cat "$TOKEN_FILE")")
    [ -n "$body" ] && args+=(-H "Content-Type: application/json" -d "$body")
    resp=$(curl "${args[@]}" "$HETZNER_API$path") || die "curl failed: $method $path"
    if [ -n "$resp" ] && [ "$(printf '%s' "$resp" | jq -r '.error // empty | .code // empty')" != "" ]; then
        die "API $method $path: $(printf '%s' "$resp" | jq -c '.error')"
    fi
    printf '%s' "$resp"
}

# Hetzner server object -> the contract's normalized shape (id as a string:
# it is opaque to core code, and providers are free to use non-numeric ids)
_hz_norm='{id: (.id|tostring), name, ip: .public_net.ipv4.ip,
           status, created, type: .server_type.name}'

provider_probe() { # NAME -> normalized JSON or `null`
    api GET "/servers?name=$1" | jq ".servers[0] | if . == null then null else $_hz_norm end"
}

provider_summon() { # NAME TYPE IMAGE LOCATION VOLUME_ID ; user_data on stdin
    local name=$1 type=$2 image=$3 location=$4 vid=$5 ud
    ud=$(cat)
    # resolve keys on their own line: a die inside the payload's $(...) only
    # exits that subshell, and the summon would proceed with a garbage body
    local resp id ip keys
    keys=$(_hz_ssh_key_names)
    resp=$(api POST /servers "$(jq -n \
        --arg name "$name" --arg type "$type" --arg image "$image" \
        --arg loc "$location" --argjson keys "$keys" \
        --argjson vid "$vid" --arg ud "$ud" \
        '{name:$name, server_type:$type, image:$image, location:$loc,
          ssh_keys:$keys, volumes:[$vid], automount:false,
          user_data:$ud, labels:{role:"daybox"}}')")
    # api's die above runs inside $(...) and exits only that subshell (bash
    # does not carry errexit into command substitutions): on an API error
    # $resp is empty and execution CONTINUES here. Without this check the
    # summon would announce "server  created, ip " and poll a server that
    # was never created (seen 2026-07-23: dedicated-core quota exceeded).
    id=$(printf '%s' "$resp" | jq -r '.server.id // empty')
    ip=$(printf '%s' "$resp" | jq -r '.server.public_net.ipv4.ip // empty')
    [ -n "$id" ] && [ -n "$ip" ] || die "server create failed (API error above) — nothing was created"
    log "server $id created, ip $ip — waiting for it to run"

    local i status
    for i in $(seq 1 60); do
        status=$(api GET "/servers/$id" | jq -r '.server.status')
        [ "$status" = "running" ] && break
        sleep 2
    done
    [ "$status" = "running" ] || die "server never reached 'running' (status: $status)"
    api GET "/servers/$id" | jq ".server | $_hz_norm"
}

provider_reap() { # ID
    api DELETE "/servers/$1" >/dev/null
}

provider_volume_ensure() { # NAME SIZE_GB LOCATION -> id
    local name=$1 size=$2 location=$3 vid
    vid=$(api GET "/volumes?name=$name" | jq -r '.volumes[0].id // empty')
    if [ -z "$vid" ]; then
        log "creating ${size}GB volume '$name' in $location (pre-formatted ext4)"
        vid=$(api POST /volumes "$(jq -n --arg n "$name" --arg loc "$location" \
                --argjson s "$size" \
                '{name:$n, size:$s, location:$loc, format:"ext4", labels:{role:"daybox"}}')" \
              | jq -r '.volume.id')
    else
        log "volume '$name' already exists (id $vid)"
    fi
    echo "$vid"
}

provider_volume_attached_to() { # ID -> server id or empty
    api GET "/volumes/$1" | jq -r '.volume.server // empty'
}

provider_volume_detach() { # ID
    api POST "/volumes/$1/actions/detach" '{}' >/dev/null
}

provider_volume_size() { # ID -> GB
    api GET "/volumes/$1" | jq -r '.volume.size // "?"'
}

provider_volume_rename() { # ID NEW_NAME
    api PUT "/volumes/$1" "$(jq -n --arg n "$2" '{name:$n}')" >/dev/null
}

provider_volume_delete() { # ID
    api DELETE "/volumes/$1" >/dev/null
}

# Hetzner rejects user_data over 32768 bytes
provider_user_data_max_bytes() { echo 32768; }

provider_price_hourly() { # TYPE LOCATION -> gross €/h
    api GET "/server_types?name=$1" \
      | jq -r --arg loc "$2" \
        '.server_types[0].prices[]? | select(.location==$loc) | .price_hourly.gross' \
      | cut -c1-6
}

provider_prepare_ssh_keys() { # DIR — register pubkeys, cache resolved names
    local dir=$1
    [ -d "$dir" ] || die "put authorized pubkeys in $dir/<name>.pub first"
    # resolve each pubkey to its Hetzner-registered name (registering if new);
    # a key may already exist in the project under a different name
    local f name fp existing names=()
    for f in "$dir"/*.pub; do
        name=$(basename "$f" .pub)
        fp=$(ssh-keygen -lf "$f" -E md5 | awk '{print $2}' | sed 's/^MD5://')
        existing=$(api GET "/ssh_keys?fingerprint=$fp" | jq -r '.ssh_keys[0].name // empty')
        if [ -n "$existing" ]; then
            log "ssh key '$name' already registered as '$existing'"
            names+=("$existing")
        else
            log "registering ssh key '$name'"
            api POST /ssh_keys "$(jq -n --arg n "$name" --rawfile k "$f" \
                '{name:$n, public_key:($k|rtrimstr("\n"))}')" >/dev/null
            names+=("$name")
        fi
    done
    # two key files can resolve to one registered key — dedupe for the API
    printf '%s\n' "${names[@]}" | sort -u | jq -R . | jq -s . > "$HZ_STATE/ssh_key_names.json"
}

_hz_ssh_key_names() { # names resolved at prepare time (may differ from filenames)
    [ -s "$HZ_STATE/ssh_key_names.json" ] || die "no resolved ssh keys — run: daybox setup"
    cat "$HZ_STATE/ssh_key_names.json"
}

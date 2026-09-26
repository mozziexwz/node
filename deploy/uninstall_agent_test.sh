#!/usr/bin/env bash
# Pure fixture tests for the relay-uninstall admission guards. No real
# systemd unit, relay state or root-owned path is changed here.
set -Eeuo pipefail
source "$(dirname -- "${BASH_SOURCE[0]}")/uninstall-agent.sh"
work=$(mktemp -d)
trap '[[ -n ${work:-} && -d $work && $work == /tmp/* ]] && rm -rf -- "$work"' EXIT
state=$work/state
mkdir -m 0700 -- "$state"
printf '%s\n' '{"agentId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","token":"test-only"}' > "$state/relay-token.json"
printf '%s\n' '{"schema":2,"offlinePolicy":"keep_last","agentId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","serverUrl":"https://panel.example.test","records":{}}' > "$state/relay-v2-state.json"
chmod 0600 "$state"/*.json
unit=$work/relay.service
printf '%s\n' \
  '[Unit]' 'Description=MSBOOST relay Agent' \
  '[Service]' 'DynamicUser=true' 'EnvironmentFile=/etc/msboost-relay.env' \
  'ExecStart=/usr/local/bin/msboost-agent --capability relay --state-dir /var/lib/private/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0 --offline-policy keep_last' \
  'StateDirectory=msboost-relay' > "$unit"
relay_uninstall_unit_preflight "$unit" /var/lib/private/msboost-relay "$(id -u)"
relay_uninstall_identity_preflight "$state" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa https://panel.example.test 1
if relay_uninstall_identity_preflight "$state" bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb https://panel.example.test; then echo 'accepted wrong node ID' >&2; exit 1; fi
if relay_uninstall_identity_preflight "$state" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa https://other.example.test; then echo 'accepted wrong panel' >&2; exit 1; fi
printf '%s\n' '{"schema":2,"offlinePolicy":"keep_last","agentId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","serverUrl":"https://panel.example.test","records":{"active":{"state":"ready","command":{"action":"upsert"}}}}' > "$state/relay-v2-state.json"
if relay_uninstall_identity_preflight "$state" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa https://panel.example.test; then echo 'accepted running forwarding' >&2; exit 1; fi
printf '%s\n' '{"schema":2,"offlinePolicy":"keep_last","agentId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","serverUrl":"https://panel.example.test","records":{"retired":{"state":"stopped","command":{"action":"revoke"}}}}' > "$state/relay-v2-state.json"
relay_uninstall_identity_preflight "$state" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa https://panel.example.test 1
mv -- "$state/relay-v2-state.json" "$work/held-state.json"
if relay_uninstall_identity_preflight "$state" aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa https://panel.example.test 1; then echo 'accepted missing keep_last state' >&2; exit 1; fi
mv -- "$work/held-state.json" "$state/relay-v2-state.json"
printf '%s\n' foreign > "$state/unrelated-file"
if relay_uninstall_state_manifest "$state"; then echo 'accepted unrelated state file' >&2; exit 1; fi
rm -f -- "$state/unrelated-file"
ln -s relay-token.json "$state/traffic-journal.json"
if relay_uninstall_state_manifest "$state"; then echo 'accepted state symlink' >&2; exit 1; fi
rm -f -- "$state/traffic-journal.json"
printf '%s\n' 'ExecStart=/bin/foreign' >> "$unit"
if relay_uninstall_unit_preflight "$unit" /var/lib/private/msboost-relay "$(id -u)"; then echo 'accepted second ExecStart' >&2; exit 1; fi
backup=$work/relay.ABCDEFGH
mkdir -m 0700 -- "$backup"
printf '%s\n' backup > "$backup/binary"
relay_uninstall_backup_manifest "$backup" "$(id -u)"
mkdir -- "$backup/user-data"
if relay_uninstall_backup_manifest "$backup" "$(id -u)"; then echo 'accepted foreign backup entry' >&2; exit 1; fi
managed=$work/managed
mkdir -- "$managed"
printf 'MSBOOST_AGENT_MANAGED_V1\n' > "$managed/managed-v1"
printf 'synthetic gost\n' > "$managed/gost-v3.3.0"
relay_uninstall_managed_manifest "$managed" "$(id -u)"
printf '%s\n' unrelated > "$managed/foreign"
if relay_uninstall_managed_manifest "$managed" "$(id -u)"; then echo 'accepted foreign managed asset' >&2; exit 1; fi
printf 'PASS: relay uninstall rejects mismatched identity, live forwarding, links and unrelated files\n'

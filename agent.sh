#!/usr/bin/env bash
# Download release-pinned standalone Agent assets; never put enrollment tokens in URLs.
set -Eeuo pipefail
umask 077
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
version=v0.1.2
capability=''; server=''; token_file=''
usage() {
  printf '%s\n' 'MSBOOST Agent bootstrap (Debian 12 amd64/arm64)' \
    'Usage: bash agent.sh --capability executor|relay --server https://panel.example.com [--version v0.1.2] [--token-file /root/private-token]' \
    'Without --token-file, paste the token at a hidden terminal prompt. HTTPS is required.'
}
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
while (( $# )); do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --capability|--server|--version|--token-file)
      (( $# >= 2 )) || fail "Missing value for $1"
      case "$1" in --capability) capability=$2 ;; --server) server=${2%/} ;; --version) version=$2 ;; --token-file) token_file=$2 ;; esac
      shift 2 ;;
    *) fail "Unknown option $1" ;;
  esac
done
[[ "$capability" == executor || "$capability" == relay ]] || fail 'Choose executor or relay.'
[[ "$server" =~ ^https://[A-Za-z0-9][A-Za-z0-9.-]*(:[0-9]{1,5})?$ ]] || fail 'Use an HTTPS origin, without path or credentials. Public HTTP is not safe for Agent tokens or SSH credentials.'
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail 'Use a fixed stable release version.'
[[ "$(uname -s)" == Linux && "$(id -u)" == 0 ]] || fail 'Run as root on the Linux machine you intend to register.'
for tool in curl sha256sum mktemp; do command -v "$tool" >/dev/null || fail "Install $tool first."; done
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; *) fail 'Only amd64/arm64 are supported.' ;; esac
stage=$(mktemp -d /tmp/msboost-agent-bootstrap.XXXXXXXX)
cleanup() { [[ "$stage" == /tmp/msboost-agent-bootstrap.* && -d "$stage" && ! -L "$stage" ]] && rm -rf -- "$stage"; }
trap cleanup EXIT
base="https://github.com/mozziexwz/node/releases/download/${version}"
for file in SHA256SUMS install-agent.sh "msboost-agent-linux-${arch}"; do
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --retry 3 --max-time 300 "$base/$file" -o "$stage/$file"
done
for file in install-agent.sh "msboost-agent-linux-${arch}"; do
  expected=$(awk -v name="$file" '$2 == name {print $1}' "$stage/SHA256SUMS")
  [[ "$expected" =~ ^[a-f0-9]{64}$ ]] || fail "Missing or duplicate checksum for $file"
  [[ "$(sha256sum "$stage/$file" | cut -d' ' -f1)" == "$expected" ]] || fail "Checksum mismatch: $file"
done
agent_sha=$expected
if [[ -z "$token_file" ]]; then
  [[ -r /dev/tty ]] || fail 'An interactive terminal or --token-file is required.'
  printf 'Paste the one-time registration token (hidden): ' >/dev/tty
  IFS= read -r -s token </dev/tty || fail 'Unable to read token.'
  printf '\n' >/dev/tty
  [[ "$token" =~ ^[A-Za-z0-9_-]{32,256}$ ]] || fail 'Invalid token format.'
  token_file="$stage/token"
  printf '%s' "$token" > "$token_file"
  unset token
fi
bash "$stage/install-agent.sh" --capability "$capability" --server "$server" \
  --agent "$stage/msboost-agent-linux-${arch}" --agent-sha256 "$agent_sha" \
  --token-file "$token_file" --gost-version 3.3.0

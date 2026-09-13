package executor

// Debian commonly mounts /run noexec. Keep executable staging on the same
// protected filesystem as the final immutable cache; never remount /run.
const relayStageScript = `
msboost_phase=install
relay_fail() { printf 'MSBOOST_ERROR_CODE=%s\nMSBOOST_ERROR_PHASE=%s\n' "$1" "$msboost_phase"; exit 1; }
relay_diagnostic() {
  if [ "$msboost_phase" = install ]; then
    printf 'MSBOOST_ERROR_CODE=install_failed\nMSBOOST_ERROR_PHASE=install\n'
  else
    msboost_diagnostic
  fi
}
trap relay_diagnostic ERR
managed=/usr/local/libexec/msboost-free
for path in /usr /usr/local /usr/local/libexec "$managed"; do
  [ ! -L "$path" ] || relay_fail ownership_failed
  if [ -e "$path" ]; then
    [ -d "$path" ] || relay_fail ownership_failed
    [ "$(stat -c %u -- "$path")" = 0 ] || relay_fail ownership_failed
    mode=$(stat -c %a -- "$path")
    (( (8#$mode & 022) == 0 )) || relay_fail ownership_failed
  fi
done
install -d -m 0755 -- "$managed"
work=$(mktemp -d "$managed/.stage.XXXXXXXX")
`

const relayBinaryScript = `
msboost_phase=integrity
printf '%s  %s\n' "$gost_sha" "$work/gost.tar.gz" | sha256sum -c - >/dev/null
msboost_phase=extract
tar -xzf "$work/gost.tar.gz" -C "$work" gost
[ -f "$work/gost" ] && [ ! -L "$work/gost" ] || relay_fail archive_failed
msboost_phase=binary
[ "$(od -An -tx1 -N4 "$work/gost" | tr -d ' \n')" = 7f454c46 ] || relay_fail binary_unusable
case "$(uname -m)" in x86_64) expected_machine=62 ;; aarch64|arm64) expected_machine=183 ;; *) relay_fail unsupported_system ;; esac
[ "$(od -An -tu2 -j18 -N2 "$work/gost" | tr -d ' \n')" = "$expected_machine" ] || relay_fail binary_unusable
binary_sha=$(sha256sum "$work/gost" | cut -d' ' -f1)
binary="$managed/gost-$gost_sha"
verify_cached_binary() {
  [ -f "$binary" ] && [ ! -L "$binary" ] || relay_fail ownership_failed
  [ "$(stat -c %u -- "$binary")" = 0 ] || relay_fail ownership_failed
  mode=$(stat -c %a -- "$binary")
  (( (8#$mode & 022) == 0 )) || relay_fail ownership_failed
  [ "$(sha256sum "$binary" | cut -d' ' -f1)" = "$binary_sha" ] || relay_fail ownership_failed
}
msboost_phase=install
if [ -e "$binary" ] || [ -L "$binary" ]; then
  # Never replace a shared executable while an existing service may use it.
  verify_cached_binary
else
  install -m 0755 -- "$work/gost" "$work/gost-ready"
  # Atomic publication on the same filesystem; no -f, so an existing file,
  # symlink or concurrent publication is never overwritten.
  if ! ln -T -- "$work/gost-ready" "$binary"; then verify_cached_binary; fi
fi
msboost_phase=binary
"$binary" -V >/dev/null
msboost_phase=install
`

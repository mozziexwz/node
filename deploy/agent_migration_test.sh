#!/usr/bin/env bash
# Offline installer contracts: fixture files only, no real systemd/network or
# system directories. Source-safe helpers are the actual installer's decisions.
set -Eeuo pipefail
TEST_REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mkdir -p "$TEST_REPO/.cache"
TEST_WORK=$(mktemp -d "$TEST_REPO/.cache/agent-migration-test.XXXXXXXX")
trap '[[ $TEST_WORK == "$TEST_REPO/.cache/agent-migration-test."* && ! -L $TEST_WORK && $(realpath -m "$TEST_WORK") == "$TEST_WORK" ]] && command rm -rf -- "$TEST_WORK"' EXIT
original_path=$PATH original_flags=$-
source "$TEST_REPO/deploy/install-agent.sh"
[[ $PATH == "$original_path" && $- == "$original_flags" ]] || { printf 'FAIL: source changed shell\n' >&2; exit 1; }
test_fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
reject() { if ("$@") > "$TEST_WORK/rejected.log" 2>&1; then test_fail "accepted $*"; fi; }
TRACE="$TEST_WORK/trace"; : > "$TRACE"
for entrypoint in "$TEST_REPO/agent.sh" "$TEST_REPO/deploy/install-agent.sh"; do
  if command bash -xv "$entrypoint" --capability executor --offline-policy keep_last --token-file /fixture/SYNTHETIC_PRIVATE_TOKEN_PATH > "$TEST_WORK/tracing.log" 2>&1; then test_fail 'invalid role accepted during trace test'; fi
  if grep -Fq SYNTHETIC_PRIVATE_TOKEN_PATH "$TEST_WORK/tracing.log"; then test_fail 'entrypoint left tracing enabled for private arguments'; fi
done
systemctl() { printf '%s\n' "$*" >> "$TRACE"; }
curl() { test_fail 'network invoked outside isolated bootstrap fixture'; }
fixture_binary() {
  printf '#!/usr/bin/env bash\n[[ ${1:-} == --help ]] || exit 45\nprintf "%%s\\n" %q\nexit %s\n' "$2" "${3:-0}" > "$1"
  chmod 755 "$1"
}
fixture_binary "$TEST_WORK/v1" '  -capability string'
fixture_binary "$TEST_WORK/v2" '  -offline-policy string'
fixture_binary "$TEST_WORK/gnu-v2" '  --offline-policy=keep_last'
fixture_binary "$TEST_WORK/unsupported-failure" '  -offline-policy string' 2
fixture_binary "$TEST_WORK/unsupported-prose" 'does not implement -offline-policy'
fixture_binary "$TEST_WORK/unsupported-prefix" '  -offline-policy-unsupported string'

install_agent_parse_args --capability relay --server https://panel.example.test/
[[ $offline_policy == keep_last && $acknowledge_restart == 0 && $server == https://panel.example.test ]] || test_fail 'v0.3 relay default is not keep_last'
install_agent_parse_args --capability relay --offline-policy lease
[[ $offline_policy == lease && $acknowledge_restart == 0 ]] || test_fail 'explicit lease compatibility was lost'
install_agent_parse_args --capability relay --offline-policy keep_last --acknowledge-relay-restart --agent /fixture/agent --gost-version 3.3.0
[[ $offline_policy == keep_last && $acknowledge_restart == 1 && $agent_file == /fixture/agent && $gost_version == 3.3.0 ]] || test_fail 'explicit migration flags lost'
install_agent_parse_args --capability executor
[[ $offline_policy == lease && $acknowledge_restart == 0 ]] || test_fail 'parser retained previous role values'
[[ $(relay_service_state_dir relay keep_last) == /var/lib/private/msboost-relay ]] || test_fail 'keep_last unit would follow the DynamicUser public symlink'
[[ $(relay_service_state_dir relay lease) == /var/lib/msboost-relay && $(relay_service_state_dir executor lease) == /var/lib/msboost-relay ]] || test_fail 'legacy service state path changed'
reject relay_service_state_dir executor keep_last
reject relay_service_state_dir relay unknown
for args in '--capability relay --offline-policy' '--capability relay --offline-policy keep-last' '--capability relay --offline-policy auto' '--capability executor --offline-policy keep_last' '--capability executor --acknowledge-relay-restart' '--capability invalid' '--capability relay --force'; do
  read -r -a words <<< "$args"; reject install_agent_parse_args "${words[@]}"
done
relay_binary_supports_v2 "$TEST_WORK/v2" || test_fail 'Go-style feature flag rejected'
relay_binary_supports_v2 "$TEST_WORK/gnu-v2" || test_fail 'GNU-style feature flag rejected'
for binary in v1 unsupported-failure unsupported-prose unsupported-prefix; do reject relay_binary_supports_v2 "$TEST_WORK/$binary"; done

public_state="$TEST_WORK/state-public" private_state="$TEST_WORK/state-private" evidence_unit="$TEST_WORK/evidence-unit"
relay_v2_evidence() { relay_v2_evidence_at "$public_state" "$private_state" "$evidence_unit"; }
reject relay_v2_evidence
for state in "$public_state" "$private_state"; do
  printf '{damaged-but-v2-evidence' > "$state"
  relay_v2_evidence || test_fail 'corrupt/existing v2 state ignored'
  command rm -- "$state"
done
command ln -s "$TEST_WORK/absent-state" "$public_state" 2>/dev/null || true
if [[ -L $public_state ]]; then relay_v2_evidence || test_fail 'dangling v2 symlink ignored'; command rm -- "$public_state"
else printf 'SKIP: dangling v2 state symlink requires Linux support\n'; fi
printf '# conservative evidence: --offline-policy keep_last\n' > "$evidence_unit"
relay_v2_evidence || test_fail 'configured v2 evidence ignored'
command rm -- "$evidence_unit"

relay_migration_preflight relay lease 1 0 "$TEST_WORK/v1"
relay_migration_preflight executor lease 1 0 "$TEST_WORK/v1"
relay_migration_preflight relay keep_last 0 0 "$TEST_WORK/v2"
relay_migration_preflight relay keep_last 1 1 "$TEST_WORK/v2"
reject relay_migration_preflight relay keep_last 1 0 "$TEST_WORK/v2"
reject relay_migration_preflight relay keep_last 0 0 "$TEST_WORK/v1"
[[ ! -s $TRACE ]] || test_fail 'preflight stopped a service'
# A first runtime v2 write while dependencies were being prepared must change
# the second, immediately-before-mutation decision for BOTH roles.
printf '{}' > "$private_state"
reject relay_migration_preflight relay lease 0 0 "$TEST_WORK/v2"
reject relay_migration_preflight executor lease 0 0 "$TEST_WORK/v1"
relay_migration_preflight executor lease 1 0 "$TEST_WORK/v2"
[[ ! -s $TRACE ]] || test_fail 'global compatibility check stopped a service'

unit_line='ExecStart=/usr/local/bin/msboost-agent --capability relay --state-dir /var/lib/msboost-relay --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0 --offline-policy keep_last'
good_unit="$TEST_WORK/good-unit"; printf '[Service]\n%s\n' "$unit_line" > "$good_unit"
relay_unit_uses_v2 "$good_unit" || test_fail 'generated v2 unit rejected'
good_private_unit="$TEST_WORK/good-private-unit"; printf '[Service]\n%s\n' "${unit_line/\/var\/lib\/msboost-relay/\/var\/lib\/private\/msboost-relay}" > "$good_private_unit"
relay_unit_uses_v2 "$good_private_unit" || test_fail 'private direct-path v2 unit rejected'
for fault in lease comment prefix duplicate-policy duplicate-role duplicate-state duplicate-gost missing-state missing-gost duplicate-exec wrong-section continuation whitespace-exec spaced-exec unknown-executable unknown-arg wrong-state private-state-suffix private-state-traversal crlf; do
  bad_unit="$TEST_WORK/unit-$fault"
  case $fault in
    lease) printf '[Service]\n%s\n' "${unit_line/keep_last/lease}" ;;
    comment) printf '[Service]\n# %s\nExecStart=/usr/local/bin/msboost-agent --capability relay\n' "$unit_line" ;;
    prefix) printf '[Service]\n%sXYZ\n' "$unit_line" ;;
    duplicate-policy) printf '[Service]\n%s --offline-policy lease\n' "$unit_line" ;;
    duplicate-role) printf '[Service]\n%s --capability executor\n' "$unit_line" ;;
    duplicate-state) printf '[Service]\n%s --state-dir /var/lib/msboost-relay\n' "$unit_line" ;;
    duplicate-gost) printf '[Service]\n%s --gost-binary /usr/local/libexec/msboost-agent/gost-v3.3.0\n' "$unit_line" ;;
    missing-state) printf '[Service]\n%s\n' "${unit_line/--state-dir \/var\/lib\/msboost-relay /}" ;;
    missing-gost) printf '[Service]\n%s\n' "${unit_line/--gost-binary \/usr\/local\/libexec\/msboost-agent\/gost-v3.3.0 /}" ;;
    duplicate-exec) printf '[Service]\n%s\n%s\n' "$unit_line" "$unit_line" ;;
    wrong-section) printf '[Unit]\n%s\n' "$unit_line" ;;
    continuation) printf '[Service]\n%s\\\n --offline-policy lease\n' "$unit_line" ;;
    whitespace-exec) printf '[Service]\n%s\n ExecStart=/bin/false\n' "$unit_line" ;;
    spaced-exec) printf '[Service]\n%s\nExecStart =/bin/false\n' "$unit_line" ;;
    unknown-executable) printf '[Service]\n%s\n' "${unit_line/msboost-agent /msboost-agent-v1 }" ;;
    unknown-arg) printf '[Service]\n%s --config override.json\n' "$unit_line" ;;
    wrong-state) printf '[Service]\n%s\n' "${unit_line/\/var\/lib\/msboost-relay/\/tmp\/msboost-relay}" ;;
    private-state-suffix) printf '[Service]\n%s\n' "${unit_line/\/var\/lib\/msboost-relay/\/var\/lib\/private\/msboost-relay-other}" ;;
    private-state-traversal) printf '[Service]\n%s\n' "${unit_line/\/var\/lib\/msboost-relay/\/var\/lib\/private\/..\/msboost-relay}" ;;
    crlf) printf '[Service]\r\n%s\r\n' "$unit_line" ;;
  esac > "$bad_unit"
  reject relay_unit_uses_v2 "$bad_unit"
done

# Run the real EXIT cleanup with only fixture destinations and mocked systemctl.
# Negative paths must preserve new files and must not daemon-reload/start v1.
for scenario in relay-old-binary relay-lease-unit relay-comment-unit executor-old-binary relay-safe-v2 relay-private-safe-v2 executor-safe-v2 legacy-no-v2; do
  case_dir="$TEST_WORK/rollback-$scenario"; mkdir -p "$case_dir/prior"
  backup="$case_dir/prior" binary="$case_dir/current-binary" envfile="$case_dir/current-env" unitfile="$case_dir/current-unit"
  stage="$case_dir/stage" capability=relay unit=msboost-relay.service
  mutation=1 committed=0 was_active=1 previous_enable_state=enabled
  printf new-binary > "$binary"; printf new-environment > "$envfile"; printf new-unit > "$unitfile"
  printf old-environment > "$backup/environment"
  cp "$TEST_WORK/v2" "$backup/binary"; cp "$good_unit" "$backup/unit"
  printf '{}' > "$private_state"; : > "$TRACE"
  case $scenario in
    relay-old-binary|executor-old-binary|legacy-no-v2) cp "$TEST_WORK/v1" "$backup/binary" ;;
    relay-lease-unit) cp "$TEST_WORK/unit-lease" "$backup/unit" ;;
    relay-comment-unit) cp "$TEST_WORK/unit-comment" "$backup/unit" ;;
    relay-private-safe-v2) cp "$good_private_unit" "$backup/unit" ;;
  esac
  [[ $scenario != executor-* ]] || { capability=executor; unit=msboost-executor.service; cp "$TEST_WORK/unit-lease" "$backup/unit"; }
  [[ $scenario != legacy-no-v2 ]] || command rm -- "$private_state"
  if (set +e; (exit 53); install_agent_cleanup) > "$case_dir/log" 2>&1; then test_fail 'failed install cleanup returned success'; else [[ $? == 53 ]] || test_fail 'cleanup lost original status'; fi
  if [[ $scenario == *safe-v2 || $scenario == legacy-no-v2 ]]; then
    cmp "$binary" "$backup/binary" && cmp "$unitfile" "$backup/unit" || test_fail 'compatible rollback files not restored'
    grep -Fxq "start $unit" "$TRACE" || test_fail 'compatible rollback did not restore prior active state'
  else
    [[ $(<"$binary") == new-binary && $(<"$envfile") == new-environment && $(<"$unitfile") == new-unit ]] || test_fail 'v2 failure restored incompatible old files'
    [[ $(wc -l < "$TRACE") == 2 ]] || test_fail 'blocked rollback performed extra lifecycle operations'
    grep -Fxq "stop $unit" "$TRACE" && grep -Fxq "disable $unit" "$TRACE" || test_fail 'failed role not safely stopped'
    if grep -Eq '^(start|restart|daemon-reload|enable) ' "$TRACE"; then test_fail 'v1 service resurrected'; fi
  fi
done

# Simulated inode/flock kernel boundary, still using the real lock acquisition
# helper and actual fixture file descriptors. No /run file is opened or removed.
for fault in none held parent-owner parent-write directory-mode file-hardlink inode-change; do
  (
    lock_dir="$TEST_WORK/lock-$fault"; mkdir "$lock_dir"
    stat() {
      case $2 in
        %u) [[ $fault != parent-owner ]] && printf 0 || printf 1000 ;;
        %a) [[ $fault != parent-write ]] && printf 755 || printf 777 ;;
        %u:%a) [[ $fault != directory-mode ]] && printf 0:700 || printf 0:755 ;;
        %u:%a:%h) [[ $fault != file-hardlink ]] && printf 0:600:1 || printf 0:600:2 ;;
        %d:%i) if [[ $1 == -Lc && $fault == inode-change ]]; then printf 1:3; else printf 1:2; fi ;;
        *) test_fail 'unexpected lock stat' ;;
      esac
    }
    flock() { [[ $# == 2 && $1 == -n && $2 =~ ^[0-9]+$ ]] || test_fail 'unsafe flock'; [[ $fault != held ]]; }
    if [[ $fault == none ]]; then
      agent_install_lock_at "$lock_dir"
      [[ -f $lock_dir/install.lock ]] || test_fail 'lock inode was removed'
      exec {agent_install_lock_fd}>&-
      [[ -f $lock_dir/install.lock ]] || test_fail 'unlock deleted shared inode'
    else reject agent_install_lock_at "$lock_dir"; fi
  ) || test_fail "installation lock case $fault"
done

# Exercise the real bootstrap's argument forwarding with synthetic release
# assets. Its network, platform and staging entrypoints are exported mocks.
bootstrap_assets="$TEST_WORK/assets"; mkdir "$bootstrap_assets"
bootstrap_version=$(sed -nE 's/^version=(v[0-9]+\.[0-9]+\.[0-9]+)$/\1/p' "$TEST_REPO/agent.sh")
[[ -n $bootstrap_version ]] || test_fail 'missing fixed Agent bootstrap version'
export bootstrap_version
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$@" > "$BOOTSTRAP_ARGS"\n' > "$bootstrap_assets/install-agent.sh"
printf synthetic-agent > "$bootstrap_assets/msboost-agent-linux-amd64"
(cd "$bootstrap_assets"; sha256sum install-agent.sh msboost-agent-linux-amd64 | sed 's/ \*/  /' > SHA256SUMS)
for mode in default-relay explicit-lease explicit-keep-last executor invalid-policy invalid-executor; do
  (
    bootstrap_stage="$TEST_WORK/bootstrap-$mode"; BOOTSTRAP_ARGS="$TEST_WORK/bootstrap-args-$mode"
    export bootstrap_assets bootstrap_stage BOOTSTRAP_ARGS
    uname() { [[ $1 != -m ]] && printf Linux || printf x86_64; }
    id() { printf 0; }
    mktemp() { [[ $* == '-d /tmp/msboost-agent-bootstrap.XXXXXXXX' ]] || exit 80; mkdir "$bootstrap_stage"; printf '%s\n' "$bootstrap_stage"; }
    curl() {
      local url='' destination=''
      while (($#)); do case $1 in https://*) url=$1; shift ;; -o) destination=$2; shift 2 ;; *) shift ;; esac; done
      [[ $url == "https://github.com/mozziexwz/node/releases/download/$bootstrap_version/"* && $destination == "$bootstrap_stage/"* ]] || exit 81
      cp "$bootstrap_assets/${url##*/}" "$destination"
    }
    export -f uname id mktemp curl
    args=(--capability relay --server https://panel.example.test --token-file /fixture/private-token)
    case $mode in
      explicit-lease) args+=(--offline-policy lease) ;;
      explicit-keep-last) args+=(--offline-policy keep_last --acknowledge-relay-restart) ;;
      executor) args[1]=executor ;;
      invalid-policy) args+=(--offline-policy automatic) ;;
      invalid-executor) args[1]=executor; args+=(--offline-policy keep_last) ;;
    esac
    if [[ $mode == invalid-* ]]; then
      reject command bash "$TEST_REPO/agent.sh" "${args[@]}"
      [[ ! -d $bootstrap_stage && ! -e $BOOTSTRAP_ARGS ]] || test_fail 'invalid bootstrap reached network/install'
    else
      command bash -xv "$TEST_REPO/agent.sh" "${args[@]}" > "$TEST_WORK/bootstrap-$mode.log" 2>&1 || { command cat "$TEST_WORK/bootstrap-$mode.log"; test_fail 'valid bootstrap failed'; }
      if grep -Fq /fixture/private-token "$TEST_WORK/bootstrap-$mode.log"; then test_fail 'successful bootstrap traced private installer arguments'; fi
      [[ -f $BOOTSTRAP_ARGS ]] || test_fail 'bootstrap did not dispatch verified installer'
      case $mode in
        default-relay)
          grep -Fxq -- --offline-policy "$BOOTSTRAP_ARGS" && grep -Fxq keep_last "$BOOTSTRAP_ARGS" || test_fail 'default relay did not forward keep_last'
          ! grep -Fxq -- --acknowledge-relay-restart "$BOOTSTRAP_ARGS" || test_fail 'new relay incorrectly acknowledged a restart' ;;
        explicit-lease)
          grep -Fxq -- --offline-policy "$BOOTSTRAP_ARGS" && grep -Fxq lease "$BOOTSTRAP_ARGS" || test_fail 'explicit lease compatibility lost by bootstrap' ;;
        explicit-keep-last)
          grep -Fxq -- --offline-policy "$BOOTSTRAP_ARGS" && grep -Fxq keep_last "$BOOTSTRAP_ARGS" && grep -Fxq -- --acknowledge-relay-restart "$BOOTSTRAP_ARGS" || test_fail 'explicit migration flags lost by bootstrap' ;;
        executor)
          ! grep -Fxq -- --offline-policy "$BOOTSTRAP_ARGS" || test_fail 'executor received relay policy' ;;
      esac
    fi
  ) || test_fail "bootstrap case $mode"
done

# These assertions bind pure/mock cases to the production ordering and EXIT.
installer="$TEST_REPO/deploy/install-agent.sh"
lock_line=$(grep -n '^agent_install_lock_at /run/msboost-agent-install$' "$installer" | cut -d: -f1)
owner_line=$(grep -n '^managed=/usr/local/libexec/msboost-agent$' "$installer" | cut -d: -f1)
[[ $lock_line -lt $owner_line ]] || test_fail 'shared lock acquired after ownership checks'
[[ $(grep -c '^relay_migration_preflight "\$capability"' "$installer") == 2 ]] || test_fail 'missing second pre-mutation compatibility check'
verified_copy_line=$(grep -n '^\[\[ "$(sha256sum "\$stage/msboost-agent"' "$installer" | cut -d: -f1)
first_probe_line=$(grep -n '^relay_migration_preflight "\$capability"' "$installer" | head -n 1 | cut -d: -f1)
[[ $verified_copy_line -lt $first_probe_line ]] || test_fail 'candidate executed before private-copy verification'
! grep -q '^relay_migration_preflight .*"\$agent_file"$' "$installer" || test_fail 'untrusted mutable source used for capability execution'
grep -q '^trap install_agent_cleanup EXIT$' "$installer" || test_fail 'real EXIT not bound to tested rollback'
grep -Fq 'relay_policy_arg=" --offline-policy $offline_policy"' "$installer" || test_fail 'relay unit did not pin its selected policy explicitly'
printf '%s\n' 'PASS: Agent migration parser/bootstrap, unsupported feature refusal, global v2 evidence, shared installation lock, strict unit rollback and no v1 resurrection'

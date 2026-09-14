#!/usr/bin/env bash
# Sourced ONLY by the already guarded/labelled real disaster CI test.
# Synthetic PG state and a disposable child; no production service is stopped.
ci_test_backup_activity_readonly() {
  [[ ${GITHUB_ACTIONS:-} == true && ${MSBOOST_CI_DISASTER:-} == 1 && $(id -u) == 0 && $INSTALL_ROOT == /opt/msboost ]] || return 1
  ci_assert_owner || return
  local fixture="$CI_ROOT/activity.test" container operation="activity-$CI_TOKEN" metadata before after report="$CI_ROOT/activity-inspection.json" fingerprint
  printf '%s\n' 'CI_BACKUP_ACTIVITY_STAGE=build-linux-fixture'
  (cd "$CI_REPO" && CGO_ENABLED=0 go test -c -o "$fixture" ./internal/control) || return
  chmod 0755 -- "$fixture" || return
  container=$(compose_live run --detach --no-deps -T --user 10001:10001 --volume "$fixture:/ci-activity.test:ro" \
    --entrypoint /ci-activity.test --env MSBOOST_CI_ACTIVITY_FIXTURE=hold --env "MSBOOST_CI_ACTIVITY_ID=$operation" \
    server -test.run '^TestBackupActivityComposeFixture$' -test.count=1 -test.timeout=6m) || return
  [[ $container =~ ^[a-f0-9]{64}$ && $(docker inspect --format '{{index .Config.Labels "msboost.ci.disaster"}}' "$container") == "$CI_TOKEN" ]] || return 1
  local ready=0
  for ((attempt=0;attempt<60;attempt++)); do
    if docker logs "$container" 2>&1 | grep -qx CI_BACKUP_ACTIVITY_OWNER_READY; then ready=1; break; fi
    [[ $(docker inspect --format '{{.State.Running}}' "$container") == true ]] || { die 'CI backup fixture exited before acquiring its original lock'; return 1; }
    sleep 1
  done
  [[ $ready == 1 ]] || { die 'CI backup fixture did not publish its ready marker'; return 1; }
  metadata=$(docker run --rm --network none --read-only --user 10001:10001 --cap-drop ALL --entrypoint stat \
    --mount type=volume,source=msboost_app_data,target=/proof,readonly "$CI_IMAGE" -c '%d:%i:%u:%g:%a:%s' -- /proof/backup-activity.lock) || return
  [[ $metadata =~ ^[0-9]+:[0-9]+:10001:10001:600:65$ ]] || return 1
  printf 'CI_BACKUP_ACTIVITY_ORIGINAL_LOCK_METADATA=%s\n' "$metadata"
  before=$(compose_live exec -T database psql --username=msboost --dbname=msboost -tAX -c "SELECT md5(payload) FROM control_state WHERE id=1") || return
  # Same original volume, no writes, all capabilities dropped except read/search.
  compose_live run --rm --no-deps -T --user 0:0 --cap-add DAC_READ_SEARCH --volume msboost_app_data:/app/data:ro \
    --entrypoint /usr/local/bin/msboost-restore server backup-activity inspect > "$report" || return
  jq -e --arg id "$operation" '.pending == true and .eligible == false and .operationId == $id and (has("lockIdentity")|not)' "$report" >/dev/null || return
  after=$(compose_live exec -T database psql --username=msboost --dbname=msboost -tAX -c "SELECT md5(payload) FROM control_state WHERE id=1") || return
  [[ $before == "$after" ]] || { die 'CI inspect mutated state while a real process held the lock'; return 1; }
  printf '%s\n' 'CI_BACKUP_ACTIVITY_LIVE_OWNER_REJECTED=1'
  # Kill only the exact temporary holder carrying this invocation's owner label.
  [[ $(docker inspect --format '{{index .Config.Labels "msboost.ci.disaster"}}' "$container") == "$CI_TOKEN" ]] || return 1
  docker kill --signal KILL "$container" >/dev/null || return
  docker wait "$container" >/dev/null || return
  # UID 0 alone must not bypass 0600 when Compose drops ALL capabilities.
  compose_live run --rm --no-deps -T --user 0:0 --volume msboost_app_data:/app/data:ro \
    --entrypoint /usr/local/bin/msboost-restore server backup-activity inspect > "$report" || return
  jq -e '.pending == true and .eligible == false' "$report" >/dev/null || return
  compose_live run --rm --no-deps -T --user 0:0 --cap-add DAC_READ_SEARCH --volume msboost_app_data:/app/data:ro \
    --entrypoint /usr/local/bin/msboost-restore server backup-activity inspect > "$report" || return
  jq -e --arg id "$operation" '.pending == true and .eligible == true and .operationId == $id and (has("lockIdentity")|not)' "$report" >/dev/null || return
  fingerprint=$(jq -er '.fingerprint' "$report") || return
  [[ $fingerprint =~ ^[a-f0-9]{64}$ ]] || return 1
  printf '%s\n' 'CI_BACKUP_ACTIVITY_READONLY_DAC_REQUIRED=1'
  printf '%s\n%s\nRECONCILE_BACKUP %s\n' "$operation" "$fingerprint" "$operation" |
    compose_live run --rm --no-deps -T --user 0:0 --cap-add DAC_READ_SEARCH --volume msboost_app_data:/app/data:ro \
      --entrypoint /usr/local/bin/msboost-restore server backup-activity reconcile > "$CI_ROOT/activity-result.json" || return
  jq -e --arg id "$operation" '.operationId == $id and .status == "interrupted_unknown" and (has("operation")|not)' "$CI_ROOT/activity-result.json" >/dev/null || return
  compose_live run --rm --no-deps -T --user 0:0 --cap-add DAC_READ_SEARCH --volume msboost_app_data:/app/data:ro \
    --entrypoint /usr/local/bin/msboost-restore server backup-activity inspect > "$report" || return
  jq -e '.pending == false and .eligible == false' "$report" >/dev/null || return
  after=$(docker run --rm --network none --read-only --user 10001:10001 --cap-drop ALL --entrypoint stat \
    --mount type=volume,source=msboost_app_data,target=/proof,readonly "$CI_IMAGE" -c '%d:%i:%u:%g:%a:%s' -- /proof/backup-activity.lock) || return
  [[ $metadata == "$after" ]] || { die 'CI recovery replaced or modified original lock metadata'; return 1; }
  [[ $(docker inspect --format '{{index .Config.Labels "msboost.ci.disaster"}}' "$container") == "$CI_TOKEN" ]] || return 1
  docker rm "$container" >/dev/null || return
  printf '%s\n' 'CI_BACKUP_ACTIVITY_PASS: live owner blocked; SIGKILL released same inode; original volume read-only; UID0 requires DAC_READ_SEARCH; PostgreSQL reconciled interrupted_unknown; lock metadata unchanged.'
}

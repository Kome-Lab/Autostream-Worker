
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${FAIL_SYSTEMCTL}' /usr/bin/systemctl && printf '%s\n' mounted > '${SYSTEMCTL_MOUNT_MARKER}' && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/failed-install.out" 2>&1
failed_status=$?
set -e
[[ ${failed_status} -ne 0 ]] || die "daemon-reload failure injection unexpectedly succeeded"
grep -Fx -- "mounted" "${SYSTEMCTL_MOUNT_MARKER}" >/dev/null || \
  die "daemon-reload failure injection did not mount the systemctl wrapper"
grep -Fx -- "daemon-reload" "${SYSTEMCTL_CALL_LOG}" >/dev/null || \
  die "daemon-reload failure injection did not reach the commit boundary"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current ]] || \
  die "failed migration left current activated"
[[ -f ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} ]] || \
  die "failed migration did not restore the legacy binary"
[[ -f ${PUBLIC_ALIAS} && ! -L ${PUBLIC_ALIAS} ]] || \
  die "failed migration did not restore the legacy alias"
grep -Fx -- "${LEGACY_BINARY_CONTENT}" "${PUBLIC_BINARY}" >/dev/null || \
  die "failed migration changed the legacy binary"
grep -Fx -- "${LEGACY_ALIAS_CONTENT}" "${PUBLIC_ALIAS}" >/dev/null || \
  die "failed migration changed the legacy alias"
[[ "$(
  printf '%s|' "$(stat -c '%u:%g:%a' -- "${PUBLIC_BINARY}")"
  sha256sum -- "${PUBLIC_BINARY}" | awk 'NR == 1 { print $1 }'
)" == "${legacy_binary_before}" &&
  "$(
    printf '%s|' "$(stat -c '%u:%g:%a' -- "${PUBLIC_ALIAS}")"
    sha256sum -- "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }'
  )" == "${legacy_alias_before}" &&
  "$(
    printf '%s|' "$(stat -c '%u:%g:%a' -- "${UNIT_PATH}")"
    sha256sum -- "${UNIT_PATH}" | awk 'NR == 1 { print $1 }'
  )" == "${legacy_unit_before}" ]] || \
  die "failed migration did not restore exact live path metadata and content"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "failed migration changed the existing environment"
[[ $(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }') == "${unit_before}" ]] || \
  die "failed migration did not restore the systemd unit"
[[ -d ${STATE_DIR} && ! -L ${STATE_DIR} &&
  $(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}") == "${state_dir_before}" ]] || \
  die "failed migration changed existing state directory metadata"
[[ -f ${STATE_SENTINEL} && ! -L ${STATE_SENTINEL} &&
  $(sha256sum "${STATE_SENTINEL}" | awk 'NR == 1 { print $1 }') == \
    "${state_sentinel_before}" &&
  "$(find "${STATE_DIR}" -mindepth 1 \
    -printf '%P|%y|%u|%g|%m|%s\n' | LC_ALL=C sort)" == "${state_tree_before}" ]] || \
  die "failed migration changed existing state directory content"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "failed migration replaced the running legacy process"
kill -0 "${old_pid}" || die "failed migration stopped the running legacy process"
assert_legacy_runtime_unit "failed migration"
systemctl is-enabled --quiet "${UNIT}" && die "failed migration unexpectedly enabled the service"

for retained in \
  "${RETAINED_DIR}/autostream-worker" \
  "${RETAINED_DIR}/worker" \
  "${RETAINED_DIR}/autostream-worker.service"; do
  [[ -f ${retained} && ! -L ${retained} ]] || \
    die "failed migration did not retain durable retry backup: ${retained}"
done
retained_backups_before="$(
  for retained in \
    "${RETAINED_DIR}/autostream-worker" \
    "${RETAINED_DIR}/worker" \
    "${RETAINED_DIR}/autostream-worker.service"; do
    printf '%s|%s|' \
      "${retained}" \
      "$(stat -c '%d:%i:%s:%Y:%Z:%f:%u:%g:%a' -- "${retained}")"
    sha256sum -- "${retained}" | awk 'NR == 1 { print $1 }'
  done
)"

rm -f -- "${STATE_SENTINEL}"
rmdir -- "${STATE_DIR}"
[[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || \
  die "could not prepare the absent-state rollback fixture"

cat > "${FAIL_SYNC}" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${SYNC_CALL_LOG}"
if [[ \$# -eq 2 && \$1 == "-f" && \$2 == "/etc/systemd/system" ]]; then
  count=0
  if [[ -f "${SYNC_MATCH_COUNT}" ]]; then
    read -r count < "${SYNC_MATCH_COUNT}"
  fi
  count=\$((count + 1))
  printf '%s\n' "\${count}" > "${SYNC_MATCH_COUNT}"
  if [[ \${count} -eq 3 ]]; then
    printf '%s\n' 'production-durability-sync-reached' > "${SYNC_REACHED_MARKER}"
    printf '%s\n' 'injected production durability sync failure' >&2
    exit 95
  fi
fi
exec "${REAL_SYNC_COPY}" "\$@"
EOF
chmod 0755 "${FAIL_SYNC}"
rm -f -- "${SYNC_MATCH_COUNT}" "${SYNC_REACHED_MARKER}"
: > "${SYNC_CALL_LOG}"

set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${FAIL_SYNC}' /usr/bin/sync && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/sync-failure.out" 2>&1
sync_failure_status=$?
set -e
[[ ${sync_failure_status} -eq 1 ]] || \
  die "durability sync failure did not return the installer failure status"
grep -Fx -- "production-durability-sync-reached" "${SYNC_REACHED_MARKER}" >/dev/null || \
  die "sync failure injection did not reach the durability boundary"
grep -Fx -- \
  "install-autostream-worker: failed to sync filesystem parent before commit: /etc/systemd/system" \
  "${WORK_DIR}/sync-failure.out" >/dev/null || \
  die "durability sync failure did not report the exact installer error"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current ]] || \
  die "sync failure did not roll back current"
[[ -f ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} ]] || \
  die "sync failure did not restore the legacy binary"
[[ -f ${PUBLIC_ALIAS} && ! -L ${PUBLIC_ALIAS} ]] || \
  die "sync failure did not restore the legacy alias"
grep -Fx -- "${LEGACY_BINARY_CONTENT}" "${PUBLIC_BINARY}" >/dev/null || \
  die "sync failure changed the legacy binary"
grep -Fx -- "${LEGACY_ALIAS_CONTENT}" "${PUBLIC_ALIAS}" >/dev/null || \
  die "sync failure changed the legacy alias"
[[ "$(
  printf '%s|' "$(stat -c '%u:%g:%a' -- "${PUBLIC_BINARY}")"
  sha256sum -- "${PUBLIC_BINARY}" | awk 'NR == 1 { print $1 }'
)" == "${legacy_binary_before}" &&
  "$(
    printf '%s|' "$(stat -c '%u:%g:%a' -- "${PUBLIC_ALIAS}")"
    sha256sum -- "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }'
  )" == "${legacy_alias_before}" &&
  "$(
    printf '%s|' "$(stat -c '%u:%g:%a' -- "${UNIT_PATH}")"
    sha256sum -- "${UNIT_PATH}" | awk 'NR == 1 { print $1 }'
  )" == "${legacy_unit_before}" ]] || \
  die "sync failure did not restore exact live path metadata and content"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "sync failure changed the existing environment"
[[ $(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }') == "${unit_before}" ]] || \
  die "sync failure did not restore the systemd unit"
[[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || \
  die "sync failure retained a state directory that was absent before installation"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "sync failure replaced the running legacy process"
kill -0 "${old_pid}" || die "sync failure stopped the running legacy process"
assert_legacy_runtime_unit "sync failure"
systemctl is-enabled --quiet "${UNIT}" && die "sync failure enabled the service"
for retained in \
  "${RETAINED_DIR}/autostream-worker" \
  "${RETAINED_DIR}/worker" \
  "${RETAINED_DIR}/autostream-worker.service"; do
  [[ -f ${retained} && ! -L ${retained} ]] || \
    die "sync failure discarded recoverable legacy backup: ${retained}"
done
[[ "$(
  for retained in \
    "${RETAINED_DIR}/autostream-worker" \
    "${RETAINED_DIR}/worker" \
    "${RETAINED_DIR}/autostream-worker.service"; do
    printf '%s|%s|' \
      "${retained}" \
      "$(stat -c '%d:%i:%s:%Y:%Z:%f:%u:%g:%a' -- "${retained}")"
    sha256sum -- "${retained}" | awk 'NR == 1 { print $1 }'
  done
)" == "${retained_backups_before}" ]] || \
  die "sync failure changed a pre-existing durable backup inode, metadata, or content"

chown autostream:autostream "${RETAINED_DIR}/autostream-worker"
tampered_backup_identity="$(
  stat -c '%d:%i:%s:%Y:%Z:%f:%u:%g:%a' -- "${RETAINED_DIR}/autostream-worker"
)"
tampered_backup_digest="$(
  sha256sum -- "${RETAINED_DIR}/autostream-worker" | awk 'NR == 1 { print $1 }'
)"
set +e
"${EXTRACTED_ROOT}/install-autostream-worker" \
  > "${WORK_DIR}/nonroot-backup-rejection.out" 2>&1
nonroot_backup_status=$?
set -e
[[ ${nonroot_backup_status} -ne 0 ]] || \
  die "non-root-owned pre-existing backup fixture unexpectedly succeeded"
grep -F -- \
  "ownership is not root:root" \
  "${WORK_DIR}/nonroot-backup-rejection.out" >/dev/null || \
  die "non-root-owned pre-existing backup rejection did not report the exact conflict"
[[ $(stat -c '%d:%i:%s:%Y:%Z:%f:%u:%g:%a' -- "${RETAINED_DIR}/autostream-worker") == \
    "${tampered_backup_identity}" &&
  $(sha256sum -- "${RETAINED_DIR}/autostream-worker" | awk 'NR == 1 { print $1 }') == \
    "${tampered_backup_digest}" ]] || \
  die "non-root-owned backup rejection changed the conflicting backup"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current &&
  ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || \
  die "non-root-owned backup rejection retained transactional host state"
chown root:root "${RETAINED_DIR}/autostream-worker"

install -d -o autostream -g autostream -m 0700 "${STATE_DIR}"
printf '%s\n' 'worker state rollback sentinel' > "${STATE_SENTINEL}"
chown autostream:autostream "${STATE_SENTINEL}"
chmod 0600 "${STATE_SENTINEL}"
rm -f -- "${PUBLIC_ALIAS}"
sync -f /usr/local/bin

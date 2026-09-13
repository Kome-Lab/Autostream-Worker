
install -o root -g root -m 0755 /usr/bin/systemctl "${REAL_SYSTEMCTL_COPY}"
install -o root -g root -m 0755 /usr/sbin/groupadd "${REAL_GROUPADD_COPY}"
install -o root -g root -m 0755 /usr/sbin/useradd "${REAL_USERADD_COPY}"
install -o root -g root -m 0755 /usr/bin/mktemp "${REAL_MKTEMP_COPY}"
install -o root -g root -m 0755 /usr/bin/sync "${REAL_SYNC_COPY}"
cat > "${FAIL_SYSTEMCTL}" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${SYSTEMCTL_CALL_LOG}"
if [[ \$# -eq 1 && \$1 == "daemon-reload" ]]; then
  exit 97
fi
exec "${REAL_SYSTEMCTL_COPY}" "\$@"
EOF
chmod 0755 "${FAIL_SYSTEMCTL}"

cat > "${SIGNAL_SYSTEMCTL}" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${SYSTEMCTL_CALL_LOG}"
if [[ \$# -eq 1 && \$1 == "daemon-reload" ]]; then
  signal_count=0
  if [[ -f "${SIGNAL_MATCH_COUNT}" ]]; then
    read -r signal_count < "${SIGNAL_MATCH_COUNT}"
  fi
  signal_count=\$((signal_count + 1))
  printf '%s\n' "\${signal_count}" > "${SIGNAL_MATCH_COUNT}"
  printf '%s\n' 'production-daemon-reload-reached' > "${SIGNAL_REACHED_MARKER}"
  if [[ \${signal_count} -le 2 ]]; then
    kill -TERM "\${PPID}"
  fi
fi
exec "${REAL_SYSTEMCTL_COPY}" "\$@"
EOF
chmod 0755 "${SIGNAL_SYSTEMCTL}"

cat > "${SIGNAL_GROUPADD}" <<EOF
#!/bin/bash
"${REAL_GROUPADD_COPY}" "\$@"
command_status=\$?
if [[ \${command_status} -eq 0 && \${!#} == "autostream" ]]; then
  printf '%s\n' 'groupadd-completed' > "${GROUPADD_SIGNAL_MARKER}"
  kill -TERM "\${PPID}"
fi
exit "\${command_status}"
EOF
chmod 0755 "${SIGNAL_GROUPADD}"

cat > "${SIGNAL_USERADD}" <<EOF
#!/bin/bash
"${REAL_USERADD_COPY}" "\$@"
command_status=\$?
if [[ \${command_status} -eq 0 && \${!#} == "autostream" ]]; then
  printf '%s\n' 'useradd-completed' > "${USERADD_SIGNAL_MARKER}"
  kill -TERM "\${PPID}"
fi
exit "\${command_status}"
EOF
chmod 0755 "${SIGNAL_USERADD}"

cat > "${FAIL_MKTEMP}" <<EOF
#!/bin/bash
if [[ \$# -eq 2 && \$1 == "-d" &&
  \$2 == "/var/tmp/autostream-worker-install.XXXXXXXX" ]]; then
  printf '%s\n' 'production-mktemp-reached' > "${MKTEMP_REACHED_MARKER}"
  printf '%s\n' 'injected production mktemp failure' >&2
  exit 96
fi
exec "${REAL_MKTEMP_COPY}" "\$@"
EOF
chmod 0755 "${FAIL_MKTEMP}"

set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${FAIL_MKTEMP}' /usr/bin/mktemp && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/mktemp-failure.out" 2>&1
mktemp_failure_status=$?
set -e
[[ ${mktemp_failure_status} -eq 1 ]] || \
  die "production mktemp failure did not return the installer failure status"
if [[ ! -f ${MKTEMP_REACHED_MARKER} ]] ||
  ! grep -Fx -- "production-mktemp-reached" "${MKTEMP_REACHED_MARKER}" >/dev/null; then
  cat "${WORK_DIR}/mktemp-failure.out" >&2
  die "mktemp failure injection did not reach production mktemp"
fi
grep -Fx -- "install-autostream-worker: failed to create input staging directory" \
  "${WORK_DIR}/mktemp-failure.out" >/dev/null || \
  die "production mktemp failure did not report the exact installer error"
for path in \
  "${UNIT_PATH}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${STATE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${SHARED_HOST_SETUP_LOCK}" \
  "${TARGET_LOCK}"; do
  [[ ! -e ${path} && ! -L ${path} ]] || \
    die "mktemp failure mutated the host: ${path}"
done
[[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
  -name 'autostream-worker-install.*' -print -quit) ]] || \
  die "mktemp failure mutated the host by retaining production staging"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "mktemp failure mutated the host by creating the service account"
fi

groupadd --non-unique --gid 0 autostream
set +e
"${EXTRACTED_ROOT}/install-autostream-worker" \
  > "${WORK_DIR}/gid-zero-preflight.out" 2>&1
gid_zero_status=$?
set -e
[[ ${gid_zero_status} -ne 0 ]] || \
  die "GID 0 service-group fixture unexpectedly succeeded"
grep -Fx -- \
  "install-autostream-worker: autostream service group must have a non-root numeric GID" \
  "${WORK_DIR}/gid-zero-preflight.out" >/dev/null || \
  die "GID 0 service-group rejection did not report the exact error"
[[ $(getent group autostream | awk -F: 'NR == 1 { print $3 }') == "0" ]] || \
  die "GID 0 service-group rejection changed the pre-existing group"
id autostream >/dev/null 2>&1 && \
  die "GID 0 service-group rejection created the autostream service account"
for path in \
  "${UNIT_PATH}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${STATE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${SHARED_HOST_SETUP_LOCK}" \
  "${TARGET_LOCK}"; do
  [[ ! -e ${path} && ! -L ${path} ]] || \
    die "GID 0 service-group rejection mutated the host: ${path}"
done
groupdel --force autostream

assert_fresh_account_signal_rollback() {
  local scenario=$1
  local status=$2
  local marker_path=$3
  local marker_content=$4
  local path

  [[ ${status} -eq 143 ]] || \
    die "${scenario} TERM rollback did not return status 143"
  grep -Fx -- "${marker_content}" "${marker_path}" >/dev/null || \
    die "${scenario} TERM rollback did not reach the account mutation boundary"
  if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
    die "${scenario} TERM rollback retained the installer-created account"
  fi
  for path in \
    "${UNIT_PATH}" \
    "${PUBLIC_BINARY}" \
    "${PUBLIC_ALIAS}" \
    "${ENV_PATH}" \
    "${STATE_DIR}" \
    "${MANAGED_ROOT}" \
    "${INSTALL_BACKUP_ROOT}" \
    /opt/autostream \
    /etc/autostream \
    /var/lib/autostream \
    /var/backups/autostream; do
    [[ ! -e ${path} && ! -L ${path} ]] || \
      die "${scenario} TERM rollback retained a transactional path: ${path}"
  done
  [[ -d /run/autostream-updater &&
    ! -L /run/autostream-updater &&
    $(stat -c '%U:%G:%a' -- /run/autostream-updater) == "root:root:700" &&
    -f ${SHARED_HOST_SETUP_LOCK} &&
    ! -L ${SHARED_HOST_SETUP_LOCK} &&
    $(stat -c '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK}") == "root:root:600" &&
    -f ${TARGET_LOCK} &&
    ! -L ${TARGET_LOCK} &&
    $(stat -c '%U:%G:%a' -- "${TARGET_LOCK}") == "root:root:600" &&
    $(find /run/autostream-updater -mindepth 1 -maxdepth 1 | wc -l) -eq 2 ]] || \
    die "${scenario} TERM rollback did not retain only permanent safe lock state"
  [[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
    -name 'autostream-worker-install.*' -print -quit) ]] || \
    die "${scenario} TERM rollback retained production staging"
}

rm -f -- "${GROUPADD_SIGNAL_MARKER}"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${SIGNAL_GROUPADD}' /usr/sbin/groupadd && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/groupadd-term-rollback.out" 2>&1
groupadd_term_status=$?
set -e
assert_fresh_account_signal_rollback \
  "groupadd" \
  "${groupadd_term_status}" \
  "${GROUPADD_SIGNAL_MARKER}" \
  "groupadd-completed"
account_signal_locks_before="$(
  printf 'shared|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)"

rm -f -- "${USERADD_SIGNAL_MARKER}"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${SIGNAL_USERADD}' /usr/sbin/useradd && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/useradd-term-rollback.out" 2>&1
useradd_term_status=$?
set -e
assert_fresh_account_signal_rollback \
  "useradd" \
  "${useradd_term_status}" \
  "${USERADD_SIGNAL_MARKER}" \
  "useradd-completed"
[[ "$(
  printf 'shared|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${account_signal_locks_before}" ]] || \
  die "useradd TERM rollback replaced or truncated a permanent lock"

groupadd --system autostream
preexisting_group_record_before="$(getent group autostream)"
preexisting_group_database_digest_before="$(
  sha256sum -- /etc/group | awk 'NR == 1 { print $1 }'
)"
preexisting_gshadow_database_digest_before="$(
  sha256sum -- /etc/gshadow | awk 'NR == 1 { print $1 }'
)"
[[ -n ${preexisting_group_record_before} &&
  ${preexisting_group_database_digest_before} =~ ^[0-9a-f]{64}$ &&
  ${preexisting_gshadow_database_digest_before} =~ ^[0-9a-f]{64}$ ]] || \
  die "could not snapshot the pre-existing autostream group fixture"
id autostream >/dev/null 2>&1 && \
  die "pre-existing autostream group fixture unexpectedly has a user"
rm -f -- "${USERADD_SIGNAL_MARKER}"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${SIGNAL_USERADD}' /usr/sbin/useradd && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/preexisting-group-useradd-term-rollback.out" 2>&1
preexisting_group_useradd_term_status=$?
set -e
[[ ${preexisting_group_useradd_term_status} -eq 143 ]] || \
  die "pre-existing group useradd TERM transaction exited with ${preexisting_group_useradd_term_status}, expected 143"
grep -Fx -- "useradd-completed" "${USERADD_SIGNAL_MARKER}" >/dev/null || \
  die "pre-existing group useradd TERM transaction did not reach useradd"
id autostream >/dev/null 2>&1 && \
  die "pre-existing group useradd TERM transaction retained the installer-created user"
[[ $(getent group autostream 2>/dev/null || true) == "${preexisting_group_record_before}" ]] || \
  die "pre-existing group useradd TERM transaction changed the autostream group"
[[ $(sha256sum -- /etc/group | awk 'NR == 1 { print $1 }') == \
    "${preexisting_group_database_digest_before}" &&
  $(sha256sum -- /etc/gshadow | awk 'NR == 1 { print $1 }') == \
    "${preexisting_gshadow_database_digest_before}" ]] || \
  die "pre-existing group useradd TERM transaction changed the local group databases"
if getent passwd autostream-install-rollback >/dev/null 2>&1 ||
  getent group autostream-install-rollback >/dev/null 2>&1; then
  die "pre-existing group useradd TERM transaction retained the reserved rollback account name"
fi
groupdel autostream

rm -f -- "${SYSTEMCTL_CALL_LOG}"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${FAIL_SYSTEMCTL}' /usr/bin/systemctl && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/fresh-daemon-reload-failure.out" 2>&1
fresh_failure_status=$?
set -e
[[ ${fresh_failure_status} -ne 0 ]] || \
  die "fresh daemon-reload rollback fixture unexpectedly succeeded"
grep -Fx -- "daemon-reload" "${SYSTEMCTL_CALL_LOG}" >/dev/null || \
  die "fresh daemon-reload rollback fixture did not reach the commit boundary"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "fresh daemon-reload rollback retained the installer-created account"
fi
for path in \
  "${UNIT_PATH}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${STATE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  /opt/autostream \
  /etc/autostream \
  /var/lib/autostream \
  /var/backups/autostream; do
  [[ ! -e ${path} && ! -L ${path} ]] || \
    die "fresh daemon-reload rollback retained a transactional path: ${path}"
done
[[ -d /run/autostream-updater &&
  ! -L /run/autostream-updater &&
  $(stat -c '%U:%G:%a' -- /run/autostream-updater) == "root:root:700" &&
  -f ${SHARED_HOST_SETUP_LOCK} &&
  ! -L ${SHARED_HOST_SETUP_LOCK} &&
  $(stat -c '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK}") == "root:root:600" &&
  -f ${TARGET_LOCK} &&
  ! -L ${TARGET_LOCK} &&
  $(stat -c '%U:%G:%a' -- "${TARGET_LOCK}") == "root:root:600" ]] || \
  die "fresh daemon-reload rollback did not retain only the permanent safe lock state"
[[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
  -name 'autostream-worker-install.*' -print -quit) ]] || \
  die "fresh daemon-reload rollback retained production staging"
printf '%s\n' 'worker permanent lock sentinel' > "${TARGET_LOCK}"
chmod 0600 "${TARGET_LOCK}"
printf '%s\n' 'worker shared host-setup lock sentinel' > "${SHARED_HOST_SETUP_LOCK}"
chmod 0600 "${SHARED_HOST_SETUP_LOCK}"
permanent_lock_before="$(
  printf 'shared|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)"

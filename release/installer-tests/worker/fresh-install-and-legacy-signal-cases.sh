
printf '%s\n' 'stale archive checksum sidecar must be ignored' > "${ARCHIVE}.sha256"
printf '%s\n' '{"stale_external_manifest":true}' \
  > "${ARTIFACTS_DIR}/release-manifest.json"
printf '%s\n' 'stale manifest checksum sidecar must be ignored' \
  > "${ARTIFACTS_DIR}/release-manifest.json.sha256"
"${EXTRACTED_ROOT}/install-autostream-worker" > "${WORK_DIR}/fresh.out"
[[ "$(
  printf 'shared|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${permanent_lock_before}" ]] || \
  die "successful installation replaced or truncated the permanent updater lock"
grep -Fx -- 'stale archive checksum sidecar must be ignored' \
  "${ARCHIVE}.sha256" >/dev/null || \
  die "fresh install changed the ignored archive checksum sidecar"
grep -Fx -- '{"stale_external_manifest":true}' \
  "${ARTIFACTS_DIR}/release-manifest.json" >/dev/null || \
  die "fresh install changed the ignored external release manifest"
grep -Fx -- 'stale manifest checksum sidecar must be ignored' \
  "${ARTIFACTS_DIR}/release-manifest.json.sha256" >/dev/null || \
  die "fresh install changed the ignored manifest checksum sidecar"
[[ -L ${MANAGED_ROOT}/current ]] || die "fresh install did not create the managed current link"
[[ -L ${PUBLIC_BINARY} && -L ${PUBLIC_ALIAS} ]] || \
  die "fresh install did not install stable public links"
[[ -f ${ENV_PATH} && ! -L ${ENV_PATH} ]] || die "fresh install did not seed the environment"
[[ $(stat -c '%U:%G:%a' -- "${ENV_PATH}") == "root:root:640" ]] || \
  die "fresh environment ownership or mode is invalid"
[[ $(stat -c '%U:%G:%a' -- "${STATE_DIR}") == "autostream:autostream:750" ]] || \
  die "fresh state ownership or mode is invalid"
id autostream >/dev/null 2>&1 || die "fresh installer did not create the autostream account"
systemctl is-active --quiet "${UNIT}" && die "fresh installer unexpectedly started the service"
systemctl is-enabled --quiet "${UNIT}" && die "fresh installer unexpectedly enabled the service"
grep -F -- "sudo systemctl enable --now ${UNIT}" "${WORK_DIR}/fresh.out" >/dev/null || \
  die "fresh install did not print the explicit start command"

rm -f -- "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}" "${ENV_PATH}" "${UNIT_PATH}"
rm -rf -- "${STATE_DIR}" "${MANAGED_ROOT}" "${INSTALL_BACKUP_ROOT}"
systemctl daemon-reload
rmdir \
  /var/backups/autostream/install-migrations \
  /var/backups/autostream \
  /var/lib/autostream \
  /opt/autostream \
  /etc/autostream >/dev/null 2>&1 || \
  die "fresh-install reset left an unexpected directory"
userdel autostream
if getent group autostream >/dev/null 2>&1; then
  groupdel autostream
fi
[[ ! -e ${MANAGED_ROOT} && ! -L ${MANAGED_ROOT} ]] || \
  die "fresh-install reset retained the managed root"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "fresh-install reset retained the autostream account"
fi

groupadd --system autostream
useradd --system --gid autostream --home-dir /var/lib/autostream \
  --no-create-home --shell /usr/sbin/nologin autostream
install -d -o root -g root -m 0755 /etc/autostream /var/lib/autostream
install -d -o autostream -g autostream -m 0700 "${STATE_DIR}"
printf '%s\n' 'worker state rollback sentinel' > "${STATE_SENTINEL}"
chown autostream:autostream "${STATE_SENTINEL}"
chmod 0600 "${STATE_SENTINEL}"
printf '%s\n' "${LEGACY_BINARY_CONTENT}" > "${PUBLIC_BINARY}"
chmod 0755 "${PUBLIC_BINARY}"
printf '%s\n' "${LEGACY_ALIAS_CONTENT}" > "${PUBLIC_ALIAS}"
chmod 0755 "${PUBLIC_ALIAS}"
printf '%s\n' "${LEGACY_ENV_CONTENT}" > "${ENV_PATH}"
chmod 0640 "${ENV_PATH}"
cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=${LEGACY_UNIT_CONTENT}

[Service]
Type=simple
ExecStart=/usr/bin/sleep infinity

[Install]
WantedBy=multi-user.target
EOF
chmod 0644 "${UNIT_PATH}"
create_runtime_unit_no_clobber "${UNIT_PATH}"
systemctl daemon-reload
fixture_owns_service=true
systemctl start "${UNIT}"
old_pid="$(systemctl show --property MainPID --value "${UNIT}")"
[[ ${old_pid} =~ ^[1-9][0-9]*$ ]] || die "legacy service did not start"
if ! old_pid_starttime="$(read_process_starttime "${old_pid}")"; then
  die "could not record the legacy service process identity"
fi
kill -0 "${old_pid}" || die "legacy service PID is not alive"
assert_loaded_runtime_unit "/usr/bin/sleep" "" "legacy startup"
legacy_unit_file_state="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"
[[ ${legacy_unit_file_state} == "disabled" ]] || \
  die "legacy fixture must begin disabled, got ${legacy_unit_file_state:-unknown}"

env_before="$(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }')"
unit_before="$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
runtime_unit_before="$(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
legacy_binary_before="$(
  printf '%s|' "$(stat -c '%u:%g:%a' -- "${PUBLIC_BINARY}")"
  sha256sum -- "${PUBLIC_BINARY}" | awk 'NR == 1 { print $1 }'
)"
legacy_alias_before="$(
  printf '%s|' "$(stat -c '%u:%g:%a' -- "${PUBLIC_ALIAS}")"
  sha256sum -- "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }'
)"
legacy_unit_before="$(
  printf '%s|' "$(stat -c '%u:%g:%a' -- "${UNIT_PATH}")"
  sha256sum -- "${UNIT_PATH}" | awk 'NR == 1 { print $1 }'
)"
state_dir_before="$(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}")"
state_tree_before="$(
  find "${STATE_DIR}" -mindepth 1 \
    -printf '%P|%y|%u|%g|%m|%s\n' |
    LC_ALL=C sort
)"
state_sentinel_before="$(sha256sum "${STATE_SENTINEL}" | awk 'NR == 1 { print $1 }')"
shared_contention_lock_before="$(
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
)"

(
  exec 7<>"${SHARED_HOST_SETUP_LOCK}"
  flock -n 7
  set +e
  "${EXTRACTED_ROOT}/install-autostream-worker" \
    > "${WORK_DIR}/shared-lock-contention.out" 2>&1
  printf '%s\n' "$?" > "${WORK_DIR}/shared-lock-contention.status"
)
shared_contention_status="$(< "${WORK_DIR}/shared-lock-contention.status")"
[[ ${shared_contention_status} -eq 1 ]] || \
  die "installer ignored shared host-setup lock contention"
grep -Fx -- \
  "install-autostream-worker: another AutoStream installer is provisioning shared host state" \
  "${WORK_DIR}/shared-lock-contention.out" >/dev/null || \
  die "shared host-setup lock contention did not report the exact installer error"
[[ "$(
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${shared_contention_lock_before}" ]] || \
  die "shared host-setup contention replaced or truncated the permanent lock"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current &&
  ! -e ${INSTALL_BACKUP_ROOT} && ! -L ${INSTALL_BACKUP_ROOT} ]] || \
  die "shared host-setup lock contention mutated transactional host state"

printf '%s\n' 'worker contention lock sentinel' > "${TARGET_LOCK}"
chmod 0600 "${TARGET_LOCK}"
contention_lock_before="$(
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)"

(
  exec 8<>"${TARGET_LOCK}"
  flock -n 8
  set +e
  "${EXTRACTED_ROOT}/install-autostream-worker" \
    > "${WORK_DIR}/lock-contention.out" 2>&1
  printf '%s\n' "$?" > "${WORK_DIR}/lock-contention.status"
)
contention_status="$(< "${WORK_DIR}/lock-contention.status")"
[[ ${contention_status} -eq 1 ]] || die "installer ignored updater lock contention"
[[ "$(
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${contention_lock_before}" ]] || \
  die "lock contention replaced or truncated the permanent updater lock"
grep -Fx -- \
  "install-autostream-worker: another privileged update is already active for ${UNIT}" \
  "${WORK_DIR}/lock-contention.out" >/dev/null || \
  die "lock contention did not report the exact installer error"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current ]] || \
  die "lock contention mutated the host by activating current"
[[ -f ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} &&
  $(sha256sum "${PUBLIC_BINARY}" | awk 'NR == 1 { print $1 }') == \
    "$(printf '%s\n' "${LEGACY_BINARY_CONTENT}" | sha256sum | awk 'NR == 1 { print $1 }')" ]] || \
  die "lock contention mutated the host legacy binary"
[[ -f ${PUBLIC_ALIAS} && ! -L ${PUBLIC_ALIAS} &&
  $(sha256sum "${PUBLIC_ALIAS}" | awk 'NR == 1 { print $1 }') == \
    "$(printf '%s\n' "${LEGACY_ALIAS_CONTENT}" | sha256sum | awk 'NR == 1 { print $1 }')" ]] || \
  die "lock contention mutated the host legacy alias"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" &&
  $(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }') == "${unit_before}" ]] || \
  die "lock contention mutated the host configuration"
[[ ! -e ${INSTALL_BACKUP_ROOT} && ! -L ${INSTALL_BACKUP_ROOT} ]] || \
  die "lock contention mutated the host backup state"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "lock contention mutated the host running process"
assert_legacy_runtime_unit "lock contention"
systemctl is-enabled --quiet "${UNIT}" && die "lock contention enabled the service"

signal_locks_before="$(
  printf 'shared|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)"
rm -f -- "${SIGNAL_REACHED_MARKER}" "${SIGNAL_MATCH_COUNT}" "${SYSTEMCTL_CALL_LOG}"
set +e
unshare --mount --propagation private bash -c \
  "mount --bind '${SIGNAL_SYSTEMCTL}' /usr/bin/systemctl && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/term-signal-rollback.out" 2>&1
term_signal_status=$?
set -e
[[ ${term_signal_status} -eq 143 ]] || \
  die "TERM signal rollback did not return status 143"
grep -Fx -- "production-daemon-reload-reached" "${SIGNAL_REACHED_MARKER}" >/dev/null || \
  die "TERM signal rollback did not reach the commit boundary"
[[ -f ${SIGNAL_MATCH_COUNT} && $(< "${SIGNAL_MATCH_COUNT}") == "2" ]] || \
  die "TERM signal rollback cleanup did not survive a repeated TERM"
[[ ! -e ${MANAGED_ROOT}/current && ! -L ${MANAGED_ROOT}/current &&
  ! -e ${MANAGED_ROOT} && ! -L ${MANAGED_ROOT} ]] || \
  die "TERM signal rollback retained a transactional managed path"
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
  )" == "${legacy_unit_before}" &&
  $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "TERM signal rollback did not restore exact live path metadata and content"
[[ -d ${STATE_DIR} && ! -L ${STATE_DIR} &&
  $(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}") == "${state_dir_before}" &&
  -f ${STATE_SENTINEL} && ! -L ${STATE_SENTINEL} &&
  $(sha256sum "${STATE_SENTINEL}" | awk 'NR == 1 { print $1 }') == \
    "${state_sentinel_before}" &&
  "$(find "${STATE_DIR}" -mindepth 1 \
    -printf '%P|%y|%u|%g|%m|%s\n' | LC_ALL=C sort)" == "${state_tree_before}" ]] || \
  die "TERM signal rollback changed existing state"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "TERM signal rollback changed the running legacy process"
kill -0 "${old_pid}" || die "TERM signal rollback stopped the running legacy process"
assert_legacy_runtime_unit "TERM signal rollback"
systemctl is-enabled --quiet "${UNIT}" && die "TERM signal rollback enabled the service"
[[ "$(
  printf 'shared|%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${SHARED_HOST_SETUP_LOCK}")"
  sha256sum -- "${SHARED_HOST_SETUP_LOCK}" | awk 'NR == 1 { print $1 }'
  printf '%s|' "$(stat -c '%d:%i:%u:%g:%a' -- "${TARGET_LOCK}")"
  sha256sum -- "${TARGET_LOCK}" | awk 'NR == 1 { print $1 }'
)" == "${signal_locks_before}" ]] || \
  die "TERM signal rollback replaced or truncated a permanent lock"
[[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
  -name 'autostream-worker-install.*' -print -quit) ]] || \
  die "TERM signal rollback retained production staging"

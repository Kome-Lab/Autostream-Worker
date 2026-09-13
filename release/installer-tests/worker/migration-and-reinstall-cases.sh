
cat > "${LOG_SYNC}" <<EOF
#!/bin/bash
printf '%s\n' "\$*" >> "${SYNC_CALL_LOG}"
exec "${REAL_SYNC_COPY}" "\$@"
EOF
chmod 0755 "${LOG_SYNC}"
: > "${SYNC_CALL_LOG}"
unshare --mount --propagation private bash -c \
  "mount --bind '${LOG_SYNC}' /usr/bin/sync && '${EXTRACTED_ROOT}/install-autostream-worker'" \
  > "${WORK_DIR}/migration.out" 2>&1
replace_owned_runtime_unit_atomically "${UNIT_PATH}"
systemctl daemon-reload
assert_loaded_runtime_unit "${PUBLIC_BINARY}" "autostream" "successful migration"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')" ]] || \
  die "successful migration did not synchronize the managed runtime unit"
grep -F -- \
  "install-autostream-worker: resuming interrupted public-path migration from verified backup: ${RETAINED_DIR}/worker" \
  "${WORK_DIR}/migration.out" >/dev/null || \
  die "interrupted backup retry did not reuse the verified legacy backup"
for synced_parent in \
  /usr/local/bin \
  /etc/autostream \
  /etc/systemd/system \
  "${MANAGED_ROOT}" \
  "${MANAGED_ROOT}/releases" \
  "${RETAINED_DIR}" \
  /run/autostream-updater; do
  grep -Fx -- "-f ${synced_parent}" "${SYNC_CALL_LOG}" >/dev/null || \
    die "successful migration did not sync mutated filesystem parent: ${synced_parent}"
done

[[ -L ${MANAGED_ROOT}/current ]] || die "successful migration did not activate current"
[[ -L ${PUBLIC_BINARY} && -L ${PUBLIC_ALIAS} ]] || \
  die "successful migration did not install stable public links"
[[ $(readlink -f -- "${PUBLIC_BINARY}") == \
  "${MANAGED_ROOT}/releases/${VERSION}-${archive_sha256:0:12}/bin/autostream-worker" ]] || \
  die "public binary does not resolve to the verified release"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "successful migration changed the existing environment"
grep -Fx -- "${LEGACY_BINARY_CONTENT}" \
  "${RETAINED_DIR}/autostream-worker" >/dev/null || \
  die "successful migration did not retain the legacy binary"
grep -Fx -- "${LEGACY_ALIAS_CONTENT}" \
  "${RETAINED_DIR}/worker" >/dev/null || \
  die "successful migration did not retain the legacy alias"
grep -F -- "${LEGACY_UNIT_CONTENT}" \
  "${RETAINED_DIR}/autostream-worker.service" >/dev/null || \
  die "successful migration did not retain the legacy systemd unit"
[[ $(stat -c '%U:%G:%a' -- "${STATE_DIR}") == "autostream:autostream:750" ]] || \
  die "successful migration changed the service state ownership contract"
grep -F -- "sudo systemctl restart ${UNIT}" "${WORK_DIR}/migration.out" >/dev/null || \
  die "active migration did not print the explicit restart command"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "successful migration replaced the running legacy process"
kill -0 "${old_pid}" || die "successful migration stopped the running legacy process"
systemctl is-enabled --quiet "${UNIT}" && die "successful migration unexpectedly enabled the service"

"${EXTRACTED_ROOT}/install-autostream-worker" > "${WORK_DIR}/idempotent.out"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "idempotent reinstall replaced the running legacy process"
assert_loaded_runtime_unit "${PUBLIC_BINARY}" "autostream" "idempotent reinstall"
[[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
  "$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')" ]] || \
  die "idempotent reinstall changed the loaded runtime unit"
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "idempotent reinstall changed the existing environment"
systemctl is-enabled --quiet "${UNIT}" && die "idempotent reinstall unexpectedly enabled the service"

printf '%s\n' "Worker installer integration scenarios passed."

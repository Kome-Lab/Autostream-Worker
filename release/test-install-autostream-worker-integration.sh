#!/bin/bash
set -euo pipefail

umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C

die() {
  printf 'worker installer integration test: %s\n' "$*" >&2
  exit 1
}

[[ ${EUID} -eq 0 ]] || die "must run as root"
[[ $(uname -m) == "x86_64" ]] || die "this integration fixture requires an amd64 Linux runner"

if [[ ${AUTOSTREAM_WORKER_INSTALLER_TEST_MOUNT_NS:-} != "1" ]]; then
  exec unshare --mount --propagation private bash -c '
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-worker-installer-test /usr/local/bin
    exec env AUTOSTREAM_WORKER_INSTALLER_TEST_MOUNT_NS=1 bash "$1"
  ' autostream-worker-installer-test-mount "$0"
fi
grep -Eq ' /usr/local/bin .* - tmpfs autostream-worker-installer-test ' \
  /proc/self/mountinfo || die "isolated /usr/local/bin mount is missing"
[[ $(stat -c '%U:%G:%a' -- /usr/local/bin) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr/local/bin fixture"

readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
readonly INSTALLER_SOURCE="${SCRIPT_DIR}/install-autostream-worker"
readonly VERSION="v9.9.9"
readonly ARTIFACT_ID="autostream-worker_${VERSION}_linux_amd64"
work_dir=""
if ! work_dir="$(mktemp -d /var/tmp/autostream-worker-installer-test.XXXXXXXX)"; then
  die "could not create integration work directory"
fi
[[ -n ${work_dir} && -d ${work_dir} && ! -L ${work_dir} ]] || \
  die "integration work directory is unsafe"
readonly WORK_DIR="${work_dir}"
readonly ARTIFACTS_DIR="${WORK_DIR}/artifacts"
readonly EXTRACTED_ROOT="${ARTIFACTS_DIR}/${ARTIFACT_ID}"
readonly ARCHIVE="${ARTIFACTS_DIR}/${ARTIFACT_ID}.tar.gz"
readonly REAL_SYSTEMCTL_COPY="${WORK_DIR}/systemctl.real"
readonly REAL_MKTEMP_COPY="${WORK_DIR}/mktemp.real"
readonly REAL_SYNC_COPY="${WORK_DIR}/sync.real"
readonly FAIL_SYSTEMCTL="${WORK_DIR}/systemctl.fail"
readonly FAIL_MKTEMP="${WORK_DIR}/mktemp.fail"
readonly FAIL_SYNC="${WORK_DIR}/sync.fail"
readonly LOG_SYNC="${WORK_DIR}/sync.log"
readonly SYSTEMCTL_CALL_LOG="${WORK_DIR}/systemctl.calls"
readonly SYSTEMCTL_MOUNT_MARKER="${WORK_DIR}/systemctl.mount.ok"
readonly MKTEMP_REACHED_MARKER="${WORK_DIR}/mktemp.reached"
readonly SYNC_REACHED_MARKER="${WORK_DIR}/sync.reached"
readonly SYNC_MATCH_COUNT="${WORK_DIR}/sync.match-count"
readonly SYNC_CALL_LOG="${WORK_DIR}/sync.calls"
readonly UNIT="autostream-worker.service"
readonly UNIT_PATH="/etc/systemd/system/${UNIT}"
readonly PUBLIC_BINARY="/usr/local/bin/autostream-worker"
readonly PUBLIC_ALIAS="/usr/local/bin/worker"
readonly ENV_PATH="/etc/autostream/worker.env"
readonly STATE_DIR="/var/lib/autostream/worker"
readonly MANAGED_ROOT="/opt/autostream/worker"
readonly INSTALL_BACKUP_ROOT="/var/backups/autostream/install-migrations/worker"
target_lock_id="$(printf '%s' "${UNIT}" | sha256sum | awk 'NR == 1 { print substr($1, 1, 12) }')"
[[ ${target_lock_id} =~ ^[0-9a-f]{12}$ ]] || die "could not derive updater target lock ID"
readonly TARGET_LOCK_ID="${target_lock_id}"
readonly TARGET_LOCK="/run/autostream-updater/.autostream-updater-${TARGET_LOCK_ID}.lock"
readonly LEGACY_UNIT_CONTENT="worker-installer-integration-legacy-unit"
readonly LEGACY_BINARY_CONTENT="worker-installer-integration-legacy-binary"
readonly LEGACY_ALIAS_CONTENT="worker-installer-integration-legacy-alias"
readonly LEGACY_ENV_CONTENT="WORKER_INSTALLER_INTEGRATION_ENV=preserve-exactly"

created_autostream_user=false
old_pid=""

cleanup() {
  local exit_code=$?
  set +e
  systemctl stop "${UNIT}" >/dev/null 2>&1
  systemctl disable "${UNIT}" >/dev/null 2>&1
  rm -f -- "${UNIT_PATH}"
  systemctl daemon-reload >/dev/null 2>&1
  if [[ -n ${old_pid} ]]; then
    kill "${old_pid}" >/dev/null 2>&1
  fi
  rm -f -- "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}" "${ENV_PATH}" "${TARGET_LOCK}"
  rm -rf -- \
    "${STATE_DIR}" \
    "${MANAGED_ROOT}" \
    "${INSTALL_BACKUP_ROOT}" \
    "${WORK_DIR}"
  rmdir \
    /var/backups/autostream/install-migrations \
    /var/backups/autostream \
    /var/lib/autostream \
    /opt/autostream \
    /etc/autostream \
    /run/autostream-updater >/dev/null 2>&1
  if [[ ${created_autostream_user} == true ]]; then
    userdel autostream >/dev/null 2>&1
    groupdel autostream >/dev/null 2>&1
  fi
  exit "${exit_code}"
}
trap cleanup EXIT
chmod 0755 "${WORK_DIR}"

for path in \
  "${UNIT_PATH}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${STATE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${TARGET_LOCK}"; do
  [[ ! -e ${path} && ! -L ${path} ]] || die "runner is not clean at ${path}"
done
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "runner already has an autostream account"
fi
created_autostream_user=true

install -d -o root -g root -m 0755 \
  "${ARTIFACTS_DIR}" \
  "${EXTRACTED_ROOT}/bin" \
  "${EXTRACTED_ROOT}/systemd"
install -o root -g root -m 0755 "${INSTALLER_SOURCE}" \
  "${EXTRACTED_ROOT}/install-autostream-worker"

cat > "${EXTRACTED_ROOT}/bin/autostream-worker" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "--version" ]; then
  printf '%s\n' 'autostream-worker v9.9.9'
  printf '%s\n' 'commit: integration-test'
  printf '%s\n' 'build_date: integration-test'
  exit 0
fi
exit 99
EOF
chmod 0755 "${EXTRACTED_ROOT}/bin/autostream-worker"
cp "${EXTRACTED_ROOT}/bin/autostream-worker" "${EXTRACTED_ROOT}/bin/worker"
chmod 0755 "${EXTRACTED_ROOT}/bin/worker"

cat > "${EXTRACTED_ROOT}/systemd/autostream-worker.service.example" <<'EOF'
[Unit]
Description=AutoStream Worker integration fixture

[Service]
Type=simple
User=autostream
Group=autostream
EnvironmentFile=-/etc/autostream/worker.env
ExecStart=/usr/local/bin/autostream-worker

[Install]
WantedBy=multi-user.target
EOF
printf '%s\n' 'AUTOSTREAM_BIND_ADDR=127.0.0.1:18084' \
  > "${EXTRACTED_ROOT}/.env.example"
printf '%s\n' 'integration fixture' > "${EXTRACTED_ROOT}/README.install.md"

(
  cd -- "${EXTRACTED_ROOT}"
  find . -type f ! -path './checksums.txt' -print0 |
    sort -z |
    xargs -0 sha256sum > checksums.txt
)
tar -C "${ARTIFACTS_DIR}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
(
  cd -- "${ARTIFACTS_DIR}"
  sha256sum "${ARTIFACT_ID}.tar.gz" > "${ARTIFACT_ID}.tar.gz.sha256"
)
archive_sha256="$(sha256sum "${ARCHIVE}" | awk 'NR == 1 { print $1 }')"
archive_size="$(stat -c %s "${ARCHIVE}")"
readonly RETAINED_DIR="${INSTALL_BACKUP_ROOT}/${VERSION}-${archive_sha256:0:12}"
jq -n \
  --arg version "${VERSION}" \
  --arg name "${ARTIFACT_ID}.tar.gz" \
  --arg sha256 "${archive_sha256}" \
  --argjson size "${archive_size}" \
  '{
    schema_version: 1,
    release_id: $version,
    channel: "host",
    published_at: "2026-01-01T00:00:00Z",
    minimum_agent_version: "v1.0.0",
    components: [{
      service: "worker",
      source_version: $version,
      commit: "0123456789abcdef0123456789abcdef01234567",
      rollback_compatible: true,
      database_schema: "none",
      artifacts: [{
        os: "linux",
        arch: "amd64",
        name: $name,
        sha256: $sha256,
        size: $size
      }, {
        os: "linux",
        arch: "arm64",
        name: ("autostream-worker_" + $version + "_linux_arm64.tar.gz"),
        sha256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        size: 12345
      }]
    }]
  }' > "${ARTIFACTS_DIR}/release-manifest.json"
(
  cd -- "${ARTIFACTS_DIR}"
  sha256sum release-manifest.json > release-manifest.json.sha256
)

install -o root -g root -m 0755 /usr/bin/systemctl "${REAL_SYSTEMCTL_COPY}"
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

"${EXTRACTED_ROOT}/install-autostream-worker" > "${WORK_DIR}/fresh.out"
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
groupdel autostream
[[ ! -e ${MANAGED_ROOT} && ! -L ${MANAGED_ROOT} ]] || \
  die "fresh-install reset retained the managed root"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "fresh-install reset retained the autostream account"
fi

groupadd --system autostream
useradd --system --gid autostream --home-dir /var/lib/autostream \
  --no-create-home --shell /usr/sbin/nologin autostream
install -d -o root -g root -m 0755 /etc/autostream /var/lib/autostream
install -d -o autostream -g autostream -m 0750 "${STATE_DIR}"
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
EOF
chmod 0644 "${UNIT_PATH}"
systemctl daemon-reload
systemctl start "${UNIT}"
old_pid="$(systemctl show --property MainPID --value "${UNIT}")"
[[ ${old_pid} =~ ^[1-9][0-9]*$ ]] || die "legacy service did not start"
kill -0 "${old_pid}" || die "legacy service PID is not alive"
systemctl is-enabled --quiet "${UNIT}" && die "legacy fixture unexpectedly enabled the service"

env_before="$(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }')"
unit_before="$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')"

(
  exec 8>"${TARGET_LOCK}"
  flock -n 8
  set +e
  "${EXTRACTED_ROOT}/install-autostream-worker" \
    > "${WORK_DIR}/lock-contention.out" 2>&1
  printf '%s\n' "$?" > "${WORK_DIR}/lock-contention.status"
)
contention_status="$(< "${WORK_DIR}/lock-contention.status")"
[[ ${contention_status} -eq 1 ]] || die "installer ignored updater lock contention"
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
systemctl is-enabled --quiet "${UNIT}" && die "lock contention enabled the service"

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
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "failed migration changed the existing environment"
[[ $(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }') == "${unit_before}" ]] || \
  die "failed migration did not restore the systemd unit"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "failed migration replaced the running legacy process"
kill -0 "${old_pid}" || die "failed migration stopped the running legacy process"
systemctl is-enabled --quiet "${UNIT}" && die "failed migration unexpectedly enabled the service"

for retained in \
  "${RETAINED_DIR}/autostream-worker" \
  "${RETAINED_DIR}/worker" \
  "${RETAINED_DIR}/autostream-worker.service"; do
  [[ -f ${retained} && ! -L ${retained} ]] || \
    die "failed migration did not retain durable retry backup: ${retained}"
done

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
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "sync failure changed the existing environment"
[[ $(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }') == "${unit_before}" ]] || \
  die "sync failure did not restore the systemd unit"
[[ $(systemctl show --property MainPID --value "${UNIT}") == "${old_pid}" ]] || \
  die "sync failure replaced the running legacy process"
kill -0 "${old_pid}" || die "sync failure stopped the running legacy process"
systemctl is-enabled --quiet "${UNIT}" && die "sync failure enabled the service"
for retained in \
  "${RETAINED_DIR}/autostream-worker" \
  "${RETAINED_DIR}/worker" \
  "${RETAINED_DIR}/autostream-worker.service"; do
  [[ -f ${retained} && ! -L ${retained} ]] || \
    die "sync failure discarded recoverable legacy backup: ${retained}"
done

rm -f -- "${PUBLIC_ALIAS}"
sync -f /usr/local/bin

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
[[ $(sha256sum "${ENV_PATH}" | awk 'NR == 1 { print $1 }') == "${env_before}" ]] || \
  die "idempotent reinstall changed the existing environment"
systemctl is-enabled --quiet "${UNIT}" && die "idempotent reinstall unexpectedly enabled the service"

printf '%s\n' "Worker installer integration scenarios passed."

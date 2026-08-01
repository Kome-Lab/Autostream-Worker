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
    set -euo pipefail
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-worker-installer-test-scratch /mnt
    install -d -o root -g root -m 0755 \
      /mnt/usr-lower \
      /mnt/etc-lower \
      /mnt/var-lower \
      /mnt/run-lower
    mount --rbind /usr /mnt/usr-lower
    mount --make-rprivate /mnt/usr-lower
    mount --rbind /etc /mnt/etc-lower
    mount --make-rprivate /mnt/etc-lower
    mount --rbind /var /mnt/var-lower
    mount --make-rprivate /mnt/var-lower
    mount --rbind /run /mnt/run-lower
    mount --make-rprivate /mnt/run-lower
    install -d -o root -g root -m 0755 \
      /mnt/usr-upper \
      /mnt/usr-upper/local \
      /mnt/etc-upper \
      /mnt/etc-upper/systemd \
      /mnt/etc-upper/systemd/system \
      /mnt/var-upper \
      /mnt/var-upper/lib \
      /mnt/var-upper/backups \
      /mnt/run-upper \
      /mnt/run-upper/systemd
    install -d -o root -g root -m 1777 /mnt/var-upper/tmp
    install -d -o root -g root -m 0700 \
      /mnt/usr-work \
      /mnt/etc-work \
      /mnt/var-work \
      /mnt/run-work
    mount -t overlay -o nodev,nosuid,lowerdir=/mnt/usr-lower,upperdir=/mnt/usr-upper,workdir=/mnt/usr-work \
      autostream-worker-installer-test-usr-overlay /usr
    mount -t overlay -o nodev,nosuid,lowerdir=/mnt/etc-lower,upperdir=/mnt/etc-upper,workdir=/mnt/etc-work \
      autostream-worker-installer-test-etc-overlay /etc
    mount -t overlay -o nodev,nosuid,lowerdir=/mnt/var-lower,upperdir=/mnt/var-upper,workdir=/mnt/var-work \
      autostream-worker-installer-test-var-overlay /var
    mount -t overlay -o nodev,nosuid,lowerdir=/mnt/run-lower,upperdir=/mnt/run-upper,workdir=/mnt/run-work \
      autostream-worker-installer-test-run-overlay /run
    mount --rbind /mnt/run-lower/systemd /run/systemd
    mount --make-rprivate /run/systemd
    systemd_identity="$(stat -c "%d:%i" -- /mnt/run-lower/systemd)"
    [[ ${systemd_identity} =~ ^[0-9]+:[0-9]+$ &&
      $(stat -c "%d:%i" -- /run/systemd) == "${systemd_identity}" ]]
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-worker-installer-test /usr/local/bin
    mount -t tmpfs -o nodev,nosuid,mode=0755,uid=0,gid=0 \
      autostream-worker-installer-test-opt /opt
    mount -t tmpfs -o ro,nodev,nosuid,noexec,mode=0555,uid=0,gid=0 \
      autostream-worker-installer-test-sealed /mnt
    exec env \
      AUTOSTREAM_WORKER_INSTALLER_TEST_MOUNT_NS=1 \
      AUTOSTREAM_WORKER_INSTALLER_TEST_SYSTEMD_IDENTITY="${systemd_identity}" \
      bash "$1"
  ' autostream-worker-installer-test-mount "$0"
fi
grep -Eq ' /mnt .* - tmpfs autostream-worker-installer-test-sealed ' \
  /proc/self/mountinfo || die "sealed /mnt mount is missing"
awk '$5 == "/mnt" &&
  $6 ~ /(^|,)ro(,|$)/ &&
  $6 ~ /(^|,)nodev(,|$)/ &&
  $6 ~ /(^|,)nosuid(,|$)/ &&
  $6 ~ /(^|,)noexec(,|$)/ { found = 1 }
  END { exit !found }' /proc/self/mountinfo || \
  die "sealed /mnt mount options are unsafe"
[[ $(stat -c '%U:%G:%a' -- /mnt) == "root:root:555" ]] || \
  die "sealed /mnt ownership or mode is unsafe"
if touch /mnt/.autostream-worker-write-probe 2>/dev/null; then
  rm -f -- /mnt/.autostream-worker-write-probe
  die "sealed /mnt unexpectedly accepted a write"
fi
grep -Eq ' /usr .* - overlay autostream-worker-installer-test-usr-overlay ' \
  /proc/self/mountinfo || die "isolated /usr overlay mount is missing"
grep -Eq ' /etc .* - overlay autostream-worker-installer-test-etc-overlay ' \
  /proc/self/mountinfo || die "isolated /etc overlay mount is missing"
grep -Eq ' /var .* - overlay autostream-worker-installer-test-var-overlay ' \
  /proc/self/mountinfo || die "isolated /var overlay mount is missing"
grep -Eq ' /run .* - overlay autostream-worker-installer-test-run-overlay ' \
  /proc/self/mountinfo || die "isolated /run overlay mount is missing"
grep -Eq ' /run/systemd ' /proc/self/mountinfo || \
  die "host-backed /run/systemd mount is missing"
readonly EXPECTED_SYSTEMD_IDENTITY="${AUTOSTREAM_WORKER_INSTALLER_TEST_SYSTEMD_IDENTITY:-}"
[[ ${EXPECTED_SYSTEMD_IDENTITY} =~ ^[0-9]+:[0-9]+$ &&
  $(stat -c '%d:%i' -- /run/systemd) == "${EXPECTED_SYSTEMD_IDENTITY}" ]] || \
  die "host-backed /run/systemd mount identity is invalid"
grep -Eq ' /usr/local/bin .* - tmpfs autostream-worker-installer-test ' \
  /proc/self/mountinfo || die "isolated /usr/local/bin mount is missing"
grep -Eq ' /opt .* - tmpfs autostream-worker-installer-test-opt ' \
  /proc/self/mountinfo || die "isolated /opt mount is missing"
[[ $(stat -c '%U:%G:%a' -- /usr) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr fixture"
[[ $(stat -c '%U:%G:%a' -- /etc) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc fixture"
[[ $(stat -c '%U:%G:%a' -- /etc/systemd) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc/systemd fixture"
[[ $(stat -c '%U:%G:%a' -- /etc/systemd/system) == "root:root:755" ]] || \
  die "could not create an isolated safe /etc/systemd/system fixture"
[[ $(stat -c '%U:%G:%a' -- /var) == "root:root:755" ]] || \
  die "could not create an isolated safe /var fixture"
[[ $(stat -c '%U:%G:%a' -- /var/lib) == "root:root:755" ]] || \
  die "could not create an isolated safe /var/lib fixture"
[[ $(stat -c '%U:%G:%a' -- /var/backups) == "root:root:755" ]] || \
  die "could not create an isolated safe /var/backups fixture"
[[ $(stat -c '%U:%G:%a' -- /var/tmp) == "root:root:1777" ]] || \
  die "could not create an isolated safe /var/tmp fixture"
[[ $(stat -c '%U:%G:%a' -- /run) == "root:root:755" ]] || \
  die "could not create an isolated safe /run fixture"
[[ $(stat -c '%U:%G:%a' -- /usr/local) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr/local fixture"
[[ $(stat -c '%U:%G:%a' -- /usr/local/bin) == "root:root:755" ]] || \
  die "could not create an isolated safe /usr/local/bin fixture"
[[ $(stat -c '%U:%G:%a' -- /opt) == "root:root:755" ]] || \
  die "could not create an isolated safe /opt fixture"

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
readonly REAL_GROUPADD_COPY="${WORK_DIR}/groupadd.real"
readonly REAL_USERADD_COPY="${WORK_DIR}/useradd.real"
readonly REAL_MKTEMP_COPY="${WORK_DIR}/mktemp.real"
readonly REAL_SYNC_COPY="${WORK_DIR}/sync.real"
readonly FAIL_SYSTEMCTL="${WORK_DIR}/systemctl.fail"
readonly SIGNAL_SYSTEMCTL="${WORK_DIR}/systemctl.signal"
readonly SIGNAL_GROUPADD="${WORK_DIR}/groupadd.signal"
readonly SIGNAL_USERADD="${WORK_DIR}/useradd.signal"
readonly FAIL_MKTEMP="${WORK_DIR}/mktemp.fail"
readonly FAIL_SYNC="${WORK_DIR}/sync.fail"
readonly LOG_SYNC="${WORK_DIR}/sync.log"
readonly SYSTEMCTL_CALL_LOG="${WORK_DIR}/systemctl.calls"
readonly SYSTEMCTL_MOUNT_MARKER="${WORK_DIR}/systemctl.mount.ok"
readonly MKTEMP_REACHED_MARKER="${WORK_DIR}/mktemp.reached"
readonly GROUPADD_SIGNAL_MARKER="${WORK_DIR}/groupadd.signal.reached"
readonly USERADD_SIGNAL_MARKER="${WORK_DIR}/useradd.signal.reached"
readonly SIGNAL_REACHED_MARKER="${WORK_DIR}/signal.reached"
readonly SIGNAL_MATCH_COUNT="${WORK_DIR}/signal.match-count"
readonly SYNC_REACHED_MARKER="${WORK_DIR}/sync.reached"
readonly SYNC_MATCH_COUNT="${WORK_DIR}/sync.match-count"
readonly SYNC_CALL_LOG="${WORK_DIR}/sync.calls"
readonly UNIT="autostream-worker.service"
readonly UNIT_PATH="/etc/systemd/system/${UNIT}"
readonly RUNTIME_UNIT_PATH="/run/systemd/system/${UNIT}"
[[ -d /run/systemd/system && ! -L /run/systemd/system &&
  $(readlink -f -- /run/systemd/system) == "/run/systemd/system" &&
  $(stat -c '%U:%G:%a' -- /run/systemd/system) == "root:root:755" ]] || \
  die "systemd runtime unit directory is unsafe"
readonly PUBLIC_BINARY="/usr/local/bin/autostream-worker"
readonly PUBLIC_ALIAS="/usr/local/bin/worker"
readonly ENV_PATH="/etc/autostream/worker.env"
readonly STATE_DIR="/var/lib/autostream/worker"
readonly STATE_SENTINEL="${STATE_DIR}/rollback-sentinel.txt"
readonly MANAGED_ROOT="/opt/autostream/worker"
readonly INSTALL_BACKUP_ROOT="/var/backups/autostream/install-migrations/worker"
target_lock_id="$(printf '%s' "${UNIT}" | sha256sum | awk 'NR == 1 { print substr($1, 1, 12) }')"
[[ ${target_lock_id} =~ ^[0-9a-f]{12}$ ]] || die "could not derive updater target lock ID"
readonly TARGET_LOCK_ID="${target_lock_id}"
readonly TARGET_LOCK="/run/autostream-updater/.autostream-updater-${TARGET_LOCK_ID}.lock"
readonly SHARED_HOST_SETUP_LOCK="/run/autostream-updater/.autostream-runtime-host-setup.lock"
readonly LEGACY_UNIT_CONTENT="worker-installer-integration-legacy-unit"
readonly LEGACY_BINARY_CONTENT="worker-installer-integration-legacy-binary"
readonly LEGACY_ALIAS_CONTENT="worker-installer-integration-legacy-alias"
readonly LEGACY_ENV_CONTENT="WORKER_INSTALLER_INTEGRATION_ENV=preserve-exactly"

created_autostream_user=false
fixture_owns_paths=false
fixture_owns_runtime_unit=false
fixture_owns_service=false
late_preflight_public_path_owned=false
runtime_unit_identity=""
runtime_unit_staging=""
old_pid=""
old_pid_starttime=""

read_process_starttime() {
  local pid=$1
  local stat_line=""
  local stat_tail=""
  local -a stat_fields=()

  [[ ${pid} =~ ^[1-9][0-9]*$ && -r /proc/${pid}/stat ]] || return 1
  IFS= read -r stat_line < "/proc/${pid}/stat" || return 1
  stat_tail="${stat_line##*) }"
  read -r -a stat_fields <<< "${stat_tail}"
  [[ ${#stat_fields[@]} -ge 20 && ${stat_fields[19]} =~ ^[0-9]+$ ]] || return 1
  printf '%s\n' "${stat_fields[19]}"
}

kill_recorded_process_if_same_starttime() {
  local current_starttime=""

  [[ -n ${old_pid} && -n ${old_pid_starttime} ]] || return 0
  if [[ ! -e /proc/${old_pid} ]]; then
    old_pid=""
    old_pid_starttime=""
    return 0
  fi
  current_starttime="$(read_process_starttime "${old_pid}")" || return 1
  [[ ${current_starttime} == "${old_pid_starttime}" ]] || return 2
  kill "${old_pid}" || return 1
  old_pid=""
  old_pid_starttime=""
}

stage_runtime_unit() {
  local source_path=$1
  local staged_path=""

  if ! staged_path="$(mktemp "/run/systemd/system/.${UNIT}.fixture.XXXXXXXX")"; then
    die "could not stage the systemd runtime unit"
  fi
  runtime_unit_staging="${staged_path}"
  install -o root -g root -m 0644 "${source_path}" "${runtime_unit_staging}"
  sync -f -- "${runtime_unit_staging}"
}

create_runtime_unit_no_clobber() {
  local source_path=$1

  stage_runtime_unit "${source_path}"
  if ! ln -- "${runtime_unit_staging}" "${RUNTIME_UNIT_PATH}"; then
    rm -f -- "${runtime_unit_staging}"
    runtime_unit_staging=""
    die "runner became unclean at ${RUNTIME_UNIT_PATH}"
  fi
  fixture_owns_runtime_unit=true
  runtime_unit_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  rm -f -- "${runtime_unit_staging}"
  runtime_unit_staging=""
  sync -f -- /run/systemd/system
}

replace_owned_runtime_unit_atomically() {
  local source_path=$1
  local current_identity=""

  [[ ${fixture_owns_runtime_unit} == true ]] || \
    die "refusing to replace an unowned systemd runtime unit"
  [[ -f ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]] || \
    die "owned systemd runtime unit disappeared or became unsafe"
  current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  [[ ${current_identity} == "${runtime_unit_identity}" ]] || \
    die "owned systemd runtime unit was replaced externally"

  stage_runtime_unit "${source_path}"
  current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  if [[ ${current_identity} != "${runtime_unit_identity}" ]]; then
    rm -f -- "${runtime_unit_staging}"
    runtime_unit_staging=""
    die "owned systemd runtime unit changed before atomic commit"
  fi
  mv -fT -- "${runtime_unit_staging}" "${RUNTIME_UNIT_PATH}"
  runtime_unit_staging=""
  runtime_unit_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  sync -f -- /run/systemd/system
}

assert_loaded_runtime_unit() {
  local expected_exec=$1
  local expected_user=$2
  local scenario=$3
  local fragment_path=""
  local exec_start=""
  local service_user=""

  fragment_path="$(systemctl show --property FragmentPath --value "${UNIT}")"
  [[ ${fragment_path} == "${RUNTIME_UNIT_PATH}" ]] || \
    die "${scenario} loaded unit from ${fragment_path:-unknown}, expected ${RUNTIME_UNIT_PATH}"
  exec_start="$(systemctl show --property ExecStart --value "${UNIT}")"
  [[ ${exec_start} == *"${expected_exec}"* ]] || \
    die "${scenario} loaded unexpected ExecStart: ${exec_start:-empty}"
  service_user="$(systemctl show --property User --value "${UNIT}")"
  [[ ${service_user} == "${expected_user}" ]] || \
    die "${scenario} loaded unexpected User: ${service_user:-root}"
}

assert_legacy_runtime_unit() {
  local scenario=$1

  [[ -f ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]] || \
    die "${scenario} lost the legacy runtime unit"
  [[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
    "${runtime_unit_before}" ]] || \
    die "${scenario} changed the legacy runtime unit"
  [[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
    "$(sha256sum "${UNIT_PATH}" | awk 'NR == 1 { print $1 }')" ]] || \
    die "${scenario} runtime unit diverged from the restored legacy unit"
  assert_loaded_runtime_unit "/usr/bin/sleep" "" "${scenario}"
}

run_preflight_cleanup_probe() {
  local probe_enabled_state=""
  local probe_hash=""
  local probe_identity=""
  local probe_pid=""
  local probe_status=0
  local mismatch_status=0
  local saved_starttime=""
  local current_identity=""

  [[ ${fixture_owns_paths} == false &&
    ${fixture_owns_runtime_unit} == false &&
    ${fixture_owns_service} == false ]] || \
    die "preflight cleanup probe began after fixture ownership"

  cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=AutoStream Worker preflight cleanup sentinel

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
  probe_pid="$(systemctl show --property MainPID --value "${UNIT}")"
  [[ ${probe_pid} =~ ^[1-9][0-9]*$ ]] || \
    die "preflight cleanup sentinel did not start"
  old_pid="${probe_pid}"
  if ! old_pid_starttime="$(read_process_starttime "${old_pid}")"; then
    die "could not record the preflight cleanup sentinel process identity"
  fi
  assert_loaded_runtime_unit "/usr/bin/sleep" "" "preflight cleanup sentinel"
  saved_starttime="${old_pid_starttime}"
  old_pid_starttime=0
  set +e
  kill_recorded_process_if_same_starttime
  mismatch_status=$?
  set -e
  [[ ${mismatch_status} -eq 2 ]] || \
    die "PID reuse guard did not reject a mismatched process identity"
  kill -0 "${probe_pid}" || die "PID reuse guard killed the mismatched process"
  old_pid_starttime="${saved_starttime}"
  probe_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  probe_hash="$(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }')"
  probe_enabled_state="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"
  rm -f -- "${UNIT_PATH}"

  set +e
  AUTOSTREAM_WORKER_INSTALLER_TEST_MOUNT_NS=1 \
    AUTOSTREAM_WORKER_INSTALLER_TEST_PREFLIGHT_PROBE=1 \
    bash "$0" > "${WORK_DIR}/preflight-cleanup-probe.out" 2>&1
  probe_status=$?
  set -e
  [[ ${probe_status} -ne 0 ]] || \
    die "preflight cleanup probe unexpectedly passed"
  grep -Fx -- \
    "worker installer integration test: runner is not clean at ${RUNTIME_UNIT_PATH}" \
    "${WORK_DIR}/preflight-cleanup-probe.out" >/dev/null || \
    die "preflight cleanup probe did not stop at the runtime unit conflict"
  [[ -f ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]] || \
    die "preflight failure removed the existing runtime unit"
  [[ $(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}") == "${probe_identity}" ]] || \
    die "preflight failure replaced the existing runtime unit"
  [[ $(sha256sum "${RUNTIME_UNIT_PATH}" | awk 'NR == 1 { print $1 }') == \
    "${probe_hash}" ]] || \
    die "preflight failure changed the existing runtime unit"
  [[ $(systemctl show --property MainPID --value "${UNIT}") == "${probe_pid}" ]] || \
    die "preflight failure replaced the existing service process"
  kill -0 "${probe_pid}" || die "preflight failure stopped the existing service process"
  [[ $(systemctl is-enabled "${UNIT}" 2>/dev/null || true) == \
    "${probe_enabled_state}" ]] || \
    die "preflight failure changed the existing service enablement"
  assert_loaded_runtime_unit "/usr/bin/sleep" "" "preflight failure"

  current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  [[ ${current_identity} == "${runtime_unit_identity}" ]] || \
    die "preflight cleanup sentinel runtime unit was replaced externally"
  systemctl stop "${UNIT}"
  current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"
  [[ ${current_identity} == "${runtime_unit_identity}" ]] || \
    die "preflight cleanup sentinel runtime unit was replaced externally"
  fixture_owns_service=false
  old_pid=""
  old_pid_starttime=""
  rm -f -- "${RUNTIME_UNIT_PATH}"
  fixture_owns_runtime_unit=false
  runtime_unit_identity=""
  sync -f -- /run/systemd/system
  systemctl daemon-reload
  [[ $(systemctl show --property LoadState --value "${UNIT}") == "not-found" ]] || \
    die "preflight cleanup sentinel remained loaded"
}

cleanup() {
  local exit_code=$?
  local current_identity=""
  local active_state=""
  local load_state=""
  local kill_status=0
  local cleanup_failed=false
  local runtime_identity_matches=false
  local should_reload=false

  set +e
  if [[ ${late_preflight_public_path_owned} == true ]]; then
    if [[ -d ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} &&
      -z $(find "${PUBLIC_BINARY}" -mindepth 1 -print -quit) ]]; then
      rmdir -- "${PUBLIC_BINARY}" || cleanup_failed=true
    else
      printf '%s\n' "worker installer integration test cleanup: late-preflight public path changed" >&2
      cleanup_failed=true
    fi
  fi
  if [[ ${fixture_owns_runtime_unit} == true &&
    -f ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]]; then
    current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}" 2>/dev/null)"
    if [[ -n ${runtime_unit_identity} &&
      ${current_identity} == "${runtime_unit_identity}" ]]; then
      runtime_identity_matches=true
    else
      printf '%s\n' "worker installer integration test cleanup: owned runtime unit identity changed" >&2
      cleanup_failed=true
    fi
  elif [[ ${fixture_owns_runtime_unit} == true ]]; then
    printf '%s\n' "worker installer integration test cleanup: owned runtime unit is missing or unsafe" >&2
    cleanup_failed=true
    [[ ! -e ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]] && should_reload=true
  fi
  if [[ ${fixture_owns_service} == true &&
    ${runtime_identity_matches} == true ]]; then
    current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}" 2>/dev/null)"
    if [[ ${current_identity} != "${runtime_unit_identity}" ]]; then
      runtime_identity_matches=false
      printf '%s\n' "worker installer integration test cleanup: runtime unit identity changed before service cleanup" >&2
      cleanup_failed=true
    else
      if systemctl stop "${UNIT}" >/dev/null 2>&1; then
        old_pid=""
        old_pid_starttime=""
      else
        printf '%s\n' "worker installer integration test cleanup: could not stop owned service" >&2
        cleanup_failed=true
      fi
      if ! systemctl disable "${UNIT}" >/dev/null 2>&1; then
        printf '%s\n' "worker installer integration test cleanup: could not disable owned service" >&2
        cleanup_failed=true
      fi
    fi
  fi
  if [[ ${fixture_owns_service} == true && -n ${old_pid} ]]; then
    kill_recorded_process_if_same_starttime
    kill_status=$?
    case ${kill_status} in
      0)
        ;;
      2)
        printf '%s\n' "worker installer integration test cleanup: refusing to kill a reused PID" >&2
        ;;
      *)
        printf '%s\n' "worker installer integration test cleanup: could not terminate recorded service process" >&2
        cleanup_failed=true
        ;;
    esac
  fi
  if [[ ${fixture_owns_runtime_unit} == true &&
    ${runtime_identity_matches} == true ]]; then
    current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}" 2>/dev/null)"
    if [[ ${current_identity} != "${runtime_unit_identity}" ]]; then
      runtime_identity_matches=false
      printf '%s\n' "worker installer integration test cleanup: runtime unit identity changed before removal" >&2
      cleanup_failed=true
    else
      if rm -f -- "${RUNTIME_UNIT_PATH}" &&
        [[ ! -e ${RUNTIME_UNIT_PATH} && ! -L ${RUNTIME_UNIT_PATH} ]]; then
        should_reload=true
      else
        printf '%s\n' "worker installer integration test cleanup: could not remove owned runtime unit" >&2
        cleanup_failed=true
      fi
    fi
  fi
  if [[ -n ${runtime_unit_staging} ]]; then
    if ! rm -f -- "${runtime_unit_staging}"; then
      printf '%s\n' "worker installer integration test cleanup: could not remove runtime staging file" >&2
      cleanup_failed=true
    fi
  fi
  if [[ ${fixture_owns_paths} == true ]]; then
    if ! rm -f -- "${UNIT_PATH}"; then
      printf '%s\n' "worker installer integration test cleanup: could not remove private systemd unit" >&2
      cleanup_failed=true
    fi
  fi
  if [[ ${should_reload} == true ]]; then
    if ! sync -f -- /run/systemd/system ||
      ! systemctl daemon-reload >/dev/null 2>&1; then
      printf '%s\n' "worker installer integration test cleanup: could not reload systemd after runtime cleanup" >&2
      cleanup_failed=true
    fi
  fi
  if [[ ${fixture_owns_service} == true || ${fixture_owns_runtime_unit} == true ]]; then
    active_state="$(systemctl show --property ActiveState --value "${UNIT}" 2>/dev/null)"
    if [[ ${active_state} != "inactive" ]]; then
      printf '%s\n' "worker installer integration test cleanup: service did not become inactive" >&2
      cleanup_failed=true
    fi
    load_state="$(systemctl show --property LoadState --value "${UNIT}" 2>/dev/null)"
    if [[ ${load_state} != "not-found" ]]; then
      printf '%s\n' "worker installer integration test cleanup: service unit remained loaded" >&2
      cleanup_failed=true
    fi
  fi
  if [[ ${fixture_owns_paths} == true ]]; then
    rm -f -- \
      "${PUBLIC_BINARY}" \
      "${PUBLIC_ALIAS}" \
      "${ENV_PATH}" \
      "${TARGET_LOCK}" \
      "${SHARED_HOST_SETUP_LOCK}"
    rm -rf -- \
      "${STATE_DIR}" \
      "${MANAGED_ROOT}" \
      "${INSTALL_BACKUP_ROOT}"
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
  fi
  rm -rf -- "${WORK_DIR}"
  if [[ ${cleanup_failed} == true && ${exit_code} -eq 0 ]]; then
    exit_code=1
  fi
  exit "${exit_code}"
}
trap cleanup EXIT
chmod 0755 "${WORK_DIR}"

for path in \
  "${UNIT_PATH}" \
  "${RUNTIME_UNIT_PATH}" \
  "${PUBLIC_BINARY}" \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${STATE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${SHARED_HOST_SETUP_LOCK}" \
  "${TARGET_LOCK}"; do
  [[ ! -e ${path} && ! -L ${path} ]] || die "runner is not clean at ${path}"
done
loaded_unit_state="$(systemctl show --property LoadState --value "${UNIT}" 2>/dev/null || true)"
[[ ${loaded_unit_state} == "not-found" ]] || \
  die "runner service is already loaded: ${UNIT} (${loaded_unit_state:-unknown})"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "runner already has an autostream account"
fi
if [[ ${AUTOSTREAM_WORKER_INSTALLER_TEST_PREFLIGHT_PROBE:-} == "1" ]]; then
  die "preflight probe unexpectedly reached the mutation boundary"
fi
run_preflight_cleanup_probe
fixture_owns_paths=true
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
  printf '%s\n' 'commit: 0123456789abcdef0123456789abcdef01234567'
  printf '%s\n' 'build_date: 2026-01-01T00:00:00Z'
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
jq -n \
  --arg version "${VERSION}" \
  --arg name "${ARTIFACT_ID}.tar.gz" \
  --arg root "${ARTIFACT_ID}" \
  '{
    schema_version: 1,
    component: "worker",
    source_version: $version,
    commit: "0123456789abcdef0123456789abcdef01234567",
    build_date: "2026-01-01T00:00:00Z",
    platform: {
      os: "linux",
      arch: "amd64"
    },
    archive: {
      name: $name,
      root: $root
    },
    compatibility: {
      minimum_agent_version: "v1.0.0",
      minimum_panel_version: null,
      rollback_compatible: true,
      database_schema: "none"
    }
  }' > "${EXTRACTED_ROOT}/artifact-manifest.json"

(
  cd -- "${EXTRACTED_ROOT}"
  find . -type f ! -path './checksums.txt' -print0 |
    sort -z |
    xargs -0 sha256sum > checksums.txt
)
tar -C "${ARTIFACTS_DIR}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
archive_sha256="$(sha256sum "${ARCHIVE}" | awk 'NR == 1 { print $1 }')"
readonly RETAINED_DIR="${INSTALL_BACKUP_ROOT}/${VERSION}-${archive_sha256:0:12}"
[[ ! -e ${ARCHIVE}.sha256 && ! -L ${ARCHIVE}.sha256 ]] || \
  die "archive-only fixture unexpectedly contains an archive checksum sidecar"
[[ ! -e ${ARTIFACTS_DIR}/release-manifest.json &&
  ! -L ${ARTIFACTS_DIR}/release-manifest.json ]] || \
  die "archive-only fixture unexpectedly contains an external release manifest"
[[ ! -e ${ARTIFACTS_DIR}/release-manifest.json.sha256 &&
  ! -L ${ARTIFACTS_DIR}/release-manifest.json.sha256 ]] || \
  die "archive-only fixture unexpectedly contains an external manifest checksum sidecar"

readonly VALID_ARCHIVE="${WORK_DIR}/${ARTIFACT_ID}.valid.tar.gz"
readonly VARIANT_PARENT="${WORK_DIR}/variant"
readonly VARIANT_ROOT="${VARIANT_PARENT}/${ARTIFACT_ID}"
install -o root -g root -m 0600 "${ARCHIVE}" "${VALID_ARCHIVE}"

prepare_variant_tree() {
  rm -rf -- "${VARIANT_PARENT}"
  install -d -o root -g root -m 0700 "${VARIANT_PARENT}"
  cp -a -- "${EXTRACTED_ROOT}" "${VARIANT_ROOT}"
}

replace_variant_manifest() {
  local filter=$1
  local next_manifest="${WORK_DIR}/artifact-manifest.next"

  jq "${filter}" "${VARIANT_ROOT}/artifact-manifest.json" > "${next_manifest}"
  install -o root -g root -m 0644 \
    "${next_manifest}" \
    "${VARIANT_ROOT}/artifact-manifest.json"
  rm -f -- "${next_manifest}"
}

regenerate_variant_checksums() {
  local next_checksums="${WORK_DIR}/variant-checksums.next"

  (
    cd -- "${VARIANT_ROOT}"
    find . -type f ! -path './checksums.txt' -print0 |
      sort -z |
      xargs -0 sha256sum > "${next_checksums}"
  )
  install -o root -g root -m 0644 \
    "${next_checksums}" \
    "${VARIANT_ROOT}/checksums.txt"
  rm -f -- "${next_checksums}"
}

package_variant_archive() {
  tar -C "${VARIANT_PARENT}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
}

package_noncanonical_duplicate_archive() {
  local raw_archive="${WORK_DIR}/noncanonical-duplicate.tar"
  local archive_list="${WORK_DIR}/noncanonical-duplicate.list"

  rm -f -- "${raw_archive}" "${archive_list}"
  tar -C "${VARIANT_PARENT}" -cf "${raw_archive}" "${ARTIFACT_ID}"
  tar -C "${VARIANT_PARENT}" -rf "${raw_archive}" \
    --transform="s#^${ARTIFACT_ID}/\\.env\\.example\$#${ARTIFACT_ID}/./.env.example#" \
    "${ARTIFACT_ID}/.env.example"
  gzip -c "${raw_archive}" > "${ARCHIVE}"
  tar -tzf "${ARCHIVE}" > "${archive_list}"
  grep -Fx -- "${ARTIFACT_ID}/.env.example" "${archive_list}" > /dev/null || \
    die "noncanonical duplicate fixture is missing its canonical entry"
  grep -Fx -- "${ARTIFACT_ID}/./.env.example" "${archive_list}" > /dev/null || \
    die "noncanonical duplicate fixture is missing its alias entry"
  rm -f -- "${raw_archive}" "${archive_list}"
}

restore_valid_archive() {
  install -o root -g root -m 0600 "${VALID_ARCHIVE}" "${ARCHIVE}"
}

assert_preflight_rejection_kept_host_clean() {
  local scenario=$1

  for path in \
    "${PUBLIC_BINARY}" \
    "${PUBLIC_ALIAS}" \
    "${ENV_PATH}" \
    "${UNIT_PATH}" \
    "${STATE_DIR}" \
    "${MANAGED_ROOT}" \
    "${INSTALL_BACKUP_ROOT}" \
    "${SHARED_HOST_SETUP_LOCK}" \
    "${TARGET_LOCK}"; do
    [[ ! -e ${path} && ! -L ${path} ]] || \
      die "${scenario} rejection mutated the host: ${path}"
  done
  [[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
    -name 'autostream-worker-install.*' -print -quit) ]] || \
    die "${scenario} rejection retained production staging"
  if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
    die "${scenario} rejection created the autostream service account"
  fi
}

expect_preflight_rejection() {
  local scenario=$1
  local expected_message=$2
  local output_path="${WORK_DIR}/${scenario}.out"
  local status

  set +e
  "${EXTRACTED_ROOT}/install-autostream-worker" > "${output_path}" 2>&1
  status=$?
  set -e
  [[ ${status} -ne 0 ]] || \
    die "${scenario} artifact unexpectedly passed preflight"
  grep -F -- "${expected_message}" "${output_path}" >/dev/null || \
    die "${scenario} rejection did not report the expected error"
  assert_preflight_rejection_kept_host_clean "${scenario}"
  restore_valid_archive
}

prepare_variant_tree
replace_variant_manifest '.source_version = "v9.9.8"'
regenerate_variant_checksums
package_variant_archive
expect_preflight_rejection \
  "manifest-version-mismatch" \
  "artifact-manifest.json does not describe this exact Worker artifact"

prepare_variant_tree
replace_variant_manifest '.platform.arch = "arm64"'
regenerate_variant_checksums
package_variant_archive
expect_preflight_rejection \
  "manifest-architecture-mismatch" \
  "artifact-manifest.json does not describe this exact Worker artifact"

prepare_variant_tree
replace_variant_manifest \
  '.commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
regenerate_variant_checksums
package_variant_archive
expect_preflight_rejection \
  "binary-commit-mismatch" \
  "Worker binary commit does not match artifact-manifest.json"

prepare_variant_tree
printf '%s\n' 'corrupt unchecked payload' >> "${VARIANT_ROOT}/.env.example"
package_variant_archive
expect_preflight_rejection \
  "inner-checksum-mismatch" \
  "./.env.example: FAILED"

prepare_variant_tree
package_noncanonical_duplicate_archive
expect_preflight_rejection \
  "noncanonical-duplicate-path" \
  "release archive contains an unsafe path: ${ARTIFACT_ID}/./.env.example"

install -d -o root -g root -m 0755 "${PUBLIC_BINARY}"
late_preflight_public_path_owned=true
set +e
"${EXTRACTED_ROOT}/install-autostream-worker" \
  > "${WORK_DIR}/late-host-preflight.out" 2>&1
late_host_preflight_status=$?
set -e
[[ ${late_host_preflight_status} -eq 1 ]] || \
  die "late host preflight fixture unexpectedly succeeded"
grep -Fx -- \
  "install-autostream-worker: existing public path is not a regular file: ${PUBLIC_BINARY}" \
  "${WORK_DIR}/late-host-preflight.out" >/dev/null || \
  die "late host preflight fixture did not reach the public-path rejection"
[[ -d ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} &&
  $(stat -c '%U:%G:%a' -- "${PUBLIC_BINARY}") == "root:root:755" &&
  -z $(find "${PUBLIC_BINARY}" -mindepth 1 -print -quit) ]] || \
  die "late host preflight changed the rejected public path"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "late host preflight created the autostream service account"
fi
for path in \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${UNIT_PATH}" \
  "${STATE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${SHARED_HOST_SETUP_LOCK}" \
  "${TARGET_LOCK}" \
  /opt/autostream \
  /etc/autostream \
  /var/lib/autostream \
  /var/backups/autostream \
  /run/autostream-updater; do
  [[ ! -e ${path} && ! -L ${path} ]] || \
    die "late host preflight created a persistent path: ${path}"
done
[[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
  -name 'autostream-worker-install.*' -print -quit) ]] || \
  die "late host preflight retained production staging"
rmdir -- "${PUBLIC_BINARY}"
late_preflight_public_path_owned=false

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

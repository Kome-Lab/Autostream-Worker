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

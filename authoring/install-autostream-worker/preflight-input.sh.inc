#!/bin/bash
set -euo pipefail

umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
export LC_ALL=C

readonly SERVICE_NAME="worker"
readonly ARTIFACT_PREFIX="autostream-worker"
readonly ARTIFACT_MANIFEST_NAME="artifact-manifest.json"
readonly MANAGED_ROOT="/opt/autostream/worker"
readonly RELEASES_DIR="${MANAGED_ROOT}/releases"
readonly CURRENT_LINK="${MANAGED_ROOT}/current"
readonly PUBLIC_BINARY="/usr/local/bin/autostream-worker"
readonly PUBLIC_ALIAS="/usr/local/bin/worker"
readonly ENV_DEST="/etc/autostream/worker.env"
readonly UNIT_DEST="/etc/systemd/system/autostream-worker.service"
readonly UNIT_NAME="autostream-worker.service"
readonly STATE_DIR="/var/lib/autostream/worker"
readonly INSTALL_MIGRATION_ROOT="/var/backups/autostream/install-migrations"
readonly LEGACY_BACKUP_ROOT="${INSTALL_MIGRATION_ROOT}/worker"
readonly AUTOSTREAM_USER_ROLLBACK_LOGIN="autostream-install-rollback"

die() {
  printf 'install-autostream-worker: %s\n' "$*" >&2
  exit 1
}

[[ ${EUID} -eq 0 ]] || die "must run as root (use sudo)"
[[ $# -eq 0 ]] || die "this installer does not accept arguments"

for command in \
  awk basename chmod chown cmp cp dirname find flock getent grep groupadd groupdel id install jq \
  ln mkdir mktemp mv readlink rm rmdir runuser sha256sum sort stat sync systemctl tar uname \
  uniq useradd userdel usermod; do
  command -v "${command}" >/dev/null 2>&1 || die "required command is unavailable: ${command}"
done

readonly DEFAULT_SCENE_FONT_FILE="/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc"
[[ -f ${DEFAULT_SCENE_FONT_FILE} && ! -L ${DEFAULT_SCENE_FONT_FILE} &&
  -r ${DEFAULT_SCENE_FONT_FILE} ]] || \
  die "required Japanese Noto scene font is unavailable: ${DEFAULT_SCENE_FONT_FILE}"
service_was_active=false
if systemctl is-active --quiet "${UNIT_NAME}"; then
  service_was_active=true
fi

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)" || \
  die "could not resolve the installer directory"
[[ -n ${SCRIPT_DIR} && ${SCRIPT_DIR} == /* && -d ${SCRIPT_DIR} && ! -L ${SCRIPT_DIR} ]] || \
  die "installer directory is missing or unsafe"
readonly SCRIPT_DIR
ARTIFACT_ID="$(basename -- "${SCRIPT_DIR}")" || \
  die "could not resolve the artifact directory name"
[[ -n ${ARTIFACT_ID} && ${ARTIFACT_ID} != */* ]] || \
  die "artifact directory name is empty or unsafe"
readonly ARTIFACT_ID
readonly ARTIFACT_PATTERN="^${ARTIFACT_PREFIX}_(v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?)_linux_(amd64|arm64)$"
if [[ ! ${ARTIFACT_ID} =~ ${ARTIFACT_PATTERN} ]]; then
  die "the extracted directory name is invalid: ${ARTIFACT_ID}"
fi
readonly VERSION="${BASH_REMATCH[1]}"
readonly ARCH="${BASH_REMATCH[3]}"

case "$(uname -m)" in
  x86_64) readonly HOST_ARCH="amd64" ;;
  aarch64|arm64) readonly HOST_ARCH="arm64" ;;
  *) die "unsupported host architecture: $(uname -m)" ;;
esac
[[ ${ARCH} == "${HOST_ARCH}" ]] || \
  die "artifact architecture ${ARCH} does not match host ${HOST_ARCH}"

ARTIFACT_PARENT="$(dirname -- "${SCRIPT_DIR}")" || \
  die "could not resolve the artifact parent directory"
[[ -n ${ARTIFACT_PARENT} && ${ARTIFACT_PARENT} == /* &&
  -d ${ARTIFACT_PARENT} && ! -L ${ARTIFACT_PARENT} ]] || \
  die "artifact parent directory is missing or unsafe"
readonly ARTIFACT_PARENT
readonly ARCHIVE_NAME="${ARTIFACT_ID}.tar.gz"
readonly ARCHIVE_SOURCE="${ARTIFACT_PARENT}/${ARCHIVE_NAME}"
[[ -f ${ARCHIVE_SOURCE} && ! -L ${ARCHIVE_SOURCE} ]] || \
  die "required release archive is missing or unsafe: ${ARCHIVE_SOURCE}"
ARTIFACT_SIZE="$(stat -c %s -- "${ARCHIVE_SOURCE}")" || \
  die "could not calculate the artifact size"
[[ ${ARTIFACT_SIZE} =~ ^[1-9][0-9]*$ ]] || \
  die "could not calculate a positive artifact size"
(( ARTIFACT_SIZE <= 268435456 )) || \
  die "release archive exceeds the 256 MiB safety limit"
readonly ARTIFACT_SIZE

verify_root_permission_boundary() {
  local path=$1
  local mode

  [[ -d ${path} && ! -L ${path} ]] || \
    die "permission boundary is missing or unsafe: ${path}"
  [[ $(readlink -f -- "${path}") == "${path}" ]] || \
    die "permission boundary must resolve to its canonical path: ${path}"
  [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
    die "permission boundary must be owned by root:root: ${path}"
  mode="$(stat -c '%a' -- "${path}")"
  (( (8#${mode} & 0022) == 0 )) || \
    die "permission boundary must not be group/other writable: ${path}"
}

verify_existing_root_permission_boundary() {
  local path=$1

  if [[ -e ${path} || -L ${path} ]]; then
    verify_root_permission_boundary "${path}"
  fi
}

verify_root_sticky_temp_boundary() {
  local path=$1
  local mode

  [[ -d ${path} && ! -L ${path} ]] || \
    die "temporary-file boundary is missing or unsafe: ${path}"
  [[ $(readlink -f -- "${path}") == "${path}" ]] || \
    die "temporary-file boundary must resolve to its canonical path: ${path}"
  [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
    die "temporary-file boundary must be owned by root:root: ${path}"
  mode="$(stat -c '%a' -- "${path}")"
  (( (8#${mode} & 01000) != 0 )) || \
    die "temporary-file boundary must have the sticky bit set: ${path}"
}

verify_existing_worker_state_directory() {
  local state_mode

  if [[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]]; then
    return 0
  fi
  [[ -d ${STATE_DIR} && ! -L ${STATE_DIR} ]] || \
    die "Worker state path must be a non-symlink directory"
  [[ $(readlink -f -- "${STATE_DIR}") == "${STATE_DIR}" ]] || \
    die "Worker state path must resolve to its canonical path"
  [[ $(stat -c '%U:%G' -- "${STATE_DIR}") == "autostream:autostream" ]] || \
    die "Worker state path has unexpected ownership"
  state_mode="$(stat -c '%a' -- "${STATE_DIR}")"
  (( (8#${state_mode} & 07022) == 0 )) || \
    die "Worker state path has unsafe write or special mode bits"
}

for boundary in \
  /opt \
  /usr \
  /usr/local \
  /usr/local/bin \
  /etc \
  /etc/systemd \
  /etc/systemd/system \
  /var \
  /var/lib \
  /run; do
  verify_root_permission_boundary "${boundary}"
done
verify_root_sticky_temp_boundary /var/tmp
for boundary in \
  /opt/autostream \
  "${MANAGED_ROOT}" \
  "${RELEASES_DIR}" \
  /var/lib/autostream \
  /etc/autostream \
  /var/backups \
  /var/backups/autostream \
  "${INSTALL_MIGRATION_ROOT}" \
  "${LEGACY_BACKUP_ROOT}" \
  /run/autostream-updater; do
  verify_existing_root_permission_boundary "${boundary}"
done
verify_existing_worker_state_directory

state_dir_existed=false
state_dir_mutated=false
state_dir_original_identity=""
state_dir_original_owner=""
state_dir_original_mode=""
state_dir_created_identity=""
if [[ -e ${STATE_DIR} || -L ${STATE_DIR} ]]; then
  state_dir_existed=true
  state_dir_original_identity="$(stat -c '%d:%i' -- "${STATE_DIR}")"
  state_dir_original_owner="$(stat -c '%u:%g' -- "${STATE_DIR}")"
  state_dir_original_mode="$(stat -c '%a' -- "${STATE_DIR}")"
fi

verify_state_directory_snapshot() {
  if [[ ${state_dir_existed} == true ]]; then
    [[ -d ${STATE_DIR} &&
      ! -L ${STATE_DIR} &&
      $(readlink -f -- "${STATE_DIR}") == "${STATE_DIR}" &&
      $(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}") == \
        "${state_dir_original_identity}:${state_dir_original_owner}:${state_dir_original_mode}" ]] || \
      die "Worker state path changed after preflight"
  else
    [[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || \
      die "Worker state path appeared after preflight"
  fi
}

deferred_termination_status=0

exit_from_termination_signal() {
  trap '' HUP INT TERM
  local signal_status=$1
  exit "${signal_status}"
}

install_termination_signal_handlers() {
  trap 'exit_from_termination_signal 129' HUP
  trap 'exit_from_termination_signal 130' INT
  trap 'exit_from_termination_signal 143' TERM
}

defer_termination_signals() {
  trap 'deferred_termination_status=129' HUP
  trap 'deferred_termination_status=130' INT
  trap 'deferred_termination_status=143' TERM
}

resume_termination_signals() {
  local pending_status

  install_termination_signal_handlers
  pending_status=${deferred_termination_status}
  deferred_termination_status=0
  if (( pending_status != 0 )); then
    exit_from_termination_signal "${pending_status}"
  fi
}

input_stage=""
cleanup_early_input_stage() {
  local original_status=$?

  trap '' HUP INT TERM
  trap - EXIT
  set +e

  if [[ -n ${input_stage} &&
    ${input_stage} == /var/tmp/autostream-worker-install.* &&
    $(dirname -- "${input_stage}") == "/var/tmp" &&
    -d ${input_stage} &&
    ! -L ${input_stage} &&
    $(readlink -f -- "${input_stage}") == "${input_stage}" ]]; then
    rm -rf -- "${input_stage}"
    sync -f /var/tmp >/dev/null 2>&1 || true
  fi
  return "${original_status}"
}
defer_termination_signals
if ! input_stage="$(mktemp -d /var/tmp/autostream-worker-install.XXXXXXXX)"; then
  die "failed to create input staging directory"
fi
trap cleanup_early_input_stage EXIT
resume_termination_signals
[[ -n ${input_stage} ]] || die "mktemp returned an empty input staging path"
[[ ${input_stage} == /var/tmp/autostream-worker-install.* &&
  $(dirname -- "${input_stage}") == "/var/tmp" &&
  -d ${input_stage} && ! -L ${input_stage} &&
  $(readlink -f -- "${input_stage}") == "${input_stage}" &&
  $(stat -c '%U:%G:%a' -- "${input_stage}") == "root:root:700" ]] || \
  die "mktemp returned an unsafe input staging directory"
readonly INPUT_STAGE="${input_stage}"
readonly UNPACK_DIR="${INPUT_STAGE}/unpack"
managed_candidate=""
managed_release_created=false
managed_release_created_identity=""
installation_committed=false
current_changed=false
previous_current_target=""
unit_installed=false
previous_unit_exists=false
previous_unit_mode=""
previous_unit_identity=""
previous_unit_sha256=""
legacy_unit_needs_backup=false
created_env=false
created_env_identity=""
created_env_sha256=""
preserve_input_stage=false
declare -a changed_public_paths=()
declare -a previous_public_kinds=()
declare -a previous_public_targets=()
declare -a previous_public_recoveries=()
declare -A interrupted_public_backups=()
legacy_backup_stage=""
lock_create_stage=""
created_autostream_group=false
created_autostream_group_gid=""
created_autostream_group_record=""
preexisting_autostream_group_record=""
created_autostream_user=false
created_autostream_user_uid=""
created_autostream_user_gid=""
created_autostream_user_record=""
autostream_user_rollback_login_ready=false
declare -a temporary_rollback_paths=()
declare -a journaled_directory_paths=()
declare -A directory_journaled=()
declare -A directory_existed=()
declare -A directory_original_identity=()
declare -A directory_original_owner=()
declare -A directory_original_mode=()
declare -A directory_created_identity=()
declare -A directory_retain_if_nonempty=()
declare -A preexisting_legacy_backup_identity=()
declare -A preexisting_legacy_backup_digest=()
declare -a mutated_filesystem_parents=("/var/tmp" "${INPUT_STAGE}")

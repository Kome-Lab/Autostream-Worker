
preflight_autostream_account() {
  local autostream_group_gid=""
  local autostream_user_uid

  if getent group autostream >/dev/null; then
    preexisting_autostream_group_record="$(getent group autostream)" || \
      die "could not capture the pre-existing autostream service group"
    [[ -n ${preexisting_autostream_group_record} ]] || \
      die "could not capture the pre-existing autostream service group"
    autostream_group_gid="$(printf '%s\n' "${preexisting_autostream_group_record}" |
      awk -F: 'NR == 1 { print $3 }')"
    [[ ${autostream_group_gid} =~ ^[0-9]+$ &&
      ${autostream_group_gid} -ne 0 ]] || \
      die "autostream service group must have a non-root numeric GID"
  elif id autostream >/dev/null 2>&1; then
    die "autostream service account exists without the required autostream group"
  fi

  if id autostream >/dev/null 2>&1; then
    [[ -n ${autostream_group_gid} ]] || \
      die "autostream service account exists without a validated primary group"
    autostream_user_uid="$(id -u autostream)"
    [[ ${autostream_user_uid} =~ ^[0-9]+$ &&
      ${autostream_user_uid} -ne 0 ]] || \
      die "autostream service account must not be root"
    [[ $(id -g autostream) == "${autostream_group_gid}" ]] || \
      die "autostream service account numeric primary GID does not match the autostream group"
    [[ $(id -gn autostream) == "autostream" ]] || \
      die "autostream service account must use the autostream primary group"
  else
    prepare_autostream_user_rollback_login || \
      die "could not reserve a safe service-account rollback login"
  fi
}

preflight_existing_host_paths_before_mutation() {
  local path
  local expected_target
  local mode
  local resolved_current

  if [[ -e ${ENV_DEST} || -L ${ENV_DEST} ]]; then
    [[ -f ${ENV_DEST} && ! -L ${ENV_DEST} ]] || \
      die "existing environment file is not a regular file"
    [[ $(stat -c '%U:%G' -- "${ENV_DEST}") == "root:root" ]] || \
      die "existing environment file must be owned by root:root"
    mode="$(stat -c '%a' -- "${ENV_DEST}")"
    [[ ${mode} == "600" || ${mode} == "640" ]] || \
      die "existing environment file mode must be 0600 or 0640"
  fi
  if [[ -e ${UNIT_DEST} || -L ${UNIT_DEST} ]]; then
    [[ -f ${UNIT_DEST} && ! -L ${UNIT_DEST} ]] || \
      die "existing systemd unit is not a regular file"
    [[ $(stat -c '%U:%G' -- "${UNIT_DEST}") == "root:root" ]] || \
      die "existing systemd unit must be owned by root:root"
    mode="$(stat -c '%a' -- "${UNIT_DEST}")"
    (( (8#${mode} & 0022) == 0 )) || \
      die "existing systemd unit must not be group/other writable"
  fi
  while IFS='|' read -r path expected_target; do
    if [[ -L ${path} ]]; then
      [[ $(readlink -- "${path}") == "${expected_target}" ]] || \
        die "existing public link has an unexpected target: ${path}"
    elif [[ -e ${path} ]]; then
      [[ -f ${path} && ! -L ${path} ]] || \
        die "existing public path is not a regular file: ${path}"
      [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
        die "existing public file must be owned by root:root: ${path}"
      mode="$(stat -c '%a' -- "${path}")"
      (( (8#${mode} & 0022) == 0 )) || \
        die "existing public file must not be group/other writable: ${path}"
    fi
  done <<EOF
${PUBLIC_BINARY}|${CURRENT_LINK}/bin/autostream-worker
${PUBLIC_ALIAS}|${PUBLIC_BINARY}
EOF
  [[ ! -e ${CURRENT_LINK} || -L ${CURRENT_LINK} ]] || \
    die "managed current path must be absent or a symlink"
  if [[ -L ${CURRENT_LINK} ]]; then
    resolved_current="$(readlink -f -- "${CURRENT_LINK}")" || \
      die "managed current link is dangling"
    [[ ${resolved_current} == "${RELEASES_DIR}/"* ]] || \
      die "managed current link points outside the release root"
    [[ -d ${resolved_current} && ! -L ${resolved_current} ]] || \
      die "managed current link target is missing or unsafe"
    [[ $(stat -c '%U:%G' -- "${resolved_current}") == "root:root" ]] || \
      die "managed current link target must be owned by root:root"
    [[ -z $(find "${resolved_current}" \( ! -user root -o ! -group root \) -print -quit) ]] || \
      die "managed current release must be entirely owned by root:root"
    [[ -z $(find "${resolved_current}" -perm /022 -print -quit) ]] || \
      die "managed current release must not be group/other writable"
    [[ -z $(find "${resolved_current}" -type l -print -quit) ]] || \
      die "managed current release must not contain symlinks"
  fi
}

preflight_autostream_account
preflight_existing_host_paths_before_mutation

record_mutated_parent /run
ensure_permanent_lock_directory /run/autostream-updater
record_mutated_parent /run/autostream-updater
readonly SHARED_HOST_SETUP_LOCK="/run/autostream-updater/.autostream-runtime-host-setup.lock"
ensure_permanent_lock_path_atomically "${SHARED_HOST_SETUP_LOCK}"
exec 8<>"${SHARED_HOST_SETUP_LOCK}"
chown root:root /proc/self/fd/8
chmod 0600 /proc/self/fd/8
shared_lock_fd_identity="$(stat -Lc '%d:%i' -- /proc/self/fd/8)"
[[ -f ${SHARED_HOST_SETUP_LOCK} && ! -L ${SHARED_HOST_SETUP_LOCK} &&
  -f /proc/self/fd/8 &&
  $(stat -Lc '%U:%G:%a' -- /proc/self/fd/8) == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK}") == "root:root:600" &&
  ${shared_lock_fd_identity} == "$(stat -c '%d:%i' -- "${SHARED_HOST_SETUP_LOCK}")" ]] || \
  die "shared host-setup lock identity changed while being opened"
flock -n 8 || die "another AutoStream installer is provisioning shared host state"
[[ -f ${SHARED_HOST_SETUP_LOCK} && ! -L ${SHARED_HOST_SETUP_LOCK} &&
  -f /proc/self/fd/8 &&
  $(stat -Lc '%U:%G:%a' -- /proc/self/fd/8) == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${SHARED_HOST_SETUP_LOCK}") == "root:root:600" &&
  ${shared_lock_fd_identity} == "$(stat -c '%d:%i' -- "${SHARED_HOST_SETUP_LOCK}")" ]] || \
  die "shared host-setup lock identity changed after acquisition"

target_lock_id=""
if ! target_lock_id="$(
  printf '%s' "${UNIT_NAME}" |
    sha256sum |
    awk 'NR == 1 { print substr($1, 1, 12) }'
)"; then
  die "could not derive the updater target lock identifier"
fi
[[ ${target_lock_id} =~ ^[0-9a-f]{12}$ ]] || \
  die "could not derive the updater target lock identifier"
readonly TARGET_LOCK_ID="${target_lock_id}"
readonly TARGET_LOCK="/run/autostream-updater/.autostream-updater-${TARGET_LOCK_ID}.lock"
ensure_permanent_lock_path_atomically "${TARGET_LOCK}"
exec 9<>"${TARGET_LOCK}"
chown root:root /proc/self/fd/9
chmod 0600 /proc/self/fd/9
target_lock_fd_identity="$(stat -Lc '%d:%i' -- /proc/self/fd/9)"
[[ -f ${TARGET_LOCK} && ! -L ${TARGET_LOCK} &&
  -f /proc/self/fd/9 &&
  $(stat -Lc '%U:%G:%a' -- /proc/self/fd/9) == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${TARGET_LOCK}") == "root:root:600" &&
  ${target_lock_fd_identity} == "$(stat -c '%d:%i' -- "${TARGET_LOCK}")" ]] || \
  die "updater target lock identity changed while being opened"
flock -n 9 || die "another privileged update is already active for ${UNIT_NAME}"
[[ -f ${TARGET_LOCK} && ! -L ${TARGET_LOCK} &&
  -f /proc/self/fd/9 &&
  $(stat -Lc '%U:%G:%a' -- /proc/self/fd/9) == "root:root:600" &&
  $(stat -c '%U:%G:%a' -- "${TARGET_LOCK}") == "root:root:600" &&
  ${target_lock_fd_identity} == "$(stat -c '%d:%i' -- "${TARGET_LOCK}")" ]] || \
  die "updater target lock identity changed after acquisition"

record_mutated_parent /etc
record_mutated_parent /var/lib
defer_termination_signals
if ! getent group autostream >/dev/null; then
  groupadd --system autostream
  created_autostream_group=true
fi
autostream_group_gid="$(getent group autostream | awk -F: 'NR == 1 { print $3 }')"
if [[ ${created_autostream_group} == true ]]; then
  created_autostream_group_gid="${autostream_group_gid}"
  created_autostream_group_record="$(getent group autostream)"
fi
resume_termination_signals
[[ ${autostream_group_gid} =~ ^[0-9]+$ &&
  ${autostream_group_gid} -ne 0 ]] || \
  die "autostream service group must have a non-root numeric GID"
if [[ ${created_autostream_group} == true ]]; then
  [[ ${created_autostream_group_gid} == "${autostream_group_gid}" &&
    $(getent group autostream) == "${created_autostream_group_record}" ]] || \
    die "created autostream service group changed before user provisioning"
else
  [[ -n ${preexisting_autostream_group_record} &&
    $(getent group autostream) == "${preexisting_autostream_group_record}" ]] || \
    die "pre-existing autostream service group changed before user provisioning"
fi
defer_termination_signals
if ! id autostream >/dev/null 2>&1; then
  [[ ${autostream_user_rollback_login_ready} == true &&
    -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    die "service-account rollback login changed before user provisioning"
  local_account_member_fields_are_clear || \
    die "service-account names are referenced by local group membership state"
  useradd --system --gid "${autostream_group_gid}" --home-dir /var/lib/autostream \
    --no-create-home --shell /usr/sbin/nologin autostream
  created_autostream_user=true
  created_autostream_user_uid="$(id -u autostream)"
  created_autostream_user_gid="$(id -g autostream)"
  created_autostream_user_record="$(getent passwd autostream)"
fi
resume_termination_signals
[[ $(id -u autostream) -ne 0 ]] || die "autostream service account must not be root"
[[ $(id -g autostream) == "${autostream_group_gid}" ]] || \
  die "autostream service account numeric primary GID does not match the autostream group"
[[ $(id -gn autostream) == "autostream" ]] || \
  die "autostream service account must use the autostream primary group"

[[ ! -L /opt/autostream ]] || die "/opt/autostream must not be a symlink"
record_mutated_parent /opt
ensure_root_directory /opt/autostream 0755
[[ ! -L ${MANAGED_ROOT} ]] || die "managed root must not be a symlink"
[[ ! -L ${RELEASES_DIR} ]] || die "managed releases path must not be a symlink"
record_mutated_parent /opt/autostream
record_mutated_parent "${MANAGED_ROOT}"
ensure_root_directory "${MANAGED_ROOT}" 0755
ensure_root_directory "${RELEASES_DIR}" 0755
record_mutated_parent "${RELEASES_DIR}"
[[ ! -L /var/lib/autostream ]] || die "/var/lib/autostream must not be a symlink"
record_mutated_parent /var/lib
ensure_root_directory /var/lib/autostream 0755
verify_existing_worker_state_directory
verify_state_directory_snapshot
record_mutated_parent /var/lib/autostream
state_dir_mutated=true
if [[ ${state_dir_existed} == true ]]; then
  install -d -o autostream -g autostream -m 0750 "${STATE_DIR}"
else
  defer_termination_signals
  mkdir -- "${STATE_DIR}"
  state_dir_created_identity="$(stat -c '%d:%i' -- "${STATE_DIR}")"
  resume_termination_signals
  chown autostream:autostream "${STATE_DIR}"
  chmod 0750 "${STATE_DIR}"
fi
[[ $(stat -c '%U:%G:%a' -- "${STATE_DIR}") == "autostream:autostream:750" ]] || \
  die "Worker state path ownership or mode is invalid"
record_mutated_parent /var
ensure_root_directory /var/backups 0755 true
record_mutated_parent /var/backups
ensure_root_directory /var/backups/autostream 0700 true
record_mutated_parent /var/backups/autostream
ensure_root_directory "${INSTALL_MIGRATION_ROOT}" 0700 true
record_mutated_parent "${INSTALL_MIGRATION_ROOT}"
ensure_root_directory "${LEGACY_BACKUP_ROOT}" 0700 true
record_mutated_parent "${LEGACY_BACKUP_ROOT}"
LEGACY_BACKUP_ROOT_IDENTITY=$(
  stat -c '%d:%i:%u:%g:%a' -- "${LEGACY_BACKUP_ROOT}"
) || die "could not capture the legacy backup root identity"
[[ ${LEGACY_BACKUP_ROOT_IDENTITY} =~ ^[0-9]+:[0-9]+:[0-9]+:[0-9]+:[0-7]+$ ]] || \
  die "legacy backup root identity is invalid"
readonly LEGACY_BACKUP_ROOT_IDENTITY

preflight_public_path() {
  local link_path=$1
  local expected_target=$2
  local backup_name
  local backup_path
  local parent
  local mode
  local source_digest

  backup_name=$(basename -- "${link_path}") || \
    die "could not resolve the public path name"
  [[ -n ${backup_name} && ${backup_name} != */* ]] || \
    die "public path name is empty or unsafe"
  backup_path="${LEGACY_BACKUP_DIR}/${backup_name}"
  verify_legacy_backup_root
  parent="$(dirname -- "${link_path}")"
  verify_root_permission_boundary "${parent}"
  if [[ -L ${link_path} ]]; then
    [[ $(readlink -- "${link_path}") == "${expected_target}" ]] || \
      die "existing public link has an unexpected target: ${link_path}"
    if [[ -e ${backup_path} || -L ${backup_path} ]]; then
      snapshot_preexisting_legacy_backup "${backup_path}"
    fi
  elif [[ -e ${link_path} ]]; then
    [[ -f ${link_path} && ! -L ${link_path} ]] || \
      die "existing public path is not a regular file: ${link_path}"
    [[ $(stat -c '%U:%G' -- "${link_path}") == "root:root" ]] || \
      die "existing public file must be owned by root:root: ${link_path}"
    mode="$(stat -c '%a' -- "${link_path}")"
    (( (8#${mode} & 0022) == 0 )) || \
      die "existing public file must not be group/other writable: ${link_path}"
    if [[ -e ${backup_path} || -L ${backup_path} ]]; then
      source_digest="$(sha256sum -- "${link_path}" | awk 'NR == 1 { print $1 }')"
      snapshot_preexisting_legacy_backup "${backup_path}" "${source_digest}"
    fi
  elif [[ -e ${backup_path} || -L ${backup_path} ]]; then
    snapshot_preexisting_legacy_backup "${backup_path}"
    interrupted_public_backups["${link_path}"]=true
  fi
}

preflight_public_path "${PUBLIC_BINARY}" "${CURRENT_LINK}/bin/autostream-worker"
preflight_public_path "${PUBLIC_ALIAS}" "${PUBLIC_BINARY}"

[[ ! -e ${CURRENT_LINK} || -L ${CURRENT_LINK} ]] || \
  die "managed current path must be absent or a symlink"
if [[ -L ${CURRENT_LINK} ]]; then
  previous_current_target="$(readlink -- "${CURRENT_LINK}")"
  resolved_previous_current="$(readlink -f -- "${CURRENT_LINK}")" || \
    die "managed current link is dangling"
  [[ ${resolved_previous_current} == "${RELEASES_DIR}/"* ]] || \
    die "managed current link points outside the release root"
  [[ -d ${resolved_previous_current} && ! -L ${resolved_previous_current} ]] || \
    die "managed current link target is missing or unsafe"
  [[ $(stat -c '%U:%G' -- "${resolved_previous_current}") == "root:root" ]] || \
    die "managed current link target must be owned by root:root"
  [[ -z $(find "${resolved_previous_current}" \( ! -user root -o ! -group root \) -print -quit) ]] || \
    die "managed current release must be entirely owned by root:root"
  [[ -z $(find "${resolved_previous_current}" -perm /022 -print -quit) ]] || \
    die "managed current release must not be group/other writable"
  [[ -z $(find "${resolved_previous_current}" -type l -print -quit) ]] || \
    die "managed current release must not contain symlinks"
  for marker in .artifact-sha256 .version checksums.txt; do
    [[ -f ${resolved_previous_current}/${marker} &&
      ! -L ${resolved_previous_current}/${marker} ]] || \
      die "managed current release marker is missing or unsafe: ${marker}"
  done
  [[ $(stat -c '%U:%G:%a' -- "${resolved_previous_current}/.artifact-sha256") == "root:root:444" ]] || \
    die "managed current release digest marker ownership or mode is invalid"
  [[ $(stat -c '%U:%G:%a' -- "${resolved_previous_current}/.version") == "root:root:444" ]] || \
    die "managed current release version marker ownership or mode is invalid"
  previous_sha256="$(< "${resolved_previous_current}/.artifact-sha256")"
  previous_version="$(< "${resolved_previous_current}/.version")"
  [[ ${previous_sha256} =~ ^[0-9a-f]{64}$ ]] || \
    die "managed current release digest marker is invalid"
  [[ ${previous_version} =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || \
    die "managed current release version marker is invalid"
  [[ $(basename -- "${resolved_previous_current}") == \
    "${previous_version}-${previous_sha256:0:12}" ]] || \
    die "managed current release directory does not match its markers"
  while IFS= read -r checksum_entry; do
    [[ ${checksum_entry} =~ ^[0-9a-f]{64}\ \ (.+)$ ]] || \
      die "managed current inner checksum entry has an invalid format"
    checked_path="${BASH_REMATCH[1]}"
    [[ ${checked_path} == ./* ]] || \
      die "managed current inner checksum entry must use a ./ relative path"
    normalized_checked_path="${checked_path#./}"
    [[ -n ${normalized_checked_path} &&
      ${normalized_checked_path} != /* &&
      ${normalized_checked_path} != ./* &&
      ${normalized_checked_path} != *\\* &&
      ${normalized_checked_path} != ../* &&
      ${normalized_checked_path} != *"/../"* &&
      ${normalized_checked_path} != *"/.." &&
      ${normalized_checked_path} != *"/./"* &&
      ${normalized_checked_path} != *"/." &&
      ${normalized_checked_path} != *"//"* &&
      ${normalized_checked_path} != "checksums.txt" ]] || \
      die "managed current inner checksum entry has an unsafe path"
    [[ -f ${resolved_previous_current}/${checked_path} &&
      ! -L ${resolved_previous_current}/${checked_path} ]] || \
      die "managed current inner checksum entry does not name a regular file"
  done < "${resolved_previous_current}/checksums.txt"
  (cd -- "${resolved_previous_current}" && sha256sum --check --strict checksums.txt)
fi
record_mutated_parent "${MANAGED_ROOT}"
current_next="${MANAGED_ROOT}/.current.next.$$"
[[ ! -e ${current_next} && ! -L ${current_next} ]] || \
  die "managed current staging path already exists"
register_temporary_rollback_path "${current_next}"
ln -s -- "${RELEASE_DIR}" "${current_next}"
defer_termination_signals
mv -Tf -- "${current_next}" "${CURRENT_LINK}"
current_changed=true
resume_termination_signals
sync -f "${MANAGED_ROOT}"
[[ $(readlink -f -- "${CURRENT_LINK}") == "${RELEASE_DIR}" ]] || \
  die "managed current link verification failed"

if [[ -z ${existing_env_identity} ]]; then
  record_mutated_parent /etc/autostream
  env_next="${ENV_DEST}.next.$$"
  [[ ! -e ${env_next} && ! -L ${env_next} ]] || \
    die "environment staging path already exists"
  register_temporary_rollback_path "${env_next}"
  install -o root -g root -m 0640 "${RELEASE_DIR}/.env.example" "${env_next}"
  sync -f /etc/autostream
  [[ ! -e ${ENV_DEST} && ! -L ${ENV_DEST} ]] || \
    die "environment file appeared during installation"
  defer_termination_signals
  mv -T -- "${env_next}" "${ENV_DEST}"
  created_env=true
  created_env_identity="$(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${ENV_DEST}")"
  created_env_sha256="$(sha256sum -- "${ENV_DEST}" | awk 'NR == 1 { print $1 }')"
  resume_termination_signals
  sync -f /etc/autostream
fi

if [[ ${previous_unit_exists} == true ]]; then
  [[ -f ${UNIT_DEST} && ! -L ${UNIT_DEST} ]] || \
    die "existing systemd unit changed during installation"
  [[ $(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${UNIT_DEST}") == "${previous_unit_identity}" ]] || \
    die "existing systemd unit identity changed during installation"
  [[ $(sha256sum -- "${UNIT_DEST}" | awk 'NR == 1 { print $1 }') == "${previous_unit_sha256}" ]] || \
    die "existing systemd unit content changed during installation"
fi
if [[ ${legacy_unit_needs_backup} == true ]]; then
  verify_legacy_backup_root
  ensure_root_directory "${LEGACY_BACKUP_DIR}" 0700 true
  ensure_durable_legacy_backup \
    "${INPUT_STAGE}/previous-autostream-worker.service" \
    "${LEGACY_UNIT_BACKUP}" \
    "${previous_unit_mode}"
fi
record_mutated_parent /etc/systemd/system
unit_next="${UNIT_DEST}.next.$$"
[[ ! -e ${unit_next} && ! -L ${unit_next} ]] || \
  die "systemd unit staging path already exists"
register_temporary_rollback_path "${unit_next}"
install -o root -g root -m 0644 \
  "${RELEASE_DIR}/systemd/autostream-worker.service.example" \
  "${unit_next}"
sync -f /etc/systemd/system
if [[ ${previous_unit_exists} != true ]]; then
  [[ ! -e ${UNIT_DEST} && ! -L ${UNIT_DEST} ]] || \
    die "systemd unit appeared during installation"
fi
defer_termination_signals
mv -Tf -- "${unit_next}" "${UNIT_DEST}"
unit_installed=true
resume_termination_signals
sync -f /etc/systemd/system

install_public_link() {
  local target=$1
  local link_path=$2
  local backup_name
  local backup_path
  local link_next="${link_path}.next.$$"
  local previous_kind
  local previous_target=""
  local previous_recovery=""
  local previous_mode=""
  local source_identity
  local source_digest

  backup_name=$(basename -- "${link_path}") || \
    die "could not resolve the public path name"
  [[ -n ${backup_name} && ${backup_name} != */* ]] || \
    die "public path name is empty or unsafe"
  backup_path="${LEGACY_BACKUP_DIR}/${backup_name}"
  if [[ -L ${link_path} ]]; then
    previous_kind="symlink"
    previous_target="$(readlink -- "${link_path}")"
  elif [[ -e ${link_path} ]]; then
    previous_kind="regular"
    previous_recovery="${INPUT_STAGE}/legacy-public-$(basename -- "${link_path}")"
    source_identity="$(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${link_path}")"
    source_digest="$(sha256sum -- "${link_path}" | awk 'NR == 1 { print $1 }')"
    previous_mode="$(stat -c '%a' -- "${link_path}")"
    cp -a -- "${link_path}" "${previous_recovery}"
    [[ -f ${link_path} &&
      ! -L ${link_path} &&
      $(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${link_path}") == "${source_identity}" &&
      $(sha256sum -- "${link_path}" | awk 'NR == 1 { print $1 }') == "${source_digest}" &&
      -f ${previous_recovery} &&
      ! -L ${previous_recovery} &&
      $(sha256sum -- "${previous_recovery}" | awk 'NR == 1 { print $1 }') == "${source_digest}" ]] || \
      die "legacy public binary changed while being staged: ${link_path}"
  elif [[ ${interrupted_public_backups["${link_path}"]-false} == true ]]; then
    previous_kind="regular"
    previous_recovery="${INPUT_STAGE}/legacy-public-$(basename -- "${link_path}")"
    verify_legacy_backup_file "${backup_path}"
    previous_mode="$(stat -c '%a' -- "${backup_path}")"
    source_digest="$(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }')"
    cp -a -- "${backup_path}" "${previous_recovery}"
    [[ -f ${previous_recovery} && ! -L ${previous_recovery} &&
      $(sha256sum -- "${previous_recovery}" | awk 'NR == 1 { print $1 }') == \
        "${source_digest}" ]] || \
      die "interrupted legacy backup changed while being staged: ${backup_path}"
    printf 'install-autostream-worker: resuming interrupted public-path migration from verified backup: %s\n' \
      "${backup_path}" >&2
  else
    previous_kind="absent"
  fi

  defer_termination_signals
  changed_public_paths+=("${link_path}")
  previous_public_kinds+=("${previous_kind}")
  previous_public_targets+=("${previous_target}")
  previous_public_recoveries+=("${previous_recovery}")
  resume_termination_signals

  record_mutated_parent "$(dirname -- "${link_path}")"
  if [[ ${previous_kind} == "regular" && -e ${link_path} ]]; then
    ensure_durable_legacy_backup \
      "${link_path}" \
      "${backup_path}" \
      "${previous_mode}"
  fi
  [[ ! -e ${link_next} && ! -L ${link_next} ]] || \
    die "public link staging path already exists: ${link_next}"
  register_temporary_rollback_path "${link_next}"
  ln -s -- "${target}" "${link_next}"
  mv -Tf -- "${link_next}" "${link_path}"
  sync -f "$(dirname -- "${link_path}")"
}

install_public_link "${CURRENT_LINK}/bin/autostream-worker" "${PUBLIC_BINARY}"
install_public_link "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}"
[[ $(readlink -f -- "${PUBLIC_BINARY}") == "${RELEASE_DIR}/bin/autostream-worker" ]] || \
  die "public Worker binary link verification failed"
[[ $(readlink -f -- "${PUBLIC_ALIAS}") == "${RELEASE_DIR}/bin/autostream-worker" ]] || \
  die "public Worker alias link verification failed"

if [[ -n ${existing_env_identity} ]]; then
  [[ -f ${ENV_DEST} && ! -L ${ENV_DEST} ]] || \
    die "existing environment file changed during installation"
  [[ $(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${ENV_DEST}") == "${existing_env_identity}" ]] || \
    die "existing environment file identity changed during installation"
  [[ $(sha256sum -- "${ENV_DEST}" | awk 'NR == 1 { print $1 }') == "${existing_env_sha256}" ]] || \
    die "existing environment file content changed during installation"
fi

verify_preexisting_legacy_backups
sync_installation_parents
systemctl daemon-reload
installation_committed=true

printf 'Installed AutoStream Worker %s.\n' "${VERSION}"
printf 'Existing settings were preserved. Review %s, then run:\n' "${ENV_DEST}"
if [[ ${service_was_active} == true ]] || systemctl is-active --quiet "${UNIT_NAME}"; then
  printf '  sudo systemctl restart %s\n' "${UNIT_NAME}"
else
  printf '  sudo systemctl enable --now %s\n' "${UNIT_NAME}"
fi
printf '  sudo systemctl status %s\n' "${UNIT_NAME}"

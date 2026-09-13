
verify_legacy_backup_root() {
  [[ -d ${LEGACY_BACKUP_ROOT} && ! -L ${LEGACY_BACKUP_ROOT} ]] || \
    die "legacy backup root changed during installation"
  [[ $(stat -c '%d:%i:%u:%g:%a' -- "${LEGACY_BACKUP_ROOT}") == \
    "${LEGACY_BACKUP_ROOT_IDENTITY}" ]] || \
    die "legacy backup root identity changed during installation"
}

verify_legacy_backup_file() {
  local backup_path=$1
  local expected_digest=${2-}
  local backup_mode

  [[ -f ${backup_path} && ! -L ${backup_path} ]] || \
    die "recoverable legacy backup conflict at ${backup_path}: expected a regular root-owned file; inspect and move the conflicting path aside, then retry"
  [[ $(stat -c '%U:%G' -- "${backup_path}") == "root:root" ]] || \
    die "recoverable legacy backup conflict at ${backup_path}: ownership is not root:root; inspect and repair or move it aside, then retry"
  backup_mode="$(stat -c '%a' -- "${backup_path}")"
  (( (8#${backup_mode} & 0022) == 0 )) || \
    die "recoverable legacy backup conflict at ${backup_path}: file is group/other writable; inspect and repair or move it aside, then retry"
  if [[ -n ${expected_digest} &&
    $(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }') != "${expected_digest}" ]]; then
    die "recoverable legacy backup conflict at ${backup_path}: source and backup differ; preserve both files and move the conflicting backup aside after inspection, then retry"
  fi
  if [[ ${preexisting_legacy_backup_identity["${backup_path}"]+present} == present ]]; then
    [[ $(stat -c '%d:%i:%s:%Y:%Z:%f:%u:%g:%a' -- "${backup_path}") == \
      "${preexisting_legacy_backup_identity["${backup_path}"]}" ]] || \
      die "pre-existing legacy backup identity or metadata changed during installation: ${backup_path}"
    [[ $(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }') == \
      "${preexisting_legacy_backup_digest["${backup_path}"]}" ]] || \
      die "pre-existing legacy backup content changed during installation: ${backup_path}"
  fi
}

snapshot_preexisting_legacy_backup() {
  local backup_path=$1
  local expected_digest=${2-}

  [[ ${preexisting_legacy_backup_identity["${backup_path}"]+present} != present ]] || {
    verify_legacy_backup_file "${backup_path}" "${expected_digest}"
    return 0
  }
  verify_legacy_backup_file "${backup_path}" "${expected_digest}"
  defer_termination_signals
  preexisting_legacy_backup_identity["${backup_path}"]="$(stat -c '%d:%i:%s:%Y:%Z:%f:%u:%g:%a' -- "${backup_path}")"
  preexisting_legacy_backup_digest["${backup_path}"]="$(sha256sum -- "${backup_path}" | awk 'NR == 1 { print $1 }')"
  resume_termination_signals
}

ensure_durable_legacy_backup() {
  local source=$1
  local backup_path=$2
  local source_mode=$3
  local source_digest

  [[ -f ${source} && ! -L ${source} ]] || \
    die "legacy backup source is missing or unsafe: ${source}"
  source_digest="$(sha256sum -- "${source}" | awk 'NR == 1 { print $1 }')"
  if [[ -e ${backup_path} || -L ${backup_path} ]]; then
    verify_legacy_backup_file "${backup_path}" "${source_digest}"
    return 0
  fi

  verify_legacy_backup_root
  ensure_root_directory "$(dirname -- "${backup_path}")" 0700 true
  record_mutated_parent "$(dirname -- "${backup_path}")"
  legacy_backup_stage="${backup_path}.next.$$"
  [[ ! -e ${legacy_backup_stage} && ! -L ${legacy_backup_stage} ]] || \
    die "legacy backup staging path already exists: ${legacy_backup_stage}"
  register_temporary_rollback_path "${legacy_backup_stage}"
  install -o root -g root -m "${source_mode}" "${source}" "${legacy_backup_stage}"
  [[ -f ${legacy_backup_stage} && ! -L ${legacy_backup_stage} &&
    $(stat -c '%U:%G:%a' -- "${legacy_backup_stage}") == "root:root:${source_mode}" &&
    $(sha256sum -- "${legacy_backup_stage}" | awk 'NR == 1 { print $1 }') == \
      "${source_digest}" ]] || \
    die "staged legacy backup verification failed: ${backup_path}"
  sync -f "$(dirname -- "${backup_path}")"
  mv -T -- "${legacy_backup_stage}" "${backup_path}"
  legacy_backup_stage=""
  sync -f "$(dirname -- "${backup_path}")"
  verify_legacy_backup_file "${backup_path}" "${source_digest}"
}

readonly RELEASE_DIR="${RELEASES_DIR}/${VERSION}-${ARTIFACT_SHA256:0:12}"
write_tree_inventory() {
  local root=$1
  local output=$2
  (
    cd -- "${root}"
    find . -mindepth 1 \
      ! -path './.artifact-sha256' \
      ! -path './.version' \
      -printf '%y %P\n' |
      sort
  ) > "${output}"
}

verify_managed_release() {
  local release_dir=$1

  [[ -d ${release_dir} && ! -L ${release_dir} ]] || \
    die "managed release is missing or unsafe: ${release_dir}"
  [[ -f ${release_dir}/.artifact-sha256 && ! -L ${release_dir}/.artifact-sha256 ]] || \
    die "managed release digest marker is missing or unsafe"
  [[ -f ${release_dir}/.version && ! -L ${release_dir}/.version ]] || \
    die "managed release version marker is missing or unsafe"
  [[ $(stat -c '%U:%G:%a' -- "${release_dir}/.artifact-sha256") == "root:root:444" ]] || \
    die "managed release digest marker ownership or mode is invalid"
  [[ $(stat -c '%U:%G:%a' -- "${release_dir}/.version") == "root:root:444" ]] || \
    die "managed release version marker ownership or mode is invalid"
  [[ $(< "${release_dir}/.artifact-sha256") == "${ARTIFACT_SHA256}" ]] || \
    die "managed release digest marker differs from the verified artifact"
  [[ $(< "${release_dir}/.version") == "${VERSION}" ]] || \
    die "managed release version marker differs from the verified artifact"
  [[ -z $(find "${release_dir}" \( ! -user root -o ! -group root \) -print -quit) ]] || \
    die "managed release must be entirely owned by root:root"
  [[ -z $(find "${release_dir}" -perm /022 -print -quit) ]] || \
    die "managed release must not be group/other writable"
  [[ -z $(find "${release_dir}" -type l -print -quit) ]] || \
    die "managed release must not contain symlinks"
  cmp -s "${EXTRACTED_ROOT}/checksums.txt" "${release_dir}/checksums.txt" || \
    die "managed release checksums differ from the verified artifact"
  write_tree_inventory "${EXTRACTED_ROOT}" "${INPUT_STAGE}/expected-tree"
  write_tree_inventory "${release_dir}" "${INPUT_STAGE}/managed-tree"
  cmp -s "${INPUT_STAGE}/expected-tree" "${INPUT_STAGE}/managed-tree" || \
    die "managed release file tree differs from the verified artifact"
  (cd -- "${release_dir}" && sha256sum --check --strict checksums.txt)
  [[ -x ${release_dir}/bin/autostream-worker ]] || \
    die "managed Worker binary is not executable"
}

if [[ -e ${RELEASE_DIR} || -L ${RELEASE_DIR} ]]; then
  verify_managed_release "${RELEASE_DIR}"
else
  defer_termination_signals
  if ! managed_candidate="$(mktemp -d "${RELEASES_DIR}/.install-${VERSION}.XXXXXXXX")"; then
    die "failed to create managed release staging directory"
  fi
  [[ -n ${managed_candidate} &&
    ${managed_candidate} == "${RELEASES_DIR}/.install-${VERSION}."* &&
    $(dirname -- "${managed_candidate}") == "${RELEASES_DIR}" &&
    -d ${managed_candidate} && ! -L ${managed_candidate} &&
    $(readlink -f -- "${managed_candidate}") == "${managed_candidate}" ]] || \
    die "mktemp returned an unsafe managed release staging directory"
  resume_termination_signals
  cp -a --no-preserve=ownership -- "${EXTRACTED_ROOT}/." "${managed_candidate}/"
  chown -R root:root "${managed_candidate}"
  chmod 0755 "${managed_candidate}"
  printf '%s\n' "${ARTIFACT_SHA256}" > "${managed_candidate}/.artifact-sha256"
  printf '%s\n' "${VERSION}" > "${managed_candidate}/.version"
  chmod 0444 "${managed_candidate}/.artifact-sha256" "${managed_candidate}/.version"
  (cd -- "${managed_candidate}" && sha256sum --check --strict checksums.txt)
  sync -f "${managed_candidate}"
  defer_termination_signals
  managed_release_created=true
  mv -T -- "${managed_candidate}" "${RELEASE_DIR}"
  managed_release_created_identity="$(stat -c '%d:%i' -- "${RELEASE_DIR}")"
  managed_candidate=""
  resume_termination_signals
  sync -f "${RELEASES_DIR}"
  verify_managed_release "${RELEASE_DIR}"
fi
record_mutated_parent "${RELEASE_DIR}"
MANAGED_BINARY_IDENTITY="$(
  runuser -u autostream -- "${RELEASE_DIR}/bin/autostream-worker" --version
)" || die "managed Worker binary is not runnable by autostream"
readonly MANAGED_BINARY_IDENTITY
verify_worker_binary_identity "${MANAGED_BINARY_IDENTITY}" "managed Worker binary"

[[ ! -L /etc/autostream ]] || die "/etc/autostream must not be a symlink"
record_mutated_parent /etc
ensure_root_directory /etc/autostream 0750
record_mutated_parent /etc/autostream
existing_env_identity=""
existing_env_sha256=""
if [[ -e ${ENV_DEST} || -L ${ENV_DEST} ]]; then
  [[ -f ${ENV_DEST} && ! -L ${ENV_DEST} ]] || \
    die "existing environment file is not a regular file"
  [[ $(stat -c '%U:%G' -- "${ENV_DEST}") == "root:root" ]] || \
    die "existing environment file must be owned by root:root"
  env_mode="$(stat -c '%a' -- "${ENV_DEST}")"
  [[ ${env_mode} == "600" || ${env_mode} == "640" ]] || \
    die "existing environment file mode must be 0600 or 0640"
  existing_env_identity="$(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${ENV_DEST}")"
  existing_env_sha256="$(sha256sum -- "${ENV_DEST}" | awk 'NR == 1 { print $1 }')"
fi

readonly LEGACY_BACKUP_DIR="${LEGACY_BACKUP_ROOT}/${VERSION}-${ARTIFACT_SHA256:0:12}"
verify_existing_root_permission_boundary "${LEGACY_BACKUP_DIR}"
record_mutated_parent "${LEGACY_BACKUP_ROOT}"
ensure_root_directory "${LEGACY_BACKUP_DIR}" 0700 true
record_mutated_parent "${LEGACY_BACKUP_DIR}"
readonly LEGACY_UNIT_BACKUP="${LEGACY_BACKUP_DIR}/autostream-worker.service"
if [[ -e ${UNIT_DEST} || -L ${UNIT_DEST} ]]; then
  [[ -f ${UNIT_DEST} && ! -L ${UNIT_DEST} ]] || \
    die "existing systemd unit is not a regular file"
  [[ $(stat -c '%U:%G' -- "${UNIT_DEST}") == "root:root" ]] || \
    die "existing systemd unit must be owned by root:root"
  previous_unit_mode="$(stat -c '%a' -- "${UNIT_DEST}")"
  (( (8#${previous_unit_mode} & 0022) == 0 )) || \
    die "existing systemd unit must not be group/other writable"
  previous_unit_identity="$(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${UNIT_DEST}")"
  previous_unit_sha256="$(sha256sum -- "${UNIT_DEST}" | awk 'NR == 1 { print $1 }')"
  copy_stable_source \
    "${UNIT_DEST}" \
    "${INPUT_STAGE}/previous-autostream-worker.service"
  previous_unit_exists=true
  if ! cmp -s \
    "${INPUT_STAGE}/previous-autostream-worker.service" \
    "${RELEASE_DIR}/systemd/autostream-worker.service.example"; then
    legacy_unit_needs_backup=true
  fi
fi
if [[ -e ${LEGACY_UNIT_BACKUP} || -L ${LEGACY_UNIT_BACKUP} ]]; then
  if [[ ${legacy_unit_needs_backup} == true ]]; then
    snapshot_preexisting_legacy_backup "${LEGACY_UNIT_BACKUP}" "${previous_unit_sha256}"
  else
    snapshot_preexisting_legacy_backup "${LEGACY_UNIT_BACKUP}"
  fi
fi

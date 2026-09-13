
register_temporary_rollback_path() {
  local path=$1
  local recorded

  [[ -n ${path} && ${path} == /* ]] || \
    die "temporary rollback path is empty or not absolute"
  for recorded in "${temporary_rollback_paths[@]}"; do
    [[ ${recorded} == "${path}" ]] && return 0
  done
  temporary_rollback_paths+=("${path}")
}

journal_root_directory_before_mutation() {
  local path=$1
  local retain_if_nonempty=${2-false}

  [[ ${directory_journaled["${path}"]-false} != true ]] || return 0
  directory_journaled["${path}"]=true
  directory_retain_if_nonempty["${path}"]="${retain_if_nonempty}"
  journaled_directory_paths+=("${path}")
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} && ! -L ${path} &&
      $(readlink -f -- "${path}") == "${path}" ]] || \
      die "root-owned directory path is unsafe before mutation: ${path}"
    directory_existed["${path}"]=true
    directory_original_identity["${path}"]="$(stat -c '%d:%i' -- "${path}")"
    directory_original_owner["${path}"]="$(stat -c '%u:%g' -- "${path}")"
    directory_original_mode["${path}"]="$(stat -c '%a' -- "${path}")"
  else
    directory_existed["${path}"]=false
  fi
}

record_mutated_parent() {
  local parent=$1
  local recorded

  for recorded in "${mutated_filesystem_parents[@]}"; do
    [[ ${recorded} == "${parent}" ]] && return 0
  done
  mutated_filesystem_parents+=("${parent}")
}

sync_installation_parents() {
  local parent

  for parent in "${mutated_filesystem_parents[@]}"; do
    [[ -d ${parent} && ! -L ${parent} ]] || \
      die "mutated filesystem parent is missing or unsafe before commit: ${parent}"
    sync -f "${parent}" || \
      die "failed to sync filesystem parent before commit: ${parent}"
  done
}

best_effort_sync_mutated_parents() {
  local parent
  local failed=false

  for parent in "${mutated_filesystem_parents[@]}"; do
    if [[ -d ${parent} && ! -L ${parent} ]]; then
      sync -f "${parent}" || failed=true
    fi
  done
  [[ ${failed} == false ]]
}

rollback_public_links() {
  local index
  local path
  local kind
  local target
  local recovery
  local rollback_next
  local expected_digest
  local expected_metadata
  local failed=false

  for ((index=${#changed_public_paths[@]} - 1; index >= 0; index--)); do
    path="${changed_public_paths[index]}"
    kind="${previous_public_kinds[index]}"
    target="${previous_public_targets[index]}"
    recovery="${previous_public_recoveries[index]}"
    case "${kind}" in
      regular)
        if [[ ! -f ${recovery} || -L ${recovery} ]]; then
          failed=true
          continue
        fi
        expected_digest="$(sha256sum -- "${recovery}" | awk 'NR == 1 { print $1 }')"
        expected_metadata="$(stat -c '%u:%g:%a:%s' -- "${recovery}")"
        rollback_next="${path}.rollback.$$"
        if [[ -e ${rollback_next} || -L ${rollback_next} ]]; then
          failed=true
          continue
        fi
        register_temporary_rollback_path "${rollback_next}"
        if cp -a -- "${recovery}" "${rollback_next}"; then
          mv -Tf -- "${rollback_next}" "${path}" || failed=true
        else
          failed=true
        fi
        [[ -f ${path} &&
          ! -L ${path} &&
          $(stat -c '%u:%g:%a:%s' -- "${path}") == "${expected_metadata}" &&
          $(sha256sum -- "${path}" | awk 'NR == 1 { print $1 }') == "${expected_digest}" ]] || \
          failed=true
        ;;
      symlink)
        rollback_next="${path}.rollback.$$"
        if [[ -e ${rollback_next} || -L ${rollback_next} ]]; then
          failed=true
          continue
        fi
        register_temporary_rollback_path "${rollback_next}"
        ln -s -- "${target}" "${rollback_next}" || failed=true
        if [[ -L ${rollback_next} ]]; then
          mv -Tf -- "${rollback_next}" "${path}" || failed=true
        fi
        [[ -L ${path} && $(readlink -- "${path}") == "${target}" ]] || failed=true
        ;;
      absent)
        rm -f -- "${path}" || failed=true
        [[ ! -e ${path} && ! -L ${path} ]] || failed=true
        ;;
    esac
    sync -f "$(dirname -- "${path}")" || failed=true
  done
  [[ ${failed} == false ]]
}

rollback_current_link() {
  local rollback_next
  local failed=false

  [[ ${current_changed} == true ]] || return 0
  if [[ -n ${previous_current_target} ]]; then
    rollback_next="${MANAGED_ROOT}/.current.rollback.$$"
    [[ ! -e ${rollback_next} && ! -L ${rollback_next} ]] || return 1
    register_temporary_rollback_path "${rollback_next}"
    ln -s -- "${previous_current_target}" "${rollback_next}" || failed=true
    if [[ -L ${rollback_next} ]]; then
      mv -Tf -- "${rollback_next}" "${CURRENT_LINK}" || failed=true
    fi
    [[ -L ${CURRENT_LINK} &&
      $(readlink -- "${CURRENT_LINK}") == "${previous_current_target}" ]] || \
      failed=true
  else
    rm -f -- "${CURRENT_LINK}" || failed=true
    [[ ! -e ${CURRENT_LINK} && ! -L ${CURRENT_LINK} ]] || failed=true
  fi
  sync -f "${MANAGED_ROOT}" || failed=true
  [[ ${failed} == false ]]
}

rollback_unit_and_environment() {
  local unit_rollback_next
  local unit_recovered=true
  local failed=false

  if [[ ${unit_installed} != true &&
    ${previous_unit_exists} != true &&
    ${created_env} != true ]]; then
    return 0
  fi

  if [[ ${unit_installed} == true ]]; then
    if [[ ${previous_unit_exists} == true ]]; then
      unit_recovered=false
      unit_rollback_next="${UNIT_DEST}.rollback.$$"
      if [[ ! -e ${unit_rollback_next} && ! -L ${unit_rollback_next} ]]; then
        register_temporary_rollback_path "${unit_rollback_next}"
      else
        failed=true
      fi
      if [[ ${failed} == false &&
        -f ${INPUT_STAGE}/previous-autostream-worker.service &&
        ! -L ${INPUT_STAGE}/previous-autostream-worker.service ]] &&
        install -o root -g root -m "${previous_unit_mode}" \
          "${INPUT_STAGE}/previous-autostream-worker.service" \
          "${unit_rollback_next}" &&
        mv -Tf -- "${unit_rollback_next}" "${UNIT_DEST}" &&
        [[ -f ${UNIT_DEST} &&
          ! -L ${UNIT_DEST} &&
          $(stat -c '%U:%G:%a' -- "${UNIT_DEST}") == \
            "root:root:${previous_unit_mode}" &&
          $(sha256sum -- "${UNIT_DEST}" | awk 'NR == 1 { print $1 }') == \
            "${previous_unit_sha256}" ]]; then
        unit_recovered=true
      else
        failed=true
      fi
    else
      rm -f -- "${UNIT_DEST}" || failed=true
      [[ ! -e ${UNIT_DEST} && ! -L ${UNIT_DEST} ]] || {
        unit_recovered=false
        failed=true
      }
    fi
    if [[ ${unit_recovered} == true ]]; then
      systemctl daemon-reload >/dev/null 2>&1 || true
    fi
  elif [[ ${previous_unit_exists} == true ]]; then
    [[ -f ${UNIT_DEST} &&
      ! -L ${UNIT_DEST} &&
      $(stat -c '%U:%G:%a' -- "${UNIT_DEST}") == \
        "root:root:${previous_unit_mode}" &&
      $(sha256sum -- "${UNIT_DEST}" | awk 'NR == 1 { print $1 }') == \
        "${previous_unit_sha256}" ]] || {
      unit_recovered=false
      failed=true
    }
  fi
  sync -f /etc/systemd/system || failed=true

  if [[ ${created_env} == true ]]; then
    if [[ -f ${ENV_DEST} &&
      ! -L ${ENV_DEST} &&
      $(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${ENV_DEST}") == "${created_env_identity}" &&
      $(sha256sum -- "${ENV_DEST}" | awk 'NR == 1 { print $1 }') == "${created_env_sha256}" ]]; then
      rm -f -- "${ENV_DEST}" || failed=true
      [[ ! -e ${ENV_DEST} && ! -L ${ENV_DEST} ]] || failed=true
    else
      printf 'install-autostream-worker: preserving environment file modified during failed installation: %s\n' \
        "${ENV_DEST}" >&2
    fi
  fi
  sync -f /etc/autostream || failed=true
  if [[ ${unit_recovered} != true ]]; then
    preserve_input_stage=true
  fi
  [[ ${failed} == false ]]
}

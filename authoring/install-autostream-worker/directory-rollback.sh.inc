
rollback_state_directory() {
  local failed=false

  [[ ${state_dir_mutated} == true ]] || return 0
  if [[ ${state_dir_existed} == true ]]; then
    if [[ -d ${STATE_DIR} &&
      ! -L ${STATE_DIR} &&
      $(readlink -f -- "${STATE_DIR}") == "${STATE_DIR}" &&
      $(stat -c '%d:%i' -- "${STATE_DIR}") == "${state_dir_original_identity}" ]]; then
      chown -- "${state_dir_original_owner}" "${STATE_DIR}" || failed=true
      chmod -- "${state_dir_original_mode}" "${STATE_DIR}" || failed=true
      [[ $(stat -c '%d:%i:%u:%g:%a' -- "${STATE_DIR}") == \
        "${state_dir_original_identity}:${state_dir_original_owner}:${state_dir_original_mode}" ]] || \
        failed=true
    else
      failed=true
    fi
  elif [[ -e ${STATE_DIR} || -L ${STATE_DIR} ]]; then
    if [[ -d ${STATE_DIR} &&
      ! -L ${STATE_DIR} &&
      $(readlink -f -- "${STATE_DIR}") == "${STATE_DIR}" &&
      -n ${state_dir_created_identity} &&
      $(stat -c '%d:%i' -- "${STATE_DIR}") == "${state_dir_created_identity}" &&
      -z $(find "${STATE_DIR}" -mindepth 1 -print -quit) ]]; then
      rmdir -- "${STATE_DIR}" || failed=true
      [[ ! -e ${STATE_DIR} && ! -L ${STATE_DIR} ]] || failed=true
    else
      failed=true
    fi
  fi
  sync -f "$(dirname -- "${STATE_DIR}")" || failed=true
  [[ ${failed} == false ]]
}

rollback_created_managed_release() {
  local failed=false
  local public_path

  [[ ${managed_release_created} == true ]] || return 0
  if [[ ! -e ${RELEASE_DIR} && ! -L ${RELEASE_DIR} ]]; then
    return 0
  fi
  if [[ ! -d ${RELEASE_DIR} ||
    -L ${RELEASE_DIR} ||
    $(readlink -f -- "${RELEASE_DIR}") != "${RELEASE_DIR}" ||
    $(stat -c '%d:%i' -- "${RELEASE_DIR}") != "${managed_release_created_identity}" ]]; then
    return 1
  fi
  if [[ -L ${CURRENT_LINK} &&
    $(readlink -f -- "${CURRENT_LINK}") == "${RELEASE_DIR}" ]]; then
    return 1
  fi
  for public_path in "${PUBLIC_BINARY}" "${PUBLIC_ALIAS}"; do
    if [[ -L ${public_path} &&
      $(readlink -f -- "${public_path}") == "${RELEASE_DIR}/bin/autostream-worker" ]]; then
      return 1
    fi
  done
  rm -rf -- "${RELEASE_DIR}" || failed=true
  [[ ! -e ${RELEASE_DIR} && ! -L ${RELEASE_DIR} ]] || failed=true
  sync -f "${RELEASES_DIR}" || failed=true
  [[ ${failed} == false ]]
}

rollback_journaled_directories() {
  local index
  local path
  local current_mode
  local failed=false

  for ((index=${#journaled_directory_paths[@]} - 1; index >= 0; index--)); do
    path="${journaled_directory_paths[index]}"
    if [[ ${directory_existed["${path}"]} == true ]]; then
      if [[ -d ${path} &&
        ! -L ${path} &&
        $(readlink -f -- "${path}") == "${path}" &&
        $(stat -c '%d:%i' -- "${path}") == "${directory_original_identity["${path}"]}" ]]; then
        chown -- "${directory_original_owner["${path}"]}" "${path}" || failed=true
        chmod -- "${directory_original_mode["${path}"]}" "${path}" || failed=true
        [[ $(stat -c '%d:%i:%u:%g:%a' -- "${path}") == \
          "${directory_original_identity["${path}"]}:${directory_original_owner["${path}"]}:${directory_original_mode["${path}"]}" ]] || \
          failed=true
      else
        failed=true
      fi
    elif [[ -e ${path} || -L ${path} ]]; then
      if [[ -d ${path} &&
        ! -L ${path} &&
        $(readlink -f -- "${path}") == "${path}" &&
        -n ${directory_created_identity["${path}"]-} &&
        $(stat -c '%d:%i' -- "${path}") == "${directory_created_identity["${path}"]}" ]]; then
        if [[ -z $(find "${path}" -mindepth 1 -print -quit) ]]; then
          rmdir -- "${path}" || failed=true
          [[ ! -e ${path} && ! -L ${path} ]] || failed=true
        else
          current_mode="$(stat -c '%a' -- "${path}")"
          if [[ ${directory_retain_if_nonempty["${path}"]} == true &&
            $(stat -c '%U:%G' -- "${path}") == "root:root" &&
            ${current_mode} =~ ^[0-7]+$ ]] &&
            (( (8#${current_mode} & 0022) == 0 )); then
            printf 'install-autostream-worker: retaining non-empty durable backup directory after rollback: %s\n' \
              "${path}" >&2
          else
            failed=true
          fi
        fi
      else
        failed=true
      fi
    fi
    if [[ -d $(dirname -- "${path}") && ! -L $(dirname -- "${path}") ]]; then
      sync -f "$(dirname -- "${path}")" || failed=true
    fi
  done
  [[ ${failed} == false ]]
}

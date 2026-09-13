
verify_preexisting_legacy_backups() {
  local path
  local failed=false

  for path in "${!preexisting_legacy_backup_identity[@]}"; do
    [[ -f ${path} &&
      ! -L ${path} &&
      $(stat -c '%d:%i:%s:%Y:%Z:%f:%u:%g:%a' -- "${path}") == \
        "${preexisting_legacy_backup_identity["${path}"]}" &&
      $(sha256sum -- "${path}" | awk 'NR == 1 { print $1 }') == \
        "${preexisting_legacy_backup_digest["${path}"]}" ]] || \
      failed=true
  done
  [[ ${failed} == false ]]
}

cleanup_temporary_rollback_paths() {
  local index
  local path
  local failed=false

  for ((index=${#temporary_rollback_paths[@]} - 1; index >= 0; index--)); do
    path="${temporary_rollback_paths[index]}"
    if [[ -e ${path} || -L ${path} ]]; then
      rm -f -- "${path}" || failed=true
      [[ ! -e ${path} && ! -L ${path} ]] || failed=true
    fi
  done
  [[ ${failed} == false ]]
}

cleanup_managed_candidate() {
  [[ -n ${managed_candidate} ]] || return 0
  [[ ! -e ${managed_candidate} && ! -L ${managed_candidate} ]] && return 0
  [[ ${managed_candidate} == "${RELEASES_DIR}/.install-${VERSION}."* &&
    $(dirname -- "${managed_candidate}") == "${RELEASES_DIR}" &&
    -d ${managed_candidate} &&
    ! -L ${managed_candidate} &&
    $(readlink -f -- "${managed_candidate}") == "${managed_candidate}" ]] || \
    return 1
  rm -rf -- "${managed_candidate}"
  [[ ! -e ${managed_candidate} && ! -L ${managed_candidate} ]]
}

cleanup() {
  local original_status=$?

  trap '' HUP INT TERM
  trap - EXIT
  set +e
  local rollback_failed=false

  if [[ ${installation_committed} != true ]]; then
    rollback_public_links || rollback_failed=true
    rollback_current_link || rollback_failed=true
    rollback_unit_and_environment || rollback_failed=true
    rollback_state_directory || rollback_failed=true
    cleanup_managed_candidate || rollback_failed=true
    cleanup_temporary_rollback_paths || rollback_failed=true
    rollback_created_managed_release || rollback_failed=true
    rollback_journaled_directories || rollback_failed=true
    rollback_autostream_account || rollback_failed=true
    verify_preexisting_legacy_backups || rollback_failed=true
  fi
  best_effort_sync_mutated_parents || rollback_failed=true
  if [[ ${rollback_failed} == true ]]; then
    preserve_input_stage=true
  fi
  cleanup_managed_candidate || rollback_failed=true
  cleanup_temporary_rollback_paths || rollback_failed=true
  if [[ ${preserve_input_stage} == true ]]; then
    printf 'install-autostream-worker: preserved recovery files at %s\n' \
      "${INPUT_STAGE}" >&2
  else
    rm -rf -- "${INPUT_STAGE}" || rollback_failed=true
  fi
  best_effort_sync_mutated_parents || rollback_failed=true
  if [[ ${rollback_failed} == true ]]; then
    printf 'install-autostream-worker: one or more cleanup operations failed; inspect host state before retrying\n' >&2
    [[ ${original_status} -ne 0 ]] || original_status=1
  fi
  return "${original_status}"
}
trap cleanup EXIT
install_termination_signal_handlers
install -d -o root -g root -m 0700 "${UNPACK_DIR}"
record_mutated_parent "${UNPACK_DIR}"

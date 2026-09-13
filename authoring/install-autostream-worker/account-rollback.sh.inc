
local_account_member_fields_are_clear() {
  [[ -f /etc/group && ! -L /etc/group && -r /etc/group &&
    -f /etc/gshadow && ! -L /etc/gshadow && -r /etc/gshadow ]] || return 1
  awk -F: \
    -v service_login="autostream" \
    -v rollback_login="${AUTOSTREAM_USER_ROLLBACK_LOGIN}" \
    '{
      count = split($4, members, ",")
      for (member_index = 1; member_index <= count; member_index++) {
        if (members[member_index] == service_login || members[member_index] == rollback_login) {
          exit 1
        }
      }
    }' /etc/group || return 1
  awk -F: \
    -v service_login="autostream" \
    -v rollback_login="${AUTOSTREAM_USER_ROLLBACK_LOGIN}" \
    '{
      admin_count = split($3, admins, ",")
      for (admin_index = 1; admin_index <= admin_count; admin_index++) {
        if (admins[admin_index] == service_login || admins[admin_index] == rollback_login) {
          exit 1
        }
      }
      member_count = split($4, members, ",")
      for (member_index = 1; member_index <= member_count; member_index++) {
        if (members[member_index] == service_login || members[member_index] == rollback_login) {
          exit 1
        }
      }
    }' /etc/gshadow
}

local_account_database_matches_digests() {
  local expected_group_digest=$1
  local expected_gshadow_digest=$2
  local group_digest gshadow_digest

  group_digest="$(sha256sum -- /etc/group | awk 'NR == 1 { print $1 }')" || return 1
  gshadow_digest="$(sha256sum -- /etc/gshadow | awk 'NR == 1 { print $1 }')" || return 1
  [[ ${group_digest} =~ ^[[:xdigit:]]{64}$ &&
    ${gshadow_digest} =~ ^[[:xdigit:]]{64}$ &&
    ${group_digest} == "${expected_group_digest}" &&
    ${gshadow_digest} == "${expected_gshadow_digest}" ]]
}

prepare_autostream_user_rollback_login() {
  [[ -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  local_account_member_fields_are_clear || return 1
  autostream_user_rollback_login_ready=true
}

restore_created_autostream_user_login() {
  local expected_group_record=$1
  local renamed_user_record=$2
  local expected_group_digest=$3
  local expected_gshadow_digest=$4

  [[ -z $(getent passwd autostream 2>/dev/null || true) &&
    $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) == \
      "${renamed_user_record}" &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  usermod --login autostream "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" || return 1
  [[ $(getent passwd autostream 2>/dev/null || true) == \
      "${created_autostream_user_record}" &&
    -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    $(getent group autostream 2>/dev/null || true) == "${expected_group_record}" &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  local_account_database_matches_digests \
    "${expected_group_digest}" "${expected_gshadow_digest}"
}

remove_created_autostream_user_preserving_group() {
  local delete_status=0 expected_group_gid expected_group_record
  local group_database_digest_before gshadow_database_digest_before
  local rename_status=0 renamed_user_record user_record_suffix

  [[ ${created_autostream_user} == true &&
    ${autostream_user_rollback_login_ready} == true &&
    ${created_autostream_user_record} =~ ^autostream(:[^:]*:[1-9][0-9]*:[1-9][0-9]*:[^:]*:/var/lib/autostream:/usr/sbin/nologin)$ ]] || \
    return 1
  user_record_suffix=${BASH_REMATCH[1]}
  [[ $(getent passwd autostream 2>/dev/null || true) == \
      "${created_autostream_user_record}" &&
    $(id -u autostream 2>/dev/null || true) == "${created_autostream_user_uid}" &&
    $(id -g autostream 2>/dev/null || true) == "${created_autostream_user_gid}" &&
    $(id -Gn autostream 2>/dev/null || true) == "autostream" ]] || return 1

  if [[ ${created_autostream_group} == true ]]; then
    expected_group_record=${created_autostream_group_record}
  else
    expected_group_record=${preexisting_autostream_group_record}
  fi
  [[ -n ${expected_group_record} &&
    $(getent group autostream 2>/dev/null || true) == "${expected_group_record}" ]] || \
    return 1
  expected_group_gid="$(printf '%s\n' "${expected_group_record}" |
    awk -F: 'NR == 1 && $1 == "autostream" { print $3 }')"
  [[ ${expected_group_gid} =~ ^[1-9][0-9]*$ &&
    ${created_autostream_user_gid} == "${expected_group_gid}" &&
    -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1

  group_database_digest_before="$(sha256sum -- /etc/group | awk 'NR == 1 { print $1 }')" || \
    return 1
  gshadow_database_digest_before="$(sha256sum -- /etc/gshadow | awk 'NR == 1 { print $1 }')" || \
    return 1
  [[ ${group_database_digest_before} =~ ^[[:xdigit:]]{64}$ &&
    ${gshadow_database_digest_before} =~ ^[[:xdigit:]]{64}$ ]] || return 1
  local_account_member_fields_are_clear || return 1
  local_account_database_matches_digests \
    "${group_database_digest_before}" "${gshadow_database_digest_before}" || return 1

  # Ubuntu userdel may delete an empty group whose name matches the login.
  # Rename only the invocation-created login so the protected group cannot match.
  renamed_user_record="${AUTOSTREAM_USER_ROLLBACK_LOGIN}${user_record_suffix}"
  if usermod --login "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" autostream; then
    rename_status=0
  else
    rename_status=$?
  fi
  if [[ ${rename_status} -ne 0 ||
    -n $(getent passwd autostream 2>/dev/null || true) ||
    $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) != \
      "${renamed_user_record}" ||
    $(getent group autostream 2>/dev/null || true) != "${expected_group_record}" ||
    -n $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] ||
    ! local_account_database_matches_digests \
      "${group_database_digest_before}" "${gshadow_database_digest_before}"; then
    restore_created_autostream_user_login \
      "${expected_group_record}" "${renamed_user_record}" \
      "${group_database_digest_before}" "${gshadow_database_digest_before}" || true
    return 1
  fi

  if userdel "${AUTOSTREAM_USER_ROLLBACK_LOGIN}"; then
    delete_status=0
  else
    delete_status=$?
  fi
  if [[ ${delete_status} -ne 0 ]]; then
    restore_created_autostream_user_login \
      "${expected_group_record}" "${renamed_user_record}" \
      "${group_database_digest_before}" "${gshadow_database_digest_before}" || true
    return 1
  fi
  [[ -z $(getent passwd autostream 2>/dev/null || true) &&
    -z $(getent passwd "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) &&
    $(getent group autostream 2>/dev/null || true) == "${expected_group_record}" &&
    -z $(getent group "${AUTOSTREAM_USER_ROLLBACK_LOGIN}" 2>/dev/null || true) ]] || \
    return 1
  local_account_database_matches_digests \
    "${group_database_digest_before}" "${gshadow_database_digest_before}" || return 1
  created_autostream_user=false
  created_autostream_user_uid=""
  created_autostream_user_gid=""
  created_autostream_user_record=""
}

rollback_autostream_account() {
  local failed=false
  local current_group

  if [[ ${created_autostream_user} == true ]]; then
    remove_created_autostream_user_preserving_group || failed=true
  fi
  if [[ ${created_autostream_group} == true &&
    ${created_autostream_user} == true ]]; then
    failed=true
  elif [[ ${created_autostream_group} == true ]]; then
    current_group="$(getent group autostream 2>/dev/null || true)"
    if [[ -z ${current_group} ]]; then
      :
    elif [[ ${current_group} == "${created_autostream_group_record}" &&
      $(printf '%s\n' "${current_group}" | awk -F: 'NR == 1 { print $3 }') == \
        "${created_autostream_group_gid}" ]] &&
      ! getent passwd autostream >/dev/null 2>&1; then
      groupdel autostream || failed=true
      getent group autostream >/dev/null 2>&1 && failed=true
    else
      failed=true
    fi
  fi
  [[ ${failed} == false ]]
}

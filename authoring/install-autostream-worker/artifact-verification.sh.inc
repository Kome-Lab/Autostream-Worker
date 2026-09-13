
copy_stable_source() {
  local source=$1
  local destination=$2
  local identity
  local digest

  identity="$(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${source}")"
  digest="$(sha256sum -- "${source}" | awk 'NR == 1 { print $1 }')"
  install -o root -g root -m 0600 "${source}" "${destination}"
  [[ -f ${source} && ! -L ${source} ]] || \
    die "release source changed while being staged: ${source}"
  [[ $(stat -c '%d:%i:%s:%Y:%f:%u:%g' -- "${source}") == "${identity}" ]] || \
    die "release source identity changed while being staged: ${source}"
  [[ $(sha256sum -- "${source}" | awk 'NR == 1 { print $1 }') == "${digest}" ]] || \
    die "release source content changed while being staged: ${source}"
  [[ $(sha256sum -- "${destination}" | awk 'NR == 1 { print $1 }') == "${digest}" ]] || \
    die "staged release file differs from its source: ${source}"
}

ensure_root_directory() {
  local path=$1
  local requested_mode=$2
  local retain_if_nonempty=${3-false}
  local expected_mode="${requested_mode#0}"
  local existing_mode

  defer_termination_signals
  journal_root_directory_before_mutation "${path}" "${retain_if_nonempty}"
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} && ! -L ${path} ]] || \
      die "root-owned directory path is unsafe: ${path}"
    [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
      die "root-owned directory has unexpected ownership: ${path}"
    existing_mode="$(stat -c '%a' -- "${path}")"
    (( (8#${existing_mode} & 0022) == 0 )) || \
      die "root-owned directory is group/other writable: ${path}"
  else
    mkdir -- "${path}"
    directory_created_identity["${path}"]="$(stat -c '%d:%i' -- "${path}")"
  fi
  resume_termination_signals
  chown root:root "${path}"
  chmod "${requested_mode}" "${path}"
  [[ $(stat -c '%U:%G:%a' -- "${path}") == "root:root:${expected_mode}" ]] || \
    die "root-owned directory mode verification failed: ${path}"
  [[ $(readlink -f -- "${path}") == "${path}" ]] || \
    die "root-owned directory must resolve to its exact canonical path: ${path}"
}

ensure_permanent_lock_directory() {
  local path=$1

  # updater target lock is permanent installer coordination state.
  if [[ -e ${path} || -L ${path} ]]; then
    [[ -d ${path} &&
      ! -L ${path} &&
      $(readlink -f -- "${path}") == "${path}" &&
      $(stat -c '%U:%G:%a' -- "${path}") == "root:root:700" ]] || \
      die "permanent updater lock directory is unsafe: ${path}"
  else
    install -d -o root -g root -m 0700 "${path}"
    [[ -d ${path} &&
      ! -L ${path} &&
      $(readlink -f -- "${path}") == "${path}" &&
      $(stat -c '%U:%G:%a' -- "${path}") == "root:root:700" ]] || \
      die "could not create the permanent updater lock directory"
    sync -f "$(dirname -- "${path}")"
  fi
}

ensure_permanent_lock_path_atomically() {
  local path=$1
  local lock_mode

  if [[ ! -e ${path} && ! -L ${path} ]]; then
    defer_termination_signals
    lock_create_stage="$(mktemp /run/autostream-updater/.lock-create.XXXXXXXX)"
    [[ -f ${lock_create_stage} &&
      ! -L ${lock_create_stage} &&
      $(stat -c '%U:%G:%a' -- "${lock_create_stage}") == "root:root:600" ]] || \
      die "could not create a safe updater lock candidate"
    register_temporary_rollback_path "${lock_create_stage}"
    if ln -- "${lock_create_stage}" "${path}" 2>/dev/null; then
      sync -f /run/autostream-updater
    elif [[ ! -e ${path} && ! -L ${path} ]]; then
      die "could not atomically create a permanent updater lock"
    fi
    rm -f -- "${lock_create_stage}"
    lock_create_stage=""
    resume_termination_signals
  fi
  [[ -f ${path} && ! -L ${path} ]] || \
    die "updater lock path is unsafe: ${path}"
  [[ $(stat -c '%U:%G' -- "${path}") == "root:root" ]] || \
    die "updater lock must be owned by root:root: ${path}"
  lock_mode="$(stat -c '%a' -- "${path}")"
  (( (8#${lock_mode} & 0077) == 0 )) || \
    die "updater lock must not grant group/other permissions: ${path}"
}

copy_stable_source "${ARCHIVE_SOURCE}" "${INPUT_STAGE}/${ARCHIVE_NAME}"

ARTIFACT_SHA256="$(
  sha256sum -- "${INPUT_STAGE}/${ARCHIVE_NAME}" | awk 'NR == 1 { print $1 }'
)" || die "could not calculate the artifact SHA-256 digest"
[[ ${ARTIFACT_SHA256} =~ ^[0-9a-f]{64}$ ]] || \
  die "could not calculate a valid artifact SHA-256 digest"
readonly ARTIFACT_SHA256
[[ $(stat -c %s -- "${INPUT_STAGE}/${ARCHIVE_NAME}") == "${ARTIFACT_SIZE}" ]] || \
  die "staged release archive size differs from its source"

tar -tzf "${INPUT_STAGE}/${ARCHIVE_NAME}" > "${INPUT_STAGE}/archive.list"
[[ -s ${INPUT_STAGE}/archive.list ]] || die "release archive is empty"
: > "${INPUT_STAGE}/archive.canonical.list"
while IFS= read -r entry; do
  [[ ${entry} =~ ^[A-Za-z0-9._/-]+$ ]] || \
    die "release archive contains an unsafe path: ${entry}"
  [[ ${entry} == "${ARTIFACT_ID}" ||
    ${entry} == "${ARTIFACT_ID}/" ||
    ${entry} == "${ARTIFACT_ID}/"* ]] || \
    die "release archive contains a path outside ${ARTIFACT_ID}: ${entry}"
  [[ ${entry} != /* &&
    ${entry} != *\\* &&
    ${entry} != ../* &&
    ${entry} != *"/../"* &&
    ${entry} != *"/.." &&
    ${entry} != *"/./"* &&
    ${entry} != *"/." &&
    ${entry} != *"//"* ]] || \
    die "release archive contains an unsafe path: ${entry}"
  canonical_entry="${entry%/}"
  [[ -n ${canonical_entry} ]] || \
    die "release archive contains an empty canonical path"
  printf '%s\n' "${canonical_entry}" >> "${INPUT_STAGE}/archive.canonical.list"
done < "${INPUT_STAGE}/archive.list"
duplicate_paths="$(
  LC_ALL=C sort "${INPUT_STAGE}/archive.canonical.list" | uniq -d
)"
[[ -z ${duplicate_paths} ]] || die "release archive contains duplicate paths"
while IFS= read -r entry; do
  case "${entry:0:1}" in
    -|d) ;;
    *) die "release archive contains a symlink, hardlink, or other unsupported entry" ;;
  esac
done < <(tar -tvzf "${INPUT_STAGE}/${ARCHIVE_NAME}")

tar --no-same-owner --no-same-permissions \
  -xzf "${INPUT_STAGE}/${ARCHIVE_NAME}" \
  -C "${UNPACK_DIR}"
readonly EXTRACTED_ROOT="${UNPACK_DIR}/${ARTIFACT_ID}"
[[ -d ${EXTRACTED_ROOT} && ! -L ${EXTRACTED_ROOT} ]] || \
  die "release archive root is missing or unsafe"
[[ -z $(find "${UNPACK_DIR}" -mindepth 1 -maxdepth 1 ! -name "${ARTIFACT_ID}" -print -quit) ]] || \
  die "release archive contains more than one top-level path"
[[ -z $(find "${EXTRACTED_ROOT}" -type l -print -quit) ]] || \
  die "release archive must not contain symlinks"
for reserved_marker in .artifact-sha256 .version; do
  [[ ! -e ${EXTRACTED_ROOT}/${reserved_marker} &&
    ! -L ${EXTRACTED_ROOT}/${reserved_marker} ]] || \
    die "release archive must not provide ${reserved_marker}"
done

find "${EXTRACTED_ROOT}" -type d -exec chmod 0755 {} +
find "${EXTRACTED_ROOT}" -type f -exec chmod 0644 {} +
for executable in \
  "${EXTRACTED_ROOT}/bin/autostream-worker" \
  "${EXTRACTED_ROOT}/bin/worker" \
  "${EXTRACTED_ROOT}/install-autostream-worker"; do
  [[ -f ${executable} && ! -L ${executable} ]] || \
    die "required executable is missing or unsafe: ${executable#${EXTRACTED_ROOT}/}"
  chmod 0755 "${executable}"
done

[[ -f ${EXTRACTED_ROOT}/checksums.txt && ! -L ${EXTRACTED_ROOT}/checksums.txt ]] || \
  die "release checksums.txt is missing or unsafe"
while IFS= read -r checksum_entry; do
  [[ ${checksum_entry} =~ ^[0-9a-f]{64}\ \ (.+)$ ]] || \
    die "inner checksum entry has an invalid format"
  checked_path="${BASH_REMATCH[1]}"
  [[ ${checked_path} == ./* ]] || \
    die "inner checksum entry must use a ./ relative path"
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
    die "inner checksum entry has an unsafe path"
  [[ -f ${EXTRACTED_ROOT}/${checked_path} && ! -L ${EXTRACTED_ROOT}/${checked_path} ]] || \
    die "inner checksum entry does not name a regular file: ${checked_path}"
done < "${EXTRACTED_ROOT}/checksums.txt"
(cd -- "${EXTRACTED_ROOT}" && sha256sum --check --strict checksums.txt)

for required in \
  ".env.example" \
  "${ARTIFACT_MANIFEST_NAME}" \
  "systemd/autostream-worker.service.example" \
  "install-autostream-worker"; do
  [[ -f ${EXTRACTED_ROOT}/${required} && ! -L ${EXTRACTED_ROOT}/${required} ]] || \
    die "required release file is missing or unsafe: ${required}"
done
find "${EXTRACTED_ROOT}" -type f ! -path "${EXTRACTED_ROOT}/checksums.txt" \
  -printf './%P\n' | sort > "${INPUT_STAGE}/expected-checksum-paths"
awk '{ print substr($0, 67) }' "${EXTRACTED_ROOT}/checksums.txt" |
  sort > "${INPUT_STAGE}/declared-checksum-paths"
cmp -s \
  "${INPUT_STAGE}/expected-checksum-paths" \
  "${INPUT_STAGE}/declared-checksum-paths" || \
  die "release checksums.txt does not cover the exact regular file set"

readonly ARTIFACT_MANIFEST_PATH="${EXTRACTED_ROOT}/${ARTIFACT_MANIFEST_NAME}"
jq -e \
  --arg component "${SERVICE_NAME}" \
  --arg version "${VERSION}" \
  --arg arch "${ARCH}" \
  --arg archive_name "${ARCHIVE_NAME}" \
  --arg archive_root "${ARTIFACT_ID}" \
  '((keys | sort) ==
     ["archive", "build_date", "commit", "compatibility", "component", "platform", "schema_version", "source_version"]) and
   (.schema_version == 1) and
   (.component == $component) and
   (.source_version == $version) and
   (.commit | type == "string" and test("^[0-9a-f]{40}$")) and
   (.build_date | type == "string" and
     test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
   (.platform | type == "object") and
   ((.platform | keys | sort) == ["arch", "os"]) and
   (.platform.os == "linux") and
   (.platform.arch == $arch) and
   (.archive | type == "object") and
   ((.archive | keys | sort) == ["name", "root"]) and
   (.archive.name == $archive_name) and
   (.archive.root == $archive_root) and
   (.compatibility | type == "object") and
   ((.compatibility | keys | sort) ==
     ["database_schema", "minimum_agent_version", "minimum_panel_version", "rollback_compatible"]) and
   (.compatibility.minimum_agent_version == "v1.0.0") and
   (.compatibility.minimum_panel_version == null) and
   (.compatibility.rollback_compatible == true) and
   (.compatibility.database_schema == "none")' \
  "${ARTIFACT_MANIFEST_PATH}" >/dev/null || \
  die "${ARTIFACT_MANIFEST_NAME} does not describe this exact Worker artifact"

ARTIFACT_COMMIT="$(jq -er '.commit' "${ARTIFACT_MANIFEST_PATH}")" || \
  die "could not read the Worker artifact commit"
readonly ARTIFACT_COMMIT
ARTIFACT_BUILD_DATE="$(jq -er '.build_date' "${ARTIFACT_MANIFEST_PATH}")" || \
  die "could not read the Worker artifact build date"
readonly ARTIFACT_BUILD_DATE

verify_worker_binary_identity() {
  local binary_output=$1
  local context=$2
  local -a identity_lines=()

  mapfile -t identity_lines <<< "${binary_output}"
  [[ ${#identity_lines[@]} -eq 3 ]] || \
    die "${context} identity output must contain exactly three lines"
  [[ ${identity_lines[0]} == "autostream-worker ${VERSION}" ]] || \
    die "${context} version does not match ${VERSION}"
  [[ ${identity_lines[1]} == "commit: ${ARTIFACT_COMMIT}" ]] || \
    die "${context} commit does not match ${ARTIFACT_MANIFEST_NAME}"
  [[ ${identity_lines[2]} == "build_date: ${ARTIFACT_BUILD_DATE}" ]] || \
    die "${context} build date does not match ${ARTIFACT_MANIFEST_NAME}"
}

grep -Fx -- "ExecStart=${PUBLIC_BINARY}" \
  "${EXTRACTED_ROOT}/systemd/autostream-worker.service.example" >/dev/null || \
  die "Worker systemd unit does not use the stable public binary path"
cmp -s \
  "${EXTRACTED_ROOT}/bin/autostream-worker" \
  "${EXTRACTED_ROOT}/bin/worker" || \
  die "worker binary aliases in the release do not match"
EXTRACTED_BINARY_IDENTITY="$(
  "${EXTRACTED_ROOT}/bin/autostream-worker" --version
)" || die "Worker binary version command failed"
readonly EXTRACTED_BINARY_IDENTITY
verify_worker_binary_identity "${EXTRACTED_BINARY_IDENTITY}" "Worker binary"

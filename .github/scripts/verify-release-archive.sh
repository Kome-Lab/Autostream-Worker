#!/usr/bin/env bash
set -euo pipefail

readonly MAX_ARCHIVE_SIZE=268435456

die() {
  echo "verify-release-archive: $*" >&2
  exit 1
}

if [[ $# -ne 2 ]]; then
  die "usage: verify-release-archive.sh <archive.tar.gz> <expected-root>"
fi

archive_path="$1"
expected_root="$2"
archive_name="${archive_path##*/}"

[[ ${expected_root} =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]] ||
  die "expected archive root is unsafe"
[[ ${archive_name} == "${expected_root}.tar.gz" ]] ||
  die "archive name does not match its expected top-level root"
[[ -f ${archive_path} && ! -L ${archive_path} ]] ||
  die "release archive must be a regular non-symlink file"

archive_size="$(stat -c %s -- "${archive_path}")" ||
  die "could not read release archive size"
[[ ${archive_size} =~ ^[1-9][0-9]*$ ]] &&
  (( archive_size <= MAX_ARCHIVE_SIZE )) ||
  die "release archive size must be between 1 and ${MAX_ARCHIVE_SIZE} bytes"

temp_parent="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
validation_template="${temp_parent%/}/autostream-release-verify.XXXXXXXX"
validation_prefix="${validation_template%XXXXXXXX}"
validation_dir=""

cleanup() {
  if [[ -n ${validation_dir} && ${validation_dir} == "${validation_prefix}"* ]]; then
    rm -rf -- "${validation_dir}"
  fi
}
trap cleanup EXIT HUP INT TERM

validation_dir="$(mktemp -d "${validation_template}")" ||
  die "could not create release archive validation directory"
archive_list="${validation_dir}/archive.list"
canonical_list="${validation_dir}/archive.canonical.list"
verbose_list="${validation_dir}/archive.verbose.list"
duplicate_list="${validation_dir}/archive.duplicates.list"
extract_dir="${validation_dir}/extract"

LC_ALL=C tar --quoting-style=escape -tzf "${archive_path}" > "${archive_list}" ||
  die "release archive listing failed"
[[ -s ${archive_list} ]] || die "release archive is empty"
LC_ALL=C tar --quoting-style=escape -tvzf "${archive_path}" > "${verbose_list}" ||
  die "release archive type listing failed"
awk '
  substr($0, 1, 1) != "-" && substr($0, 1, 1) != "d" { exit 1 }
' "${verbose_list}" ||
  die "release archive contains a non-file/non-directory member"

: > "${canonical_list}"
root_member_count=0
while IFS= read -r entry || [[ -n ${entry} ]]; do
  [[ -n ${entry} ]] ||
    die "release archive contains an empty member name"
  [[ ${entry} != /* &&
     ${entry} != ./* &&
     ${entry} != *\\* &&
     ${entry} != *"//"* &&
     ${entry} != *"/./"* &&
     ${entry} != *"/." &&
     ${entry} != *"/../"* &&
     ${entry} != *"/.." ]] ||
    die "release archive contains a non-canonical member name"

  canonical_entry="${entry%/}"
  [[ -n ${canonical_entry} ]] ||
    die "release archive contains an empty canonical member name"
  if [[ ${canonical_entry} == "${expected_root}" ]]; then
    root_member_count=$((root_member_count + 1))
  elif [[ ${canonical_entry} != "${expected_root}/"* ]]; then
    die "release archive member is outside the expected top-level root"
  fi
  printf '%s\n' "${canonical_entry}" >> "${canonical_list}"
done < "${archive_list}"

[[ ${root_member_count} -eq 1 ]] ||
  die "release archive must contain exactly one expected top-level root entry"
LC_ALL=C sort "${canonical_list}" | uniq -d > "${duplicate_list}"
[[ ! -s ${duplicate_list} ]] ||
  die "release archive contains duplicate canonical member names"
checksum_member_count="$(
  grep -Fxc -- "${expected_root}/checksums.txt" "${canonical_list}" || true
)"
[[ ${checksum_member_count} -eq 1 ]] ||
  die "release archive must contain exactly one root checksums.txt"

mkdir -p "${extract_dir}"
tar --no-same-owner --no-same-permissions -xzf "${archive_path}" -C "${extract_dir}" ||
  die "release archive extraction failed"
extracted_root="${extract_dir}/${expected_root}"
[[ -d ${extracted_root} && ! -L ${extracted_root} ]] ||
  die "release archive did not extract the expected regular directory root"
special_entry="$(find "${extracted_root}" ! -type f ! -type d -print -quit)"
[[ -z ${special_entry} ]] ||
  die "release archive contains a non-file/non-directory member"
checksums_path="${extracted_root}/checksums.txt"
[[ -f ${checksums_path} && ! -L ${checksums_path} ]] ||
  die "release archive checksums.txt is not a regular file"

actual_files="${validation_dir}/actual-files.list"
declared_files="${validation_dir}/declared-files.list"
declared_files_sorted="${validation_dir}/declared-files.sorted.list"
declared_duplicates="${validation_dir}/declared-files.duplicates.list"
(
  cd "${extracted_root}"
  find . -type f ! -path './checksums.txt' -print | LC_ALL=C sort
) > "${actual_files}"

if ! LC_ALL=C awk '
  {
    digest = substr($0, 1, 64)
    separator = substr($0, 65, 2)
    path = substr($0, 67)
    if (length(digest) != 64 ||
        digest !~ /^[0-9a-f]+$/ ||
        separator != "  " ||
        path !~ /^\.\// ||
        path == "./") {
      exit 1
    }
    print path
  }
' "${checksums_path}" > "${declared_files}"; then
  die "release checksums.txt has an invalid entry"
fi
LC_ALL=C sort "${declared_files}" > "${declared_files_sorted}"
LC_ALL=C uniq -d "${declared_files_sorted}" > "${declared_duplicates}"
[[ ! -s ${declared_duplicates} ]] ||
  die "release checksums.txt contains duplicate file entries"
if ! diff -u "${actual_files}" "${declared_files_sorted}"; then
  die "release checksums.txt does not cover the exact regular-file inventory"
fi
(
  cd "${extracted_root}"
  sha256sum --check --strict checksums.txt > /dev/null
) || die "release archive checksum verification failed"

echo "Verified release archive ${archive_name}"

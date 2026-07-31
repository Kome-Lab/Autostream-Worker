#!/usr/bin/env bash
set -euo pipefail

die() {
  echo "test-verify-release-archive: $*" >&2
  exit 1
}

[[ $# -eq 1 ]] ||
  die "usage: test-verify-release-archive.sh <verifier>"
verifier="$1"
[[ -f ${verifier} && ! -L ${verifier} ]] ||
  die "verifier must be a regular non-symlink file"
verifier="$(
  cd "$(dirname "${verifier}")"
  printf '%s/%s\n' "$(pwd -P)" "$(basename "${verifier}")"
)"

test_root="autostream-test_v1.0.0_linux_amd64"
test_parent="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
test_template="${test_parent%/}/autostream-release-verifier-test.XXXXXXXX"
test_prefix="${test_template%XXXXXXXX}"
test_dir=""

cleanup() {
  if [[ -n ${test_dir} && ${test_dir} == "${test_prefix}"* ]]; then
    rm -rf -- "${test_dir}"
  fi
}
trap cleanup EXIT HUP INT TERM

test_dir="$(mktemp -d "${test_template}")" ||
  die "could not create verifier test directory"
base_dir="${test_dir}/base"
mkdir -p "${base_dir}/${test_root}/bin"
printf '%s\n' "fixture binary" > "${base_dir}/${test_root}/bin/app"
printf '%s\n' '{"schema_version":1}' > "${base_dir}/${test_root}/artifact-manifest.json"
(
  cd "${base_dir}/${test_root}"
  find . -type f ! -path './checksums.txt' -print0 |
    LC_ALL=C sort -z |
    xargs -0 sha256sum --text > checksums.txt
)
tar -C "${base_dir}" -czf "${base_dir}/${test_root}.tar.gz" "${test_root}"
bash "${verifier}" "${base_dir}/${test_root}.tar.gz" "${test_root}" > /dev/null

prepare_case() {
  local case_name="$1"
  case_dir="${test_dir}/${case_name}"
  mkdir -p "${case_dir}"
  cp -a "${base_dir}/${test_root}" "${case_dir}/${test_root}"
  case_archive="${case_dir}/${test_root}.tar.gz"
}

pack_case() {
  tar -C "${case_dir}" -czf "${case_archive}" "${test_root}"
}

expect_failure() {
  local case_name="$1"
  local expected_error="$2"
  local log_path="${test_dir}/${case_name}.log"
  if bash "${verifier}" "${case_archive}" "${test_root}" > "${log_path}" 2>&1; then
    die "${case_name} fixture unexpectedly passed"
  fi
  grep -F -- "${expected_error}" "${log_path}" > /dev/null ||
    die "${case_name} fixture failed outside the expected boundary"
}

prepare_case "extra-file"
printf '%s\n' "not checksummed" > "${case_dir}/${test_root}/extra-file"
pack_case
expect_failure "extra-file" \
  "release checksums.txt does not cover the exact regular-file inventory"

prepare_case "missing-checksum"
awk '$2 != "./artifact-manifest.json"' \
  "${case_dir}/${test_root}/checksums.txt" \
  > "${case_dir}/${test_root}/checksums.txt.next"
mv "${case_dir}/${test_root}/checksums.txt.next" \
  "${case_dir}/${test_root}/checksums.txt"
pack_case
expect_failure "missing-checksum" \
  "release checksums.txt does not cover the exact regular-file inventory"

prepare_case "stale-checksum"
printf '%s\n' "changed after checksum generation" \
  > "${case_dir}/${test_root}/bin/app"
pack_case
expect_failure "stale-checksum" "release archive checksum verification failed"

prepare_case "duplicate-member"
duplicate_tar="${case_dir}/${test_root}.tar"
tar -C "${case_dir}" -cf "${duplicate_tar}" "${test_root}"
tar -C "${case_dir}" -rf "${duplicate_tar}" "${test_root}/bin/app"
gzip -c "${duplicate_tar}" > "${case_archive}"
expect_failure "duplicate-member" \
  "release archive contains duplicate canonical member names"

prepare_case "canonical-alias"
alias_tar="${case_dir}/${test_root}.tar"
tar -C "${case_dir}" -cf "${alias_tar}" "${test_root}"
tar -C "${case_dir}" \
  --transform="s#^${test_root}/bin/app\$#${test_root}/bin//app#" \
  -rf "${alias_tar}" "${test_root}/bin/app"
gzip -c "${alias_tar}" > "${case_archive}"
expect_failure "canonical-alias" \
  "release archive contains a non-canonical member name"

prepare_case "symlink-entry"
if ln -s app "${case_dir}/${test_root}/bin/app-link" 2> /dev/null &&
   [[ -L ${case_dir}/${test_root}/bin/app-link ]]; then
  pack_case
  expect_failure "symlink-entry" \
    "release archive contains a non-file/non-directory member"
else
  echo "SKIP symlink-entry: symbolic links are unavailable on this host"
fi

prepare_case "fifo-entry"
if mkfifo "${case_dir}/${test_root}/runtime.pipe" 2> /dev/null &&
   [[ -p ${case_dir}/${test_root}/runtime.pipe ]]; then
  pack_case
  expect_failure "fifo-entry" \
    "release archive contains a non-file/non-directory member"
else
  echo "SKIP fifo-entry: FIFOs are unavailable on this host"
fi

echo "Release archive verifier fixtures passed"

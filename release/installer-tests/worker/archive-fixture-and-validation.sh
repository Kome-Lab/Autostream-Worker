
install -d -o root -g root -m 0755 \
  "${ARTIFACTS_DIR}" \
  "${EXTRACTED_ROOT}/bin" \
  "${EXTRACTED_ROOT}/systemd"
install -o root -g root -m 0755 "${INSTALLER_SOURCE}" \
  "${EXTRACTED_ROOT}/install-autostream-worker"

cat > "${EXTRACTED_ROOT}/bin/autostream-worker" <<'EOF'
#!/bin/sh
if [ "${1:-}" = "--version" ]; then
  printf '%s\n' 'autostream-worker v9.9.9'
  printf '%s\n' 'commit: 0123456789abcdef0123456789abcdef01234567'
  printf '%s\n' 'build_date: 2026-01-01T00:00:00Z'
  exit 0
fi
exit 99
EOF
chmod 0755 "${EXTRACTED_ROOT}/bin/autostream-worker"
cp "${EXTRACTED_ROOT}/bin/autostream-worker" "${EXTRACTED_ROOT}/bin/worker"
chmod 0755 "${EXTRACTED_ROOT}/bin/worker"

cat > "${EXTRACTED_ROOT}/systemd/autostream-worker.service.example" <<'EOF'
[Unit]
Description=AutoStream Worker integration fixture

[Service]
Type=simple
User=autostream
Group=autostream
EnvironmentFile=-/etc/autostream/worker.env
LoadCredential=node-listener.json:/opt/autostream/local-executor/ports/worker.json
ExecStart=/usr/local/bin/autostream-worker

[Install]
WantedBy=multi-user.target
EOF
printf '%s\n' 'AUTOSTREAM_NODE_CONFIG=/etc/autostream-worker/config.yml' \
  > "${EXTRACTED_ROOT}/.env.example"
printf '%s\n' 'integration fixture' > "${EXTRACTED_ROOT}/README.install.md"
jq -n \
  --arg version "${VERSION}" \
  --arg name "${ARTIFACT_ID}.tar.gz" \
  --arg root "${ARTIFACT_ID}" \
  '{
    schema_version: 1,
    component: "worker",
    source_version: $version,
    commit: "0123456789abcdef0123456789abcdef01234567",
    build_date: "2026-01-01T00:00:00Z",
    platform: {
      os: "linux",
      arch: "amd64"
    },
    archive: {
      name: $name,
      root: $root
    },
    compatibility: {
      minimum_agent_version: "v1.0.0",
      minimum_panel_version: null,
      rollback_compatible: true,
      database_schema: "none"
    }
  }' > "${EXTRACTED_ROOT}/artifact-manifest.json"

(
  cd -- "${EXTRACTED_ROOT}"
  find . -type f ! -path './checksums.txt' -print0 |
    sort -z |
    xargs -0 sha256sum > checksums.txt
)
tar -C "${ARTIFACTS_DIR}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
archive_sha256="$(sha256sum "${ARCHIVE}" | awk 'NR == 1 { print $1 }')"
readonly RETAINED_DIR="${INSTALL_BACKUP_ROOT}/${VERSION}-${archive_sha256:0:12}"
[[ ! -e ${ARCHIVE}.sha256 && ! -L ${ARCHIVE}.sha256 ]] || \
  die "archive-only fixture unexpectedly contains an archive checksum sidecar"
[[ ! -e ${ARTIFACTS_DIR}/release-manifest.json &&
  ! -L ${ARTIFACTS_DIR}/release-manifest.json ]] || \
  die "archive-only fixture unexpectedly contains an external release manifest"
[[ ! -e ${ARTIFACTS_DIR}/release-manifest.json.sha256 &&
  ! -L ${ARTIFACTS_DIR}/release-manifest.json.sha256 ]] || \
  die "archive-only fixture unexpectedly contains an external manifest checksum sidecar"

readonly VALID_ARCHIVE="${WORK_DIR}/${ARTIFACT_ID}.valid.tar.gz"
readonly VARIANT_PARENT="${WORK_DIR}/variant"
readonly VARIANT_ROOT="${VARIANT_PARENT}/${ARTIFACT_ID}"
install -o root -g root -m 0600 "${ARCHIVE}" "${VALID_ARCHIVE}"

prepare_variant_tree() {
  rm -rf -- "${VARIANT_PARENT}"
  install -d -o root -g root -m 0700 "${VARIANT_PARENT}"
  cp -a -- "${EXTRACTED_ROOT}" "${VARIANT_ROOT}"
}

replace_variant_manifest() {
  local filter=$1
  local next_manifest="${WORK_DIR}/artifact-manifest.next"

  jq "${filter}" "${VARIANT_ROOT}/artifact-manifest.json" > "${next_manifest}"
  install -o root -g root -m 0644 \
    "${next_manifest}" \
    "${VARIANT_ROOT}/artifact-manifest.json"
  rm -f -- "${next_manifest}"
}

regenerate_variant_checksums() {
  local next_checksums="${WORK_DIR}/variant-checksums.next"

  (
    cd -- "${VARIANT_ROOT}"
    find . -type f ! -path './checksums.txt' -print0 |
      sort -z |
      xargs -0 sha256sum > "${next_checksums}"
  )
  install -o root -g root -m 0644 \
    "${next_checksums}" \
    "${VARIANT_ROOT}/checksums.txt"
  rm -f -- "${next_checksums}"
}

package_variant_archive() {
  tar -C "${VARIANT_PARENT}" -czf "${ARCHIVE}" "${ARTIFACT_ID}"
}

package_noncanonical_duplicate_archive() {
  local raw_archive="${WORK_DIR}/noncanonical-duplicate.tar"
  local archive_list="${WORK_DIR}/noncanonical-duplicate.list"

  rm -f -- "${raw_archive}" "${archive_list}"
  tar -C "${VARIANT_PARENT}" -cf "${raw_archive}" "${ARTIFACT_ID}"
  tar -C "${VARIANT_PARENT}" -rf "${raw_archive}" \
    --transform="s#^${ARTIFACT_ID}/\\.env\\.example\$#${ARTIFACT_ID}/./.env.example#" \
    "${ARTIFACT_ID}/.env.example"
  gzip -c "${raw_archive}" > "${ARCHIVE}"
  tar -tzf "${ARCHIVE}" > "${archive_list}"
  grep -Fx -- "${ARTIFACT_ID}/.env.example" "${archive_list}" > /dev/null || \
    die "noncanonical duplicate fixture is missing its canonical entry"
  grep -Fx -- "${ARTIFACT_ID}/./.env.example" "${archive_list}" > /dev/null || \
    die "noncanonical duplicate fixture is missing its alias entry"
  rm -f -- "${raw_archive}" "${archive_list}"
}

restore_valid_archive() {
  install -o root -g root -m 0600 "${VALID_ARCHIVE}" "${ARCHIVE}"
}

assert_preflight_rejection_kept_host_clean() {
  local scenario=$1

  for path in \
    "${PUBLIC_BINARY}" \
    "${PUBLIC_ALIAS}" \
    "${ENV_PATH}" \
    "${UNIT_PATH}" \
    "${STATE_DIR}" \
    "${MANAGED_ROOT}" \
    "${INSTALL_BACKUP_ROOT}" \
    "${SHARED_HOST_SETUP_LOCK}" \
    "${TARGET_LOCK}"; do
    [[ ! -e ${path} && ! -L ${path} ]] || \
      die "${scenario} rejection mutated the host: ${path}"
  done
  [[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
    -name 'autostream-worker-install.*' -print -quit) ]] || \
    die "${scenario} rejection retained production staging"
  if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
    die "${scenario} rejection created the autostream service account"
  fi
}

expect_preflight_rejection() {
  local scenario=$1
  local expected_message=$2
  local output_path="${WORK_DIR}/${scenario}.out"
  local status

  set +e
  "${EXTRACTED_ROOT}/install-autostream-worker" > "${output_path}" 2>&1
  status=$?
  set -e
  [[ ${status} -ne 0 ]] || \
    die "${scenario} artifact unexpectedly passed preflight"
  grep -F -- "${expected_message}" "${output_path}" >/dev/null || \
    die "${scenario} rejection did not report the expected error"
  assert_preflight_rejection_kept_host_clean "${scenario}"
  restore_valid_archive
}

prepare_variant_tree
replace_variant_manifest '.source_version = "v9.9.8"'
regenerate_variant_checksums
package_variant_archive
expect_preflight_rejection \
  "manifest-version-mismatch" \
  "artifact-manifest.json does not describe this exact Worker artifact"

prepare_variant_tree
replace_variant_manifest '.platform.arch = "arm64"'
regenerate_variant_checksums
package_variant_archive
expect_preflight_rejection \
  "manifest-architecture-mismatch" \
  "artifact-manifest.json does not describe this exact Worker artifact"

prepare_variant_tree
replace_variant_manifest \
  '.commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
regenerate_variant_checksums
package_variant_archive
expect_preflight_rejection \
  "binary-commit-mismatch" \
  "Worker binary commit does not match artifact-manifest.json"

prepare_variant_tree
printf '%s\n' 'corrupt unchecked payload' >> "${VARIANT_ROOT}/.env.example"
package_variant_archive
expect_preflight_rejection \
  "inner-checksum-mismatch" \
  "./.env.example: FAILED"

prepare_variant_tree
package_noncanonical_duplicate_archive
expect_preflight_rejection \
  "noncanonical-duplicate-path" \
  "release archive contains an unsafe path: ${ARTIFACT_ID}/./.env.example"

install -d -o root -g root -m 0755 "${PUBLIC_BINARY}"
late_preflight_public_path_owned=true
set +e
"${EXTRACTED_ROOT}/install-autostream-worker" \
  > "${WORK_DIR}/late-host-preflight.out" 2>&1
late_host_preflight_status=$?
set -e
[[ ${late_host_preflight_status} -eq 1 ]] || \
  die "late host preflight fixture unexpectedly succeeded"
grep -Fx -- \
  "install-autostream-worker: existing public path is not a regular file: ${PUBLIC_BINARY}" \
  "${WORK_DIR}/late-host-preflight.out" >/dev/null || \
  die "late host preflight fixture did not reach the public-path rejection"
[[ -d ${PUBLIC_BINARY} && ! -L ${PUBLIC_BINARY} &&
  $(stat -c '%U:%G:%a' -- "${PUBLIC_BINARY}") == "root:root:755" &&
  -z $(find "${PUBLIC_BINARY}" -mindepth 1 -print -quit) ]] || \
  die "late host preflight changed the rejected public path"
if id autostream >/dev/null 2>&1 || getent group autostream >/dev/null 2>&1; then
  die "late host preflight created the autostream service account"
fi
for path in \
  "${PUBLIC_ALIAS}" \
  "${ENV_PATH}" \
  "${UNIT_PATH}" \
  "${STATE_DIR}" \
  "${MANAGED_ROOT}" \
  "${INSTALL_BACKUP_ROOT}" \
  "${SHARED_HOST_SETUP_LOCK}" \
  "${TARGET_LOCK}" \
  /opt/autostream \
  /etc/autostream \
  /var/lib/autostream \
  /var/backups/autostream \
  /run/autostream-updater; do
  [[ ! -e ${path} && ! -L ${path} ]] || \
    die "late host preflight created a persistent path: ${path}"
done
[[ -z $(find /var/tmp -mindepth 1 -maxdepth 1 \
  -name 'autostream-worker-install.*' -print -quit) ]] || \
  die "late host preflight retained production staging"
rmdir -- "${PUBLIC_BINARY}"
late_preflight_public_path_owned=false

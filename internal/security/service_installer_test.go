package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerReleaseShipsManagedServiceInstaller(t *testing.T) {
	root := filepath.Join("..", "..")
	installerPath := filepath.Join(root, "release", "install-autostream-worker")
	installerBytes, err := os.ReadFile(installerPath)
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)

	for _, marker := range []string{
		"set -euo pipefail",
		`readonly SERVICE_NAME="worker"`,
		`readonly MANAGED_ROOT="/opt/autostream/worker"`,
		`readonly PUBLIC_BINARY="/usr/local/bin/autostream-worker"`,
		`readonly PUBLIC_ALIAS="/usr/local/bin/worker"`,
		`readonly ENV_DEST="/etc/autostream/worker.env"`,
		`readonly UNIT_DEST="/etc/systemd/system/autostream-worker.service"`,
		`readonly INSTALL_MIGRATION_ROOT="/var/backups/autostream/install-migrations"`,
		`readonly LEGACY_BACKUP_ROOT="${INSTALL_MIGRATION_ROOT}/worker"`,
		`$(stat -c '%U:%G' -- "${STATE_DIR}") == "autostream:autostream"`,
		`install -d -o autostream -g autostream -m 0750 "${STATE_DIR}"`,
		`ensure_root_directory "${LEGACY_BACKUP_ROOT}" 0700`,
		`must resolve to its exact canonical path`,
		`[[ ${env_mode} == "600" || ${env_mode} == "640" ]]`,
		`.database_schema == "none"`,
		"release archive contains duplicate paths",
		"tar uname \\\n  uniq useradd",
		"sha256sum --check --strict",
		"artifact-manifest.json",
		".artifact-sha256",
		".version",
		"state_dir_existed=false",
		"state_dir_original_identity",
		"verify_state_directory_snapshot",
		"state_dir_mutated=true",
		"rollback_state_directory",
		"preflight_existing_host_paths_before_mutation",
		"rollback_unit_and_environment",
		"previous-autostream-worker.service",
		"systemctl daemon-reload",
		"systemctl is-active --quiet",
	} {
		if !strings.Contains(installer, marker) {
			t.Fatalf("service installer is missing %q", marker)
		}
	}

	workflowBytes, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release-host.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(workflowBytes)
	for _, marker := range []string{
		`cp release/install-autostream-worker "${root}/install-autostream-worker"`,
		`chmod 0755 "${root}/install-autostream-worker"`,
		"bash -n release/install-autostream-worker",
		"bash -n release/test-install-autostream-worker-integration.sh",
		"sudo bash release/test-install-autostream-worker-integration.sh",
		"DATABASE_SCHEMA: none",
		"artifacts/autostream-worker_${{ needs.release-host.outputs.version }}_linux_amd64.tar.gz",
		"artifacts/autostream-worker_${{ needs.release-host.outputs.version }}_linux_arm64.tar.gz",
	} {
		if !strings.Contains(workflow, marker) {
			t.Fatalf("host release workflow is missing installer packaging marker %q", marker)
		}
	}
	ciBytes, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"bash -n release/install-autostream-worker",
		"bash -n release/test-install-autostream-worker-integration.sh",
		"sudo bash release/test-install-autostream-worker-integration.sh",
	} {
		if !strings.Contains(string(ciBytes), marker) {
			t.Fatalf("CI workflow is missing installer integration marker %q", marker)
		}
	}

	unitBytes, err := os.ReadFile(filepath.Join(root, "systemd", "autostream-worker.service.example"))
	if err != nil {
		t.Fatal(err)
	}
	unit := string(unitBytes)
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/autostream-worker") {
		t.Fatal("Worker systemd unit must use the stable public binary path")
	}
	if strings.Contains(unit, "ExecStart=/opt/autostream/worker/current/") {
		t.Fatal("Worker systemd unit exposes installer-owned release internals")
	}

	guideBytes, err := os.ReadFile(filepath.Join(root, "release", "README.install.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(guideBytes)
	for _, marker := range []string{
		"sudo ./install-autostream-worker",
		"installer-owned",
		"gh attestation verify autostream-worker_vX.Y.Z_linux_amd64.tar.gz",
		"On an administration workstation",
		"Only the matching",
		"one uploaded archive",
		"No `.sha256` sidecar or external `release-manifest.json` is needed",
		"artifact-manifest.json",
		"sudo tar --no-same-owner --no-same-permissions -xzf autostream-worker_vX.Y.Z_linux_amd64.tar.gz",
		"/var/backups/autostream/install-migrations/worker",
		"--repo Kome-Lab/Autostream-Worker",
		"--signer-workflow Kome-Lab/Autostream-Worker/.github/workflows/release-host.yml",
		"--deny-self-hosted-runners",
	} {
		if !strings.Contains(guide, marker) {
			t.Fatalf("install guide is missing simple installer marker %q", marker)
		}
	}
	for _, forbidden := range []string{
		"gh attestation verify release-manifest.json",
		"/tmp/release-manifest.json",
		".tar.gz.sha256 /opt/autostream/releases/artifacts",
	} {
		if strings.Contains(guide, forbidden) {
			t.Fatalf("install guide still requires external manual-install metadata %q", forbidden)
		}
	}
}

func TestWorkerInstallerRejectsVersionPrefixCollisions(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "release", "install-autostream-worker"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(body)
	const exactIdentityCheck = `[[ ${identity_lines[0]} == "autostream-worker ${VERSION}" ]]`
	if count := strings.Count(installer, exactIdentityCheck); count != 1 {
		t.Fatalf("expected one reusable exact-line version identity check, got %d", count)
	}
	if strings.Contains(installer, `grep -F -- "autostream-worker ${VERSION}"`) {
		t.Fatal("version identity check must reject prefix or multi-line collisions")
	}
}

func TestWorkerManualInstallerUsesArchiveOnlyContract(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "release", "install-autostream-worker"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(body)

	for _, marker := range []string{
		`readonly ARTIFACT_MANIFEST_NAME="artifact-manifest.json"`,
		`["archive", "build_date", "commit", "compatibility", "component", "platform", "schema_version", "source_version"]`,
		`["arch", "os"]`,
		`["name", "root"]`,
		`["database_schema", "minimum_agent_version", "minimum_panel_version", "rollback_compatible"]`,
		`(.component == $component)`,
		`(.archive.name == $archive_name)`,
		`(.archive.root == $archive_root)`,
		`(.compatibility.minimum_agent_version == "v1.0.0")`,
		`(.compatibility.minimum_panel_version == null)`,
		`ARTIFACT_SIZE="$(stat -c %s -- "${ARCHIVE_SOURCE}")"`,
		`(( ARTIFACT_SIZE <= 268435456 ))`,
		`${entry} != *\\*`,
		`${entry} != *"/./"*`,
		`${entry} != *"/."`,
		`${entry} != *"//"*`,
		`canonical_entry="${entry%/}"`,
		`"${INPUT_STAGE}/archive.canonical.list"`,
		`commit does not match ${ARTIFACT_MANIFEST_NAME}`,
		`build date does not match ${ARTIFACT_MANIFEST_NAME}`,
	} {
		if !strings.Contains(installer, marker) {
			t.Fatalf("archive-only installer is missing %q", marker)
		}
	}

	for _, forbidden := range []string{
		"ARCHIVE_CHECKSUM_SOURCE",
		"MANIFEST_SOURCE",
		"release-manifest.json",
		".tar.gz.sha256",
	} {
		if strings.Contains(installer, forbidden) {
			t.Fatalf("manual installer still references external release metadata %q", forbidden)
		}
	}

	sizeCheckIndex := strings.Index(installer, `ARTIFACT_SIZE="$(stat -c %s -- "${ARCHIVE_SOURCE}")"`)
	copyIndex := strings.Index(installer, `copy_stable_source "${ARCHIVE_SOURCE}"`)
	if sizeCheckIndex < 0 || copyIndex < 0 || sizeCheckIndex >= copyIndex {
		t.Fatal("archive size limit must be enforced before the root-owned stable copy")
	}

	canonicalListIndex := strings.Index(installer, `: > "${INPUT_STAGE}/archive.canonical.list"`)
	duplicateCheckIndex := strings.Index(installer, `LC_ALL=C sort "${INPUT_STAGE}/archive.canonical.list" | uniq -d`)
	extractionIndex := strings.Index(installer, `tar --no-same-owner --no-same-permissions`)
	if canonicalListIndex < 0 || duplicateCheckIndex < 0 || extractionIndex < 0 ||
		canonicalListIndex >= duplicateCheckIndex || duplicateCheckIndex >= extractionIndex {
		t.Fatal("archive paths must be canonicalized and de-duplicated before extraction")
	}
	hostPreflightIndex := strings.Index(installer, "\npreflight_existing_host_paths_before_mutation\n")
	accountMutationIndex := strings.Index(installer, "\nif ! getent group autostream")
	if hostPreflightIndex < 0 || accountMutationIndex < 0 || hostPreflightIndex >= accountMutationIndex {
		t.Fatal("existing host paths must be preflighted before service-account mutation")
	}
}

func TestWorkerInstallerIntegrationFixtureCoversPrivilegedTransitions(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(
		"..", "..", "release", "test-install-autostream-worker-integration.sh",
	))
	if err != nil {
		t.Fatal(err)
	}
	fixture := string(body)
	for _, marker := range []string{
		`AUTOSTREAM_WORKER_INSTALLER_TEST_MOUNT_NS`,
		`autostream-worker-installer-test-scratch /mnt`,
		`/mnt/usr-lower \`,
		`/mnt/usr-upper/local`,
		`/mnt/usr-work \`,
		`mount --rbind /usr /mnt/usr-lower`,
		`mount --make-rprivate /mnt/usr-lower`,
		`mount --rbind /etc /mnt/etc-lower`,
		`mount --make-rprivate /mnt/etc-lower`,
		`mount --rbind /var /mnt/var-lower`,
		`mount --make-rprivate /mnt/var-lower`,
		`mount --rbind /run /mnt/run-lower`,
		`mount --make-rprivate /mnt/run-lower`,
		`lowerdir=/mnt/usr-lower,upperdir=/mnt/usr-upper,workdir=/mnt/usr-work`,
		`lowerdir=/mnt/etc-lower,upperdir=/mnt/etc-upper,workdir=/mnt/etc-work`,
		`lowerdir=/mnt/var-lower,upperdir=/mnt/var-upper,workdir=/mnt/var-work`,
		`lowerdir=/mnt/run-lower,upperdir=/mnt/run-upper,workdir=/mnt/run-work`,
		`install -d -o root -g root -m 1777 /mnt/var-upper/tmp`,
		`autostream-worker-installer-test-usr-overlay`,
		`grep -Eq ' /usr .* - overlay autostream-worker-installer-test-usr-overlay '`,
		`autostream-worker-installer-test-etc-overlay`,
		`grep -Eq ' /etc .* - overlay autostream-worker-installer-test-etc-overlay '`,
		`autostream-worker-installer-test-var-overlay`,
		`grep -Eq ' /var .* - overlay autostream-worker-installer-test-var-overlay '`,
		`autostream-worker-installer-test-run-overlay`,
		`grep -Eq ' /run .* - overlay autostream-worker-installer-test-run-overlay '`,
		`mount --rbind /mnt/run-lower/systemd /run/systemd`,
		`systemd_identity="$(stat -c "%d:%i" -- /mnt/run-lower/systemd)"`,
		`AUTOSTREAM_WORKER_INSTALLER_TEST_SYSTEMD_IDENTITY="${systemd_identity}"`,
		`autostream-worker-installer-test-sealed /mnt`,
		`ro,nodev,nosuid,noexec,mode=0555`,
		`sealed /mnt mount is missing`,
		`sealed /mnt mount options are unsafe`,
		`sealed /mnt ownership or mode is unsafe`,
		`sealed /mnt unexpectedly accepted a write`,
		`readonly EXPECTED_SYSTEMD_IDENTITY="${AUTOSTREAM_WORKER_INSTALLER_TEST_SYSTEMD_IDENTITY:-}"`,
		`host-backed /run/systemd mount is missing`,
		`host-backed /run/systemd mount identity is invalid`,
		`autostream-worker-installer-test /usr/local/bin`,
		`autostream-worker-installer-test-opt /opt`,
		`isolated /usr overlay mount is missing`,
		`isolated /usr/local/bin mount is missing`,
		`isolated /opt mount is missing`,
		`could not create an isolated safe /usr fixture`,
		`could not create an isolated safe /etc fixture`,
		`could not create an isolated safe /etc/systemd fixture`,
		`could not create an isolated safe /etc/systemd/system fixture`,
		`could not create an isolated safe /var fixture`,
		`could not create an isolated safe /var/lib fixture`,
		`could not create an isolated safe /var/backups fixture`,
		`could not create an isolated safe /var/tmp fixture`,
		`could not create an isolated safe /run fixture`,
		`could not create an isolated safe /usr/local fixture`,
		`could not create an isolated safe /usr/local/bin fixture`,
		`could not create an isolated safe /opt fixture`,
		`mount --bind '${FAIL_SYSTEMCTL}' /usr/bin/systemctl`,
		`daemon-reload failure injection did not reach the commit boundary`,
		`late host preflight created the autostream service account`,
		`late host preflight created a persistent path`,
		`failed migration did not restore the legacy binary`,
		`successful migration did not retain the legacy systemd unit`,
		`idempotent reinstall replaced the running legacy process`,
		`fresh installer did not create the autostream account`,
		`fresh installer unexpectedly started the service`,
		`fresh installer unexpectedly enabled the service`,
		`legacy_unit_file_state="$(systemctl is-enabled "${UNIT}" 2>/dev/null || true)"`,
		`legacy fixture must begin disabled`,
		`systemctl show --property MainPID`,
		`readonly RUNTIME_UNIT_PATH="/run/systemd/system/${UNIT}"`,
		`systemd runtime unit directory is unsafe`,
		`fixture_owns_paths=false`,
		`fixture_owns_runtime_unit=false`,
		`fixture_owns_service=false`,
		`if [[ ${fixture_owns_paths} == true ]]; then`,
		`if [[ ${fixture_owns_service} == true &&`,
		`if [[ ${fixture_owns_runtime_unit} == true &&`,
		`runtime_identity_matches=false`,
		`${runtime_identity_matches} == true`,
		`old_pid_starttime=""`,
		`/proc/${pid}/stat`,
		`${stat_fields[19]}`,
		`kill_recorded_process_if_same_starttime`,
		`PID reuse guard did not reject a mismatched process identity`,
		`PID reuse guard killed the mismatched process`,
		`cleanup_failed=false`,
		`owned runtime unit identity changed`,
		`runtime unit identity changed before service cleanup`,
		`runtime unit identity changed before removal`,
		`could not remove owned runtime unit`,
		`systemctl show --property ActiveState --value`,
		`service did not become inactive`,
		`service unit remained loaded`,
		`if [[ ${cleanup_failed} == true && ${exit_code} -eq 0 ]]; then`,
		`loaded_unit_state="$(systemctl show --property LoadState --value "${UNIT}"`,
		`runner service is already loaded`,
		`AUTOSTREAM_WORKER_INSTALLER_TEST_PREFLIGHT_PROBE=1`,
		`run_preflight_cleanup_probe`,
		`preflight failure removed the existing runtime unit`,
		`preflight failure replaced the existing service process`,
		`preflight failure changed the existing service enablement`,
		`ln -- "${runtime_unit_staging}" "${RUNTIME_UNIT_PATH}"`,
		`mv -fT -- "${runtime_unit_staging}" "${RUNTIME_UNIT_PATH}"`,
		`owned systemd runtime unit changed before atomic commit`,
		`sync -f -- "${runtime_unit_staging}"`,
		`sync -f -- /run/systemd/system`,
		`replace_owned_runtime_unit_atomically "${UNIT_PATH}"`,
		`systemctl show --property FragmentPath --value`,
		`systemctl show --property ExecStart --value`,
		`systemctl show --property User --value`,
		`assert_legacy_runtime_unit "failed migration"`,
		`readonly STATE_SENTINEL="${STATE_DIR}/rollback-sentinel.txt"`,
		`install -d -o autostream -g autostream -m 0700 "${STATE_DIR}"`,
		`failed migration changed existing state directory metadata`,
		`failed migration changed existing state directory content`,
		`failed migration did not restore exact live path metadata and content`,
		`assert_legacy_runtime_unit "sync failure"`,
		`sync failure retained a state directory that was absent before installation`,
		`sync failure did not restore exact live path metadata and content`,
		`GID 0 service-group rejection created the autostream service account`,
		`fresh daemon-reload rollback retained the installer-created account`,
		`fresh daemon-reload rollback retained a transactional path`,
		`fresh daemon-reload rollback did not retain only the permanent safe lock state`,
		`TERM signal rollback did not return status 143`,
		`TERM signal rollback cleanup did not survive a repeated TERM`,
		`TERM signal rollback changed the running legacy process`,
		`TERM signal rollback did not restore exact live path metadata and content`,
		`TERM signal rollback retained production staging`,
		`successful installation replaced or truncated the permanent updater lock`,
		`installer ignored shared host-setup lock contention`,
		`shared host-setup contention replaced or truncated the permanent lock`,
		`lock contention replaced or truncated the permanent updater lock`,
		`mount --bind '${SIGNAL_GROUPADD}' /usr/sbin/groupadd`,
		`mount --bind '${SIGNAL_USERADD}' /usr/sbin/useradd`,
		`assert_fresh_account_signal_rollback`,
		`useradd TERM rollback replaced or truncated a permanent lock`,
		`sync failure changed a pre-existing durable backup inode, metadata, or content`,
		`non-root-owned pre-existing backup fixture unexpectedly succeeded`,
		`non-root-owned backup rejection changed the conflicting backup`,
		`successful migration did not synchronize the managed runtime unit`,
		`idempotent reinstall changed the loaded runtime unit`,
		`"${TARGET_LOCK}"; do`,
		`/var/backups/autostream/install-migrations/worker`,
		`database_schema: "none"`,
	} {
		if !strings.Contains(fixture, marker) {
			t.Fatalf("Worker installer integration fixture is missing %q", marker)
		}
	}
	atomicReplaceStart := strings.Index(fixture, "replace_owned_runtime_unit_atomically() {")
	if atomicReplaceStart < 0 {
		t.Fatal("Worker installer fixture is missing the atomic runtime unit replacement function")
	}
	atomicReplaceEnd := strings.Index(
		fixture[atomicReplaceStart:],
		"\n}\n\nassert_loaded_runtime_unit()",
	)
	if atomicReplaceEnd < 0 {
		t.Fatal("Worker installer fixture is missing the atomic runtime unit replacement function")
	}
	atomicReplace := fixture[atomicReplaceStart : atomicReplaceStart+atomicReplaceEnd]
	stageRuntimeIndex := strings.Index(atomicReplace, `stage_runtime_unit "${source_path}"`)
	recheckIdentityIndex := strings.LastIndex(
		atomicReplace,
		`current_identity="$(stat -c '%d:%i' -- "${RUNTIME_UNIT_PATH}")"`,
	)
	rejectMismatchIndex := strings.Index(
		atomicReplace,
		`if [[ ${current_identity} != "${runtime_unit_identity}" ]]; then`,
	)
	atomicCommitIndex := strings.Index(
		atomicReplace,
		`mv -fT -- "${runtime_unit_staging}" "${RUNTIME_UNIT_PATH}"`,
	)
	if stageRuntimeIndex < 0 ||
		recheckIdentityIndex < 0 ||
		rejectMismatchIndex < 0 ||
		atomicCommitIndex < 0 ||
		stageRuntimeIndex >= recheckIdentityIndex ||
		recheckIdentityIndex >= rejectMismatchIndex ||
		rejectMismatchIndex >= atomicCommitIndex {
		t.Fatal("Worker runtime unit replacement must stage, recheck ownership, reject mismatches, then commit atomically")
	}
	namespaceIndex := strings.Index(
		fixture,
		`if [[ ${AUTOSTREAM_WORKER_INSTALLER_TEST_MOUNT_NS:-} != "1" ]]; then`,
	)
	workDirIndex := strings.Index(fixture, `work_dir=""`)
	if namespaceIndex < 0 || workDirIndex < 0 || namespaceIndex >= workDirIndex {
		t.Fatal("installer integration fixture must enter its isolated mount namespace before creating mutable state")
	}
	scratchIndex := strings.Index(
		fixture,
		`autostream-worker-installer-test-scratch /mnt`,
	)
	outerStrictIndex := strings.LastIndex(fixture, "set -euo pipefail")
	usrLowerIndex := strings.Index(fixture, `mount --rbind /usr /mnt/usr-lower`)
	usrPrivateIndex := strings.Index(fixture, `mount --make-rprivate /mnt/usr-lower`)
	etcLowerIndex := strings.Index(fixture, `mount --rbind /etc /mnt/etc-lower`)
	etcPrivateIndex := strings.Index(fixture, `mount --make-rprivate /mnt/etc-lower`)
	varLowerIndex := strings.Index(fixture, `mount --rbind /var /mnt/var-lower`)
	varPrivateIndex := strings.Index(fixture, `mount --make-rprivate /mnt/var-lower`)
	runLowerIndex := strings.Index(fixture, `mount --rbind /run /mnt/run-lower`)
	runPrivateIndex := strings.Index(fixture, `mount --make-rprivate /mnt/run-lower`)
	usrUpperIndex := strings.Index(fixture, `/mnt/usr-upper \`)
	etcUpperIndex := strings.Index(fixture, `/mnt/etc-upper \`)
	varUpperIndex := strings.Index(fixture, `/mnt/var-upper \`)
	runUpperIndex := strings.Index(fixture, `/mnt/run-upper \`)
	usrWorkIndex := strings.Index(fixture, `/mnt/usr-work \`)
	etcWorkIndex := strings.Index(fixture, `/mnt/etc-work \`)
	varWorkIndex := strings.Index(fixture, `/mnt/var-work \`)
	runWorkIndex := strings.Index(fixture, `/mnt/run-work`)
	usrOverlayIndex := strings.Index(fixture, `autostream-worker-installer-test-usr-overlay /usr`)
	etcOverlayIndex := strings.Index(fixture, `autostream-worker-installer-test-etc-overlay /etc`)
	varOverlayIndex := strings.Index(fixture, `autostream-worker-installer-test-var-overlay /var`)
	runOverlayIndex := strings.Index(fixture, `autostream-worker-installer-test-run-overlay /run`)
	systemdBindIndex := strings.Index(fixture, `mount --rbind /mnt/run-lower/systemd /run/systemd`)
	systemdIdentityIndex := strings.Index(
		fixture,
		`systemd_identity="$(stat -c "%d:%i" -- /mnt/run-lower/systemd)"`,
	)
	runtimeSafetyIndex := strings.Index(
		fixture,
		`readonly RUNTIME_UNIT_PATH="/run/systemd/system/${UNIT}"`,
	)
	binMountIndex := strings.Index(
		fixture,
		`autostream-worker-installer-test /usr/local/bin`,
	)
	optMountIndex := strings.Index(fixture, `autostream-worker-installer-test-opt /opt`)
	sealedMountIndex := strings.Index(fixture, `autostream-worker-installer-test-sealed /mnt`)
	identityExportIndex := strings.Index(
		fixture,
		`AUTOSTREAM_WORKER_INSTALLER_TEST_SYSTEMD_IDENTITY="${systemd_identity}"`,
	)
	if outerStrictIndex < 0 || scratchIndex < 0 ||
		usrLowerIndex < 0 || usrPrivateIndex < 0 ||
		etcLowerIndex < 0 || etcPrivateIndex < 0 ||
		varLowerIndex < 0 || varPrivateIndex < 0 ||
		runLowerIndex < 0 || runPrivateIndex < 0 ||
		usrUpperIndex < 0 || etcUpperIndex < 0 ||
		varUpperIndex < 0 || runUpperIndex < 0 ||
		usrWorkIndex < 0 || etcWorkIndex < 0 ||
		varWorkIndex < 0 || runWorkIndex < 0 ||
		usrOverlayIndex < 0 || etcOverlayIndex < 0 ||
		varOverlayIndex < 0 || runOverlayIndex < 0 ||
		systemdBindIndex < 0 || systemdIdentityIndex < 0 ||
		binMountIndex < 0 || optMountIndex < 0 ||
		sealedMountIndex < 0 || identityExportIndex < 0 ||
		workDirIndex < 0 || runtimeSafetyIndex < 0 ||
		namespaceIndex >= outerStrictIndex ||
		outerStrictIndex >= scratchIndex ||
		scratchIndex >= usrLowerIndex ||
		usrLowerIndex >= usrPrivateIndex ||
		usrPrivateIndex >= etcLowerIndex ||
		etcLowerIndex >= etcPrivateIndex ||
		etcPrivateIndex >= varLowerIndex ||
		varLowerIndex >= varPrivateIndex ||
		varPrivateIndex >= runLowerIndex ||
		runLowerIndex >= runPrivateIndex ||
		runPrivateIndex >= usrUpperIndex ||
		usrUpperIndex >= etcUpperIndex ||
		etcUpperIndex >= varUpperIndex ||
		varUpperIndex >= runUpperIndex ||
		runUpperIndex >= usrWorkIndex ||
		usrWorkIndex >= etcWorkIndex ||
		etcWorkIndex >= varWorkIndex ||
		varWorkIndex >= runWorkIndex ||
		runWorkIndex >= usrOverlayIndex ||
		usrOverlayIndex >= etcOverlayIndex ||
		etcOverlayIndex >= varOverlayIndex ||
		varOverlayIndex >= runOverlayIndex ||
		runOverlayIndex >= systemdBindIndex ||
		systemdBindIndex >= systemdIdentityIndex ||
		systemdIdentityIndex >= binMountIndex ||
		binMountIndex >= optMountIndex ||
		optMountIndex >= sealedMountIndex ||
		sealedMountIndex >= identityExportIndex ||
		identityExportIndex >= workDirIndex ||
		workDirIndex >= runtimeSafetyIndex {
		t.Fatal("installer integration fixture mount isolation and mutable-state ordering is incomplete")
	}
	const safeAccountReset = "userdel autostream\nif getent group autostream >/dev/null 2>&1; then\n  groupdel autostream\nfi"
	if count := strings.Count(fixture, safeAccountReset); count != 1 {
		t.Fatalf("expected one account reset that tolerates userdel removing the private group, got %d", count)
	}
	if count := strings.Count(fixture, "[Install]\nWantedBy=multi-user.target"); count != 3 {
		t.Fatalf("integration fixture must define three enable-capable but disabled units, got %d", count)
	}
	preflightIndex := strings.Index(fixture, `for path in \`)
	probeIndex := strings.Index(fixture, "\nrun_preflight_cleanup_probe\n")
	ownershipIndex := strings.Index(fixture, `fixture_owns_paths=true`)
	runtimeCreateIndex := strings.LastIndex(fixture, `create_runtime_unit_no_clobber "${UNIT_PATH}"`)
	if preflightIndex < 0 || probeIndex < 0 || ownershipIndex < 0 || runtimeCreateIndex < 0 ||
		preflightIndex >= probeIndex || probeIndex >= ownershipIndex ||
		ownershipIndex >= runtimeCreateIndex {
		t.Fatal("fixture must run the non-destructive preflight probe before claiming path ownership")
	}
	if strings.Contains(
		fixture,
		`install -o root -g root -m 0644 "${UNIT_PATH}" "${RUNTIME_UNIT_PATH}"`,
	) {
		t.Fatal("runtime unit creation must be atomic and no-clobber")
	}
	innerFixtureIndex := strings.Index(fixture, "\nfi\ngrep -Eq ' /mnt ")
	if innerFixtureIndex < 0 ||
		strings.Contains(fixture[innerFixtureIndex:], "/mnt/run-lower") {
		t.Fatal("sealed inner fixture must not retain a writable lower-directory alias")
	}
	starttimeCheckIndex := strings.Index(
		fixture,
		`[[ ${current_starttime} == "${old_pid_starttime}" ]]`,
	)
	fallbackKillIndex := strings.Index(fixture, `kill "${old_pid}" || return 1`)
	if starttimeCheckIndex < 0 || fallbackKillIndex < 0 ||
		starttimeCheckIndex >= fallbackKillIndex {
		t.Fatal("raw PID fallback must verify the recorded /proc start time before kill")
	}
}

func TestWorkerInstallerPrevalidatesAccountAndBindsPermanentLock(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "release", "install-autostream-worker"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(body)

	groupValidation := strings.Index(installer, `autostream_group_gid="$(getent group autostream`)
	userCreation := strings.Index(installer, `useradd --system --gid "${autostream_group_gid}"`)
	primaryGroupValidation := strings.LastIndex(installer,
		`[[ $(id -g autostream) == "${autostream_group_gid}" ]]`)
	if groupValidation < 0 || userCreation < 0 || primaryGroupValidation < 0 ||
		groupValidation >= userCreation || userCreation >= primaryGroupValidation {
		t.Fatal("Worker must validate a non-root numeric service GID before user creation and bind the user to it")
	}
	for _, marker := range []string{
		`created_autostream_group=false`,
		`created_autostream_user=false`,
		`rollback_autostream_account`,
		`if [[ -z ${current_group} ]]; then`,
		`rollback_journaled_directories`,
		`rollback_created_managed_release`,
		`snapshot_preexisting_legacy_backup`,
		`verify_preexisting_legacy_backups`,
		`register_temporary_rollback_path`,
		`trap cleanup_early_input_stage EXIT`,
		`trap 'exit_from_termination_signal 129' HUP`,
		`trap 'exit_from_termination_signal 130' INT`,
		`trap 'exit_from_termination_signal 143' TERM`,
		`trap 'deferred_termination_status=143' TERM`,
		`exit_from_termination_signal "${pending_status}"`,
		`resume_termination_signals`,
		`trap - EXIT`,
		`trap '' HUP INT TERM`,
		`exec 9<>"${TARGET_LOCK}"`,
		`readonly SHARED_HOST_SETUP_LOCK="/run/autostream-updater/.autostream-runtime-host-setup.lock"`,
		`exec 8<>"${SHARED_HOST_SETUP_LOCK}"`,
		`-f /proc/self/fd/8`,
		`$(stat -Lc '%U:%G:%a' -- /proc/self/fd/8) == "root:root:600"`,
		`shared host-setup lock identity changed after acquisition`,
		`chmod 0600 /proc/self/fd/9`,
		`chown root:root /proc/self/fd/9`,
		`-f /proc/self/fd/9`,
		`$(stat -Lc '%U:%G:%a' -- /proc/self/fd/9) == "root:root:600"`,
		`updater target lock identity changed after acquisition`,
		`updater target lock is permanent installer coordination state`,
	} {
		if !strings.Contains(installer, marker) {
			t.Fatalf("Worker installer is missing transactional account/lock marker %q", marker)
		}
	}
	if strings.Contains(installer, `exec 9>"${TARGET_LOCK}"`) {
		t.Fatal("Worker lock acquisition must not truncate the permanent lock inode")
	}
	if strings.Contains(installer, `exec 8>"${SHARED_HOST_SETUP_LOCK}"`) {
		t.Fatal("Worker shared lock acquisition must not truncate the permanent lock inode")
	}
	if strings.Contains(installer, `stat -Lc '%F:%U:%G:%a'`) {
		t.Fatal("Worker lock validation must not depend on GNU stat's content-sensitive file-type description")
	}
	sharedLockIndex := strings.Index(installer, `flock -n 8 || die "another AutoStream installer is provisioning shared host state"`)
	targetLockIndex := strings.Index(installer, `flock -n 9 || die "another privileged update is already active for ${UNIT_NAME}"`)
	accountMutationIndex := strings.LastIndex(installer, "\nif ! getent group autostream")
	if sharedLockIndex < 0 || targetLockIndex < 0 || accountMutationIndex < 0 ||
		sharedLockIndex >= targetLockIndex || targetLockIndex >= accountMutationIndex {
		t.Fatal("Worker must acquire the permanent shared host lock before its unit lock and account mutation")
	}
}

func TestWorkerInstallerDefersTerminationAcrossRollbackJournalWindows(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "release", "install-autostream-worker"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(body)

	section := func(name, start, end string) string {
		t.Helper()
		startIndex := strings.Index(installer, start)
		if startIndex < 0 {
			t.Fatalf("%s section start is missing: %q", name, start)
		}
		endIndex := strings.Index(installer[startIndex+len(start):], end)
		if endIndex < 0 {
			t.Fatalf("%s section end is missing: %q", name, end)
		}
		return installer[startIndex : startIndex+len(start)+endIndex]
	}
	assertOrdered := func(name, region string, markers ...string) {
		t.Helper()
		offset := 0
		for _, marker := range markers {
			index := strings.Index(region[offset:], marker)
			if index < 0 {
				t.Fatalf("%s signal journal is missing ordered marker %q", name, marker)
			}
			offset += index + len(marker)
		}
	}

	assertOrdered(
		"signal resume",
		section("signal resume", "resume_termination_signals() {", "\n}\n\ninput_stage="),
		"install_termination_signal_handlers",
		"pending_status=${deferred_termination_status}",
		"deferred_termination_status=0",
		`exit_from_termination_signal "${pending_status}"`,
	)
	assertOrdered(
		"input staging",
		section("input staging", `input_stage=""`, `readonly INPUT_STAGE=`),
		"defer_termination_signals",
		`input_stage="$(mktemp -d /var/tmp/autostream-worker-install.XXXXXXXX)"`,
		"trap cleanup_early_input_stage EXIT",
		"resume_termination_signals",
	)
	assertOrdered(
		"root directory",
		section("root directory", "ensure_root_directory() {", "\n}\n\nensure_permanent_lock_directory()"),
		"defer_termination_signals",
		"journal_root_directory_before_mutation",
		`mkdir -- "${path}"`,
		`directory_created_identity["${path}"]`,
		"resume_termination_signals",
	)
	assertOrdered(
		"permanent lock candidate",
		section("permanent lock candidate", "ensure_permanent_lock_path_atomically() {", "\n}\n\ncopy_stable_source"),
		"defer_termination_signals",
		`lock_create_stage="$(mktemp`,
		`register_temporary_rollback_path "${lock_create_stage}"`,
		`ln -- "${lock_create_stage}" "${path}"`,
		`rm -f -- "${lock_create_stage}"`,
		"resume_termination_signals",
	)
	assertOrdered(
		"service account",
		section("service account", "record_mutated_parent /etc\nrecord_mutated_parent /var/lib", `[[ $(id -u autostream) -ne 0 ]]`),
		"defer_termination_signals",
		"groupadd --system autostream",
		"created_autostream_group=true",
		"created_autostream_group_record=",
		"resume_termination_signals",
		"defer_termination_signals",
		`useradd --system --gid "${autostream_group_gid}"`,
		"created_autostream_user=true",
		"created_autostream_user_record=",
		"resume_termination_signals",
	)
	assertOrdered(
		"state directory",
		section("state directory", "state_dir_mutated=true", `[[ $(stat -c '%U:%G:%a' -- "${STATE_DIR}")`),
		"defer_termination_signals",
		`mkdir -- "${STATE_DIR}"`,
		"state_dir_created_identity=",
		"resume_termination_signals",
	)
	assertOrdered(
		"backup snapshot",
		section("backup snapshot", "snapshot_preexisting_legacy_backup() {", "\n}\n\nensure_durable_legacy_backup()"),
		"defer_termination_signals",
		`preexisting_legacy_backup_identity["${backup_path}"]`,
		`preexisting_legacy_backup_digest["${backup_path}"]`,
		"resume_termination_signals",
	)
	assertOrdered(
		"managed release",
		section("managed release", `if [[ -e ${RELEASE_DIR} || -L ${RELEASE_DIR} ]]`, `record_mutated_parent "${RELEASE_DIR}"`),
		"defer_termination_signals",
		`managed_candidate="$(mktemp -d`,
		"resume_termination_signals",
		"defer_termination_signals",
		"managed_release_created=true",
		`mv -T -- "${managed_candidate}" "${RELEASE_DIR}"`,
		"managed_release_created_identity=",
		"resume_termination_signals",
	)
	assertOrdered(
		"current link",
		section("current link", `current_next="${MANAGED_ROOT}/.current.next.$$"`, `if [[ -z ${existing_env_identity} ]]`),
		"defer_termination_signals",
		`mv -Tf -- "${current_next}" "${CURRENT_LINK}"`,
		"current_changed=true",
		"resume_termination_signals",
	)
	assertOrdered(
		"environment",
		section("environment", `if [[ -z ${existing_env_identity} ]]`, `if [[ ${previous_unit_exists} == true ]]`),
		"defer_termination_signals",
		`mv -T -- "${env_next}" "${ENV_DEST}"`,
		"created_env=true",
		"created_env_identity=",
		"created_env_sha256=",
		"resume_termination_signals",
	)
	assertOrdered(
		"systemd unit",
		section("systemd unit", `unit_next="${UNIT_DEST}.next.$$"`, "install_public_link() {"),
		"defer_termination_signals",
		`mv -Tf -- "${unit_next}" "${UNIT_DEST}"`,
		"unit_installed=true",
		"resume_termination_signals",
	)
	assertOrdered(
		"public path journal",
		section("public path journal", "install_public_link() {", "\n}\n\ninstall_public_link "),
		"defer_termination_signals",
		`changed_public_paths+=("${link_path}")`,
		`previous_public_kinds+=("${previous_kind}")`,
		`previous_public_targets+=("${previous_target}")`,
		`previous_public_recoveries+=("${previous_recovery}")`,
		"resume_termination_signals",
		`mv -Tf -- "${link_next}" "${link_path}"`,
	)
	assertOrdered(
		"cleanup",
		section("cleanup", "cleanup() {", "\n}\ntrap cleanup EXIT"),
		`trap '' HUP INT TERM`,
		"trap - EXIT",
		"rollback_public_links",
		"rollback_autostream_account",
	)
}

func TestWorkerInstallerCoversStagingDurabilityAndInterruptedRetry(t *testing.T) {
	root := filepath.Join("..", "..")
	installerBytes, err := os.ReadFile(filepath.Join(root, "release", "install-autostream-worker"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(installerBytes)
	for _, marker := range []string{
		`input_stage=""`,
		`if ! input_stage="$(mktemp -d /var/tmp/autostream-worker-install.XXXXXXXX)"`,
		`failed to create input staging directory`,
		`[[ -n ${input_stage} ]]`,
		`verify_root_permission_boundary`,
		`permission boundary must be owned by root:root`,
		`permission boundary must not be group/other writable`,
		`sync_installation_parents`,
		`failed to sync filesystem parent before commit`,
		`best_effort_sync_mutated_parents`,
		`recoverable legacy backup conflict`,
		`resuming interrupted public-path migration from verified backup`,
		`readonly TARGET_LOCK="/run/autostream-updater/.autostream-updater-${TARGET_LOCK_ID}.lock"`,
		`flock -n 9 || die "another privileged update is already active for ${UNIT_NAME}"`,
		`["archive", "build_date", "commit", "compatibility", "component", "platform", "schema_version", "source_version"]`,
		`test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")`,
		`["database_schema", "minimum_agent_version", "minimum_panel_version", "rollback_compatible"]`,
		`test("^[0-9a-f]{40}$")`,
		`(.platform.arch == $arch)`,
		`(.archive.name == $archive_name)`,
		`release checksums.txt does not cover the exact regular file set`,
		`verify_existing_worker_state_directory`,
		`Worker state path must resolve to its canonical path`,
		`Worker state path has unsafe write or special mode bits`,
		`mv -Tf -- "${link_next}" "${link_path}"`,
	} {
		if !strings.Contains(installer, marker) {
			t.Fatalf("service installer is missing staging/durability marker %q", marker)
		}
	}
	if strings.Contains(installer, `rm -f -- "${link_path}"`) {
		t.Fatal("public-path migration must atomically replace the existing regular file without an absent-path window")
	}

	fixtureBytes, err := os.ReadFile(filepath.Join(
		root, "release", "test-install-autostream-worker-integration.sh",
	))
	if err != nil {
		t.Fatal(err)
	}
	fixture := string(fixtureBytes)
	for _, marker := range []string{
		`FAIL_MKTEMP=`,
		`mktemp failure injection did not reach production mktemp`,
		`install-autostream-worker: failed to create input staging directory`,
		`mktemp failure mutated the host`,
		`FAIL_SYNC=`,
		`sync failure injection did not reach the durability boundary`,
		`sync failure did not restore the legacy binary`,
		`interrupted backup retry did not reuse the verified legacy backup`,
		`installer ignored updater lock contention`,
		`lock contention mutated the host`,
		`artifact-manifest.json`,
		`build_date: "2026-01-01T00:00:00Z"`,
		`minimum_agent_version: "v1.0.0"`,
		`commit: "0123456789abcdef0123456789abcdef01234567"`,
		`archive-only fixture unexpectedly contains an archive checksum sidecar`,
		`archive-only fixture unexpectedly contains an external release manifest`,
		`manifest-version-mismatch`,
		`manifest-architecture-mismatch`,
		`binary-commit-mismatch`,
		`inner-checksum-mismatch`,
		`noncanonical-duplicate-path`,
		`${ARTIFACT_ID}/./.env.example`,
		`package_noncanonical_duplicate_archive`,
		`rejection retained production staging`,
		`stale archive checksum sidecar must be ignored`,
		`fresh install changed the ignored external release manifest`,
	} {
		if !strings.Contains(fixture, marker) {
			t.Fatalf("Worker installer integration fixture is missing durability marker %q", marker)
		}
	}
}

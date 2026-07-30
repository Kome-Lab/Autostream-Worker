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
		"sha256sum --check --strict",
		"release-manifest.json",
		".artifact-sha256",
		".version",
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
		"gh attestation verify release-manifest.json",
		"sudo install -o root -g root -m 0644 /tmp/release-manifest.json /opt/autostream/releases/artifacts/release-manifest.json",
		"root-owned archive and",
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
}

func TestWorkerInstallerRejectsVersionPrefixCollisions(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "release", "install-autostream-worker"))
	if err != nil {
		t.Fatal(err)
	}
	const exactLineCheck = `grep -Fx -- "autostream-worker ${VERSION}"`
	if count := strings.Count(string(body), exactLineCheck); count != 2 {
		t.Fatalf("expected exact-line version checks before and after managed copy, got %d", count)
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
		`assert_legacy_runtime_unit "sync failure"`,
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
		`["channel", "components", "minimum_agent_version", "published_at", "release_id", "schema_version"]`,
		`test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")`,
		`["artifacts", "commit", "database_schema", "rollback_compatible", "service", "source_version"]`,
		`test("^[0-9a-f]{40}$")`,
		`(.components[0].artifacts | type == "array" and length == 2)`,
		`[.components[0].artifacts[].arch] | sort == ["amd64", "arm64"]`,
		`((keys | sort) == ["arch", "name", "os", "sha256", "size"])`,
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
		`published_at: "2026-01-01T00:00:00Z"`,
		`minimum_agent_version: "v1.0.0"`,
		`commit: "0123456789abcdef0123456789abcdef01234567"`,
		`arch: "arm64"`,
	} {
		if !strings.Contains(fixture, marker) {
			t.Fatalf("Worker installer integration fixture is missing durability marker %q", marker)
		}
	}
}

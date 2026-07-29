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
		`mount --bind '${FAIL_SYSTEMCTL}' /usr/bin/systemctl`,
		`daemon-reload failure injection did not reach the commit boundary`,
		`failed migration did not restore the legacy binary`,
		`successful migration did not retain the legacy systemd unit`,
		`idempotent reinstall replaced the running legacy process`,
		`fresh installer did not create the autostream account`,
		`fresh installer unexpectedly started the service`,
		`fresh installer unexpectedly enabled the service`,
		`systemctl show --property MainPID`,
		`/var/backups/autostream/install-migrations/worker`,
		`database_schema: "none"`,
	} {
		if !strings.Contains(fixture, marker) {
			t.Fatalf("Worker installer integration fixture is missing %q", marker)
		}
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

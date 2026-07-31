package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostReleaseWorkflowPublishesDeterministicNotesAndExactImmutableAssets(t *testing.T) {
	workflowPath := filepath.Join("..", "..", ".github", "workflows", "release-host.yml")
	payload, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(payload)

	orderedContract := []string{
		"permissions:\n  contents: read",
		"outputs:",
		"version: ${{ steps.meta.outputs.version }}",
		"- uses: actions/upload-artifact@",
		"publish-release:",
		"needs: release-host",
		"group: host-release-publish-${{ needs.release-host.outputs.version }}",
		"ARTIFACT_PREFIX: autostream-worker",
		"- name: Require repository immutable releases",
		"GH_TOKEN: ${{ secrets.AUTOSTREAM_RELEASE_SETTINGS_TOKEN || github.token }}",
		"\"repos/${GITHUB_REPOSITORY}/immutable-releases\"",
		"jq -e '(.enabled == true)'",
		"- name: Validate immutable release namespace and local asset set",
		"gh api --paginate \"repos/${GITHUB_REPOSITORY}/releases?per_page=100\"",
		"select(.tag_name == $tag)",
		"already exists (including drafts)",
		"workflow_dispatch may not overwrite or reuse it",
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_amd64.tar.gz",
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_amd64.tar.gz.sha256",
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_arm64.tar.gz",
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_arm64.tar.gz.sha256",
		"release-manifest.json",
		"release-manifest.json.sha256",
		"size=\"$(stat -c %s \"${path}\")\"",
		"digest=\"sha256:$(sha256sum \"${path}\" | awk '{ print $1 }')\"",
		"host-release-expected-assets.json",
		"- name: Create unpublished staging release with deterministic notes",
		"release_notes_path=\"${RUNNER_TEMP}/host-release-notes.md\"",
		"Migration bridge: `ssh-free-pull-v2`",
		"release_notes_digest=\"sha256:$(sha256sum \"${release_notes_path}\"",
		"host-release-expected-notes.sha256",
		"-F \"body=@${release_notes_path}\"",
		"- name: Upload all assets to staging release",
		"if [[ \"${#assets[@]}\" -ne 6 ]]",
		"- name: Verify staging release assets",
		"host-release-draft-notes.md",
		"jq -j '.body' \"${release_json}\" > \"${draft_notes_path}\"",
		"cmp -s \"${RUNNER_TEMP}/host-release-notes.md\" \"${draft_notes_path}\"",
		"draft_notes_digest=\"sha256:$(sha256sum \"${draft_notes_path}\"",
		"host-release-uploaded-projection.json",
		"(length == 6) and",
		"test(\"^sha256:[0-9a-f]{64}$\")",
		"diff -u \"${RUNNER_TEMP}/host-release-expected-assets.json\" \"${uploaded_projection}\"",
		"- name: Attest Worker archives",
		"artifacts/autostream-worker_${{ needs.release-host.outputs.version }}_linux_amd64.tar.gz",
		"artifacts/autostream-worker_${{ needs.release-host.outputs.version }}_linux_arm64.tar.gz",
		"- name: Attest release manifest",
		"- name: Publish verified release atomically",
		"IMMUTABILITY_TOKEN: ${{ secrets.AUTOSTREAM_RELEASE_SETTINGS_TOKEN || github.token }}",
		"Release ${RELEASE_VERSION} appeared during staging; refusing to overwrite it",
		"Cannot re-confirm immutable releases immediately before publication",
		"gh api --method POST \"repos/${GITHUB_REPOSITORY}/git/refs\"",
		"-f ref=\"refs/tags/${RELEASE_VERSION}\"",
		"-f sha=\"${GITHUB_SHA}\"",
		"host-release-owned-final-tag",
		"Created tag ${RELEASE_VERSION} does not resolve to workflow commit ${GITHUB_SHA}",
		"gh api --method PATCH \"repos/${GITHUB_REPOSITORY}/releases/${DRAFT_RELEASE_ID}\"",
		"--argjson release_id \"${DRAFT_RELEASE_ID}\"",
		"(.id == $release_id)",
		"(.immutable == true)",
		"(.body | type == \"string\")",
		`(.body | gsub("\\s"; "") | length > 0)`,
		"(.assets | length == 6)",
		"host-release-published-notes.md",
		"jq -j '.body' \"${published_json}\" > \"${published_notes_path}\"",
		"cmp -s \"${RUNNER_TEMP}/host-release-notes.md\" \"${published_notes_path}\"",
		"published_notes_digest=\"sha256:$(sha256sum \"${published_notes_path}\"",
		"host-release-published-assets.json",
		"\"${RUNNER_TEMP}/host-release-expected-assets.json\" \\",
		"\"${RUNNER_TEMP}/host-release-published-assets.json\"",
		"Published tag ${RELEASE_VERSION} does not resolve to workflow commit ${GITHUB_SHA}",
		"host-release-postpublish-staging-tag.json",
		"Refusing to delete staging tag ${DRAFT_TAG}: it is not the workflow-owned commit",
		"gh api --method DELETE \"repos/${GITHUB_REPOSITORY}/git/refs/tags/${DRAFT_TAG}\"",
		"- name: Reconcile staging state without deleting releases or tags",
		"Automatic release deletion is forbidden",
		"Automatic tag deletion is forbidden",
		"requires manual recovery",
		"published-but-unverified",
	}
	position := 0
	for _, marker := range orderedContract {
		relative := strings.Index(workflow[position:], marker)
		if relative < 0 {
			t.Fatalf("release workflow is missing ordered immutable-publication marker %q", marker)
		}
		position += relative + len(marker)
	}

	if strings.Contains(workflow, "gh api --method DELETE \"repos/${GITHUB_REPOSITORY}/releases/${DRAFT_RELEASE_ID}\"") {
		t.Fatal("release workflow must never automatically delete a release")
	}
	if strings.Contains(workflow, "gh api --method DELETE \"repos/${GITHUB_REPOSITORY}/git/refs/tags/${RELEASE_VERSION}\"") {
		t.Fatal("release workflow must never automatically delete the final release tag")
	}
	cleanupStart := strings.Index(workflow, "- name: Reconcile staging state without deleting releases or tags")
	if cleanupStart < 0 || strings.Contains(workflow[cleanupStart:], "gh api --method DELETE") {
		t.Fatal("failure reconciliation must not contain any DELETE mutation")
	}
	stagingTagDelete := "gh api --method DELETE \"repos/${GITHUB_REPOSITORY}/git/refs/tags/${DRAFT_TAG}\""
	if count := strings.Count(workflow, stagingTagDelete); count != 1 {
		t.Fatalf("release workflow must delete the staging tag only during verified publication, got %d occurrences", count)
	}
}

func TestHostReleaseWorkflowEmbedsArchiveOnlyManualInstallManifest(t *testing.T) {
	workflowPath := filepath.Join("..", "..", ".github", "workflows", "release-host.yml")
	payload, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(payload)

	for _, marker := range []string{
		`> "${root}/artifact-manifest.json"`,
		`"s/vX\\.Y\\.Z/${version}/g"`,
		`"s/<VERSION>/${version}/g"`,
		`"s/<ARCH>/${arch}/g"`,
		`"s/linux_amd64/linux_${arch}/g"`,
		`packaged README.install.md still contains a version placeholder`,
		`["archive", "build_date", "commit", "compatibility", "component", "platform", "schema_version", "source_version"]`,
		`component: "worker"`,
		`minimum_agent_version: "v1.0.0"`,
		`minimum_panel_version: null`,
		`rollback_compatible: true`,
		`database_schema: "none"`,
		`artifact_manifest_member="${artifact_root}/artifact-manifest.json"`,
		`tar -xOzf "${path}" "${artifact_manifest_member}" > "${artifact_manifest_path}"`,
		`"${embedded_manifest_sha}  ./artifact-manifest.json"`,
		`(( size > 268435456 ))`,
		`(.size | type == "number" and . > 0 and . <= 268435456)`,
		`--slurpfile embedded "${artifact_manifest_path}"`,
		`(.components[0].service == $embedded[0].component)`,
		`(.components[0].source_version == $embedded[0].source_version)`,
		`(.components[0].commit == $embedded[0].commit)`,
		`($external.name == $embedded[0].archive.name)`,
		`"${artifact_manifest_path}" > /dev/null`,
	} {
		if !strings.Contains(workflow, marker) {
			t.Fatalf("host release workflow is missing embedded artifact manifest marker %q", marker)
		}
	}

	manifestIndex := strings.Index(workflow, `> "${root}/artifact-manifest.json"`)
	checksumsIndex := strings.Index(workflow, `(cd "${root}" && find . -type f ! -path './checksums.txt'`)
	archiveIndex := strings.Index(workflow, `tar -C staging -czf "artifacts/${artifact}.tar.gz"`)
	externalManifestIndex := strings.Index(workflow, `> artifacts/release-manifest.json`)
	if manifestIndex < 0 || checksumsIndex < 0 || archiveIndex < 0 || externalManifestIndex < 0 ||
		manifestIndex >= checksumsIndex || checksumsIndex >= archiveIndex || archiveIndex >= externalManifestIndex {
		t.Fatal("artifact manifest must be checksummed inside the archive before legacy external metadata is generated")
	}
}

func TestHostReleaseVerifiesActualArchiveAndPinsExactLegacyAssets(t *testing.T) {
	workflowPath := filepath.Join("..", "..", ".github", "workflows", "release-host.yml")
	payload, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(payload)

	const stableVersionGuard = `if [[ ! "${version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then`
	if !strings.Contains(workflow, stableVersionGuard) {
		t.Fatal("stable Host Release workflow must reject prerelease version suffixes")
	}
	for _, marker := range []string{
		`bash -n .github/scripts/verify-release-archive.sh`,
		`bash -n .github/scripts/test-verify-release-archive.sh`,
		`bash .github/scripts/test-verify-release-archive.sh .github/scripts/verify-release-archive.sh`,
		`bash .github/scripts/verify-release-archive.sh "artifacts/${artifact}.tar.gz" "${artifact}"`,
	} {
		if !strings.Contains(workflow, marker) {
			t.Fatalf("Host Release workflow is missing archive verifier marker %q", marker)
		}
	}

	assertExactExpectedReleaseAssets(t, workflow, []string{
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_amd64.tar.gz",
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_amd64.tar.gz.sha256",
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_arm64.tar.gz",
		"${ARTIFACT_PREFIX}_${RELEASE_VERSION}_linux_arm64.tar.gz.sha256",
		"release-manifest.json",
		"release-manifest.json.sha256",
	})
	assertReleaseArchiveVerifierCoverage(t)
}

func assertExactExpectedReleaseAssets(t *testing.T, workflow string, expected []string) {
	t.Helper()

	lines := strings.Split(workflow, "\n")
	var actual []string
	inExpectedNames := false
	foundBlock := false
	for _, line := range lines {
		if strings.Contains(line, `cat > "${expected_names}" <<EOF`) {
			if foundBlock {
				t.Fatal("release workflow contains multiple expected_names heredocs")
			}
			foundBlock = true
			inExpectedNames = true
			continue
		}
		if !inExpectedNames {
			continue
		}
		name := strings.TrimSpace(line)
		if name == "EOF" {
			inExpectedNames = false
			continue
		}
		if name == "" {
			t.Fatal("expected_names heredoc contains an empty asset name")
		}
		actual = append(actual, name)
	}
	if !foundBlock || inExpectedNames {
		t.Fatal("release workflow expected_names heredoc is missing or unterminated")
	}

	seen := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if _, duplicate := seen[name]; duplicate {
			t.Fatalf("release workflow expected_names contains duplicate %q", name)
		}
		seen[name] = struct{}{}
	}
	if got, want := strings.Join(actual, "\n"), strings.Join(expected, "\n"); got != want {
		t.Fatalf("release workflow expected_names mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func assertReleaseArchiveVerifierCoverage(t *testing.T) {
	t.Helper()

	verifierPath := filepath.Join("..", "..", ".github", "scripts", "verify-release-archive.sh")
	verifier, err := os.ReadFile(verifierPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		`release archive contains a non-file/non-directory member`,
		`release archive contains a non-canonical member name`,
		`release archive contains duplicate canonical member names`,
		`release checksums.txt does not cover the exact regular-file inventory`,
		`sha256sum --check --strict checksums.txt`,
	} {
		if !strings.Contains(string(verifier), marker) {
			t.Fatalf("release archive verifier is missing %q", marker)
		}
	}

	fixturePath := filepath.Join("..", "..", ".github", "scripts", "test-verify-release-archive.sh")
	fixture, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"extra-file",
		"missing-checksum",
		"stale-checksum",
		"duplicate-member",
		"symlink-entry",
		"fifo-entry",
		"canonical-alias",
	} {
		if !strings.Contains(string(fixture), marker) {
			t.Fatalf("release archive verifier fixture is missing %q", marker)
		}
	}
}

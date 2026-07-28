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

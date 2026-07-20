package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostReleaseRejectsExistingNamespaceAndPinsDispatchTagToBuildSHA(t *testing.T) {
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
		"gh api --paginate \"repos/${GITHUB_REPOSITORY}/releases?per_page=100\"",
		"select(.tag_name == $tag)",
		"already exists (including drafts)",
		"workflow_dispatch may not overwrite or reuse it",
		"- name: Bind workflow-dispatch tag to build commit",
		"if: github.ref_type != 'tag'",
		"gh api --method POST \"repos/${GITHUB_REPOSITORY}/git/refs\"",
		"-f ref=\"refs/tags/${RELEASE_VERSION}\"",
		"-f sha=\"${GITHUB_SHA}\"",
		"Created tag ${RELEASE_VERSION} does not resolve to workflow commit ${GITHUB_SHA}",
		"uses: softprops/action-gh-release@",
		"target_commitish: ${{ github.sha }}",
		"fail_on_unmatched_files: true",
		"overwrite_files: false",
		"- name: Verify published release tag",
		".draft == false",
		"is not a published final release",
		"does not resolve to workflow commit ${GITHUB_SHA}",
	}
	position := 0
	for _, marker := range orderedContract {
		relative := strings.Index(workflow[position:], marker)
		if relative < 0 {
			t.Fatalf("release workflow is missing ordered immutable-publication marker %q", marker)
		}
		position += relative + len(marker)
	}
}

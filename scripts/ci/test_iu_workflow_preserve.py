import importlib.util
import pathlib
import unittest

SPEC = importlib.util.spec_from_file_location("iu_workflow_preserve", pathlib.Path(__file__).with_name("iu-workflow-preserve.py"))
GUARD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARD)


class WorkflowPreservationTests(unittest.TestCase):
    baseline = "on: [pull_request]\njobs:\n  old:\n    steps:\n      - run: go test ./...\n"

    def test_append_test_job_passes(self):
        GUARD.verify(self.baseline, self.baseline + "  iu:\n    steps:\n      - run: bash scripts/ci/iu-checks.sh\n", "append")

    def test_legacy_step_change_fails(self):
        with self.assertRaises(AssertionError):
            GUARD.verify(self.baseline, self.baseline.replace("go test ./...", "true") + "  iu: {}\n", "append")

    def test_added_publisher_fails(self):
        for command in ["docker push example/image", "gh release create v1", "git push origin main", "uses: docker/build-push-action@abcdef", "contents: write", "${{ secrets.REAL_PROVIDER }}"]:
            with self.subTest(command=command), self.assertRaises(AssertionError):
                GUARD.verify(self.baseline, self.baseline + "      - run: " + command + "\n", "append")

    def test_updater_exact_two_extensions_only(self):
        before = GUARD.UPDATER_CHECKOUT + GUARD.UPDATER_PIN + "      - run: go test ./...\n"
        after = before.replace(GUARD.UPDATER_CHECKOUT, GUARD.UPDATER_CHECKOUT_NEW).replace(GUARD.UPDATER_PIN, GUARD.UPDATER_PIN + GUARD.UPDATER_IU)
        GUARD.verify(before, after, "updater")
        with self.assertRaises(AssertionError):
            GUARD.verify(before, after.replace("go test ./...", "true"), "updater")


if __name__ == "__main__":
    unittest.main()

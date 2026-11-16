"""Negative gate fixtures are independent of the candidate product output."""
import importlib.util
import json
import pathlib
import tempfile
import unittest

SPEC = importlib.util.spec_from_file_location("iu_json_events", pathlib.Path(__file__).with_name("iu-json-events.py"))
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)


def event(action, test=None, package="example/product"):
    result = {"Action": action, "Package": package}
    if test is not None:
        result["Test"] = test
    return json.dumps(result)


class RequiredExecutionTests(unittest.TestCase):
    required = [("example/product", "TestProductBoundary")]

    def valid_events(self):
        return [event("run", "TestProductBoundary"), event("pass", "TestProductBoundary"), event("pass")]

    def test_actual_execution_passes(self):
        self.assertEqual(GATE.verify(self.required, self.valid_events()), 1)

    def test_missing_root_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, [event("pass")])

    def test_pass_without_run_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, [event("pass", "TestProductBoundary"), event("pass")])

    def test_subtest_skip_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, self.valid_events() + [event("skip", "TestProductBoundary/required")])

    def test_package_failure_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, self.valid_events() + [event("fail")])

    def test_root_failure_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, [event("run", "TestProductBoundary"), event("fail", "TestProductBoundary")])

    def test_wrong_package_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, [event("run", "TestProductBoundary", "other/package"), event("pass", "TestProductBoundary", "other/package"), event("pass", package="other/package")])

    def test_incomplete_package_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, self.valid_events()[:-1])

    def test_duplicate_execution_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, self.valid_events() + self.valid_events())

    def test_pass_before_run_fails(self):
        with self.assertRaises(ValueError):
            GATE.verify(self.required, [event("pass", "TestProductBoundary"), event("run", "TestProductBoundary"), event("pass")])

    def test_malformed_event_fails(self):
        for malformed in ["not json", "[]", "{}"]:
            with self.subTest(malformed=malformed), self.assertRaises(ValueError):
                GATE.verify(self.required, self.valid_events() + [malformed])

    def test_inventory_rejects_empty_duplicate_and_subtest(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = pathlib.Path(temporary, "roots.txt")
            for contents in ["# empty\n", "example/product TestProductBoundary\nexample/product TestProductBoundary\n", "example/product TestProductBoundary/subcase\n"]:
                path.write_text(contents, encoding="utf-8")
                with self.subTest(contents=contents), self.assertRaises(ValueError):
                    GATE.load_inventory(path)


if __name__ == "__main__":
    unittest.main()

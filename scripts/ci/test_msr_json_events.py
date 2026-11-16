import importlib.util
import io
import json
import pathlib
import unittest

spec = importlib.util.spec_from_file_location("msr_events", pathlib.Path(__file__).with_name("msr-json-events.py"))
events = importlib.util.module_from_spec(spec)
spec.loader.exec_module(events)
PACKAGE = "github.com/example/autostream-control-panel/internal/httpapi"
REQUIRED = [(PACKAGE, "TestMSR01ManualStopPersistsHold")]


def execution():
    return [{"Action": "run", "Package": PACKAGE, "Test": REQUIRED[0][1]},
            {"Action": "pass", "Package": PACKAGE, "Test": REQUIRED[0][1]},
            {"Action": "pass", "Package": PACKAGE}]


class MSREventTests(unittest.TestCase):
    def verify(self, candidate):
        return events.verify(REQUIRED, io.StringIO("\n".join(json.dumps(row) for row in candidate)))

    def test_actual_root_and_package_are_required(self):
        self.assertEqual(self.verify(execution()), 1)
        for candidate in [execution()[1:], execution()[:-1], execution() + execution(), []]:
            with self.subTest(candidate=candidate), self.assertRaises(ValueError):
                self.verify(candidate)

    def test_skip_fail_and_unreviewed_roots_cannot_hide_under_pass(self):
        for action, name in [("skip", REQUIRED[0][1] + "/lost_ack"), ("fail", REQUIRED[0][1]), ("run", "TestOther"), ("build-fail", None)]:
            candidate = execution() + [{"Action": action, "Package": PACKAGE, "Test": name}]
            with self.subTest(action=action, name=name), self.assertRaises(ValueError):
                self.verify(candidate)

    def test_malformed_json_and_wrong_package_are_rejected(self):
        with self.assertRaises(ValueError):
            events.verify(REQUIRED, ["not-json"])
        candidate = execution()
        candidate[0]["Package"] += "/different"
        with self.assertRaises(ValueError):
            self.verify(candidate)

    def test_required_delayed_start_case_cannot_be_replaced_by_root_pass(self):
        child = (PACKAGE, REQUIRED[0][1] + "/claim_before_dispatch")
        with self.assertRaises(ValueError):
            events.verify(REQUIRED, [json.dumps(row) for row in execution()], [child])
        candidate = execution()
        candidate[1:1] = [{"Action": action, "Package": PACKAGE, "Test": child[1]} for action in ["run", "pass"]]
        self.assertEqual(events.verify(REQUIRED, [json.dumps(row) for row in candidate], [child]), 1)


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Independently retain all legacy CI steps, pins, triggers and interop."""
import json
import pathlib
import re
import subprocess

WORKFLOWS = {
    "18e4639462957e26fea2a3a45a734af67dc9ef93": (".github/workflows/v3-authorization-ci.yml", "append"),
    "cdc106ab795623e41e2fe42a73c598c782e5c8de": (".github/workflows/v3-dev-ci.yml", "updater"),
    "8c17e7389ccdb3429f1e59a1a6147b27fe3c3793": (".github/workflows/ci.yml", "append"),
    "381790940e0fed8532d9396f0a2a40c0bfd0a612": (".github/workflows/ci.yml", "append"),
    "a05e4d24bc9ffef54244dc5424b32d090f87e849": (".github/workflows/ci.yml", "append"),
}
UPDATER_CHECKOUT = "          persist-credentials: false\n          submodules: false\n"
UPDATER_CHECKOUT_NEW = "          persist-credentials: false\n          fetch-depth: 0\n          submodules: false\n"
UPDATER_PIN = "      - name: Verify pinned Contracts evidence source\n        run: bash scripts/ci/verify-v3-evidence-module.sh\n"
UPDATER_IU = "      - name: Required IU-WS4 product preflight and independent-process inventory\n        run: bash scripts/ci/iu-checks.sh\n"


def verify(before, after, mode):
    if mode == "append":
        assert after.startswith(before), "existing CI bytes changed instead of an additive extension"
        added = after[len(before):]
    elif mode == "updater":
        assert before.count(UPDATER_CHECKOUT) == 1 and before.count(UPDATER_PIN) == 1, "Updater CI anchors drifted"
        expected = before.replace(UPDATER_CHECKOUT, UPDATER_CHECKOUT_NEW, 1)
        expected = expected.replace(UPDATER_PIN, UPDATER_PIN + UPDATER_IU, 1)
        assert after == expected, "Updater existing CI changed beyond exact additive checkout/inventory extensions"
        added = UPDATER_IU
    else:
        raise AssertionError("unknown workflow preservation mode")
    forbidden = r"(?:\bdocker\s+(?:push|login)\b|\bgh\s+release\b|\bgit\s+push\b|action-gh-release|build-push-action|(?:packages|contents):\s*write|workflow_dispatch:|workflow_call:|\$\{\{\s*secrets\.)"
    assert not re.search(forbidden, added, flags=re.IGNORECASE), "IU CI extension contains a publication/credential/dispatch path"


def main():
    manifest = json.loads(pathlib.Path("scripts/ci/iu-source-patches.json").read_text(encoding="utf-8"))
    baseline = manifest["baseline"]
    workflow, mode = WORKFLOWS[baseline]
    changed = subprocess.check_output(["git", "diff", "--name-only", baseline, "HEAD", "--", ".github/workflows"]).decode().splitlines()
    assert changed == [workflow], "unrelated/release Workflow changed"
    before = subprocess.check_output(["git", "show", baseline + ":" + workflow]).decode("utf-8").replace("\r\n", "\n")
    after = pathlib.Path(workflow).read_text(encoding="utf-8")
    verify(before, after, mode)
    print("IU CI extension: PASS; all legacy steps, pins, triggers and interop preserved")


if __name__ == "__main__":
    main()

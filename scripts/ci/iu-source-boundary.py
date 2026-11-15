#!/usr/bin/env python3
"""Verify the complete reviewed IU delta without weakening historical guards."""
import hashlib
import json
import pathlib
import subprocess
import sys

BASELINE = "381790940e0fed8532d9396f0a2a40c0bfd0a612"
MANIFEST = "scripts/ci/iu-source-patches.json"


def git(*args):
    return subprocess.check_output(["git", *args]).decode("utf-8")


def canonical(data):
    return data.replace(b"\r\n", b"\n")


def digest(data):
    return hashlib.sha256(canonical(data)).hexdigest()


def main():
    manifest = json.loads(pathlib.Path(MANIFEST).read_text(encoding="utf-8"))
    assert manifest["schema"] == 1 and manifest["baseline"] == BASELINE, "IU manifest baseline/version changed"
    new = manifest["new"]
    patches = manifest["patches"]
    assert not set(new).intersection(patches), "ambiguous IU ownership"
    assert MANIFEST not in new and MANIFEST not in patches, "manifest must not describe itself"
    owned = set(new) | set(patches) | {MANIFEST}
    for path in owned:
        parsed = pathlib.PurePosixPath(path)
        assert not parsed.is_absolute() and ".." not in parsed.parts and "\\" not in path, "IU path escapes Repository"
    if len(sys.argv) == 3 and sys.argv[1] == "--owns":
        # Callers must run the complete guard first. Membership alone is never
        # a validation result and cannot exempt a file from reconstruction.
        return 0 if sys.argv[2] in owned else 1
    assert len(sys.argv) == 1, "unsupported IU boundary arguments"
    subprocess.run(["git", "diff", "--exit-code", "HEAD", "--"], check=True)
    subprocess.run(["git", "merge-base", "--is-ancestor", BASELINE, "HEAD"], check=True)
    changed = {}
    for line in git("diff", "--name-status", BASELINE, "HEAD").splitlines():
        status, path = line.split("\t", 1)
        assert status in {"A", "M"}, "IU removed or renamed preserved source: " + path
        assert not (path.endswith(".sql") and status != "A"), "IU changed an existing Migration: " + path
        changed[path] = status
    assert set(changed) == owned, "IU changed paths differ from exact reviewed manifest"
    for path, expected in new.items():
        assert changed[path] == "A", "existing source reclassified as new: " + path
        assert digest(pathlib.Path(path).read_bytes()) == expected, "IU new source digest differs: " + path
    for path, patch in patches.items():
        assert changed[path] == "M", "IU reviewed source disappeared: " + path
        original = canonical(subprocess.check_output(["git", "show", BASELINE + ":" + path]))
        assert digest(original) == patch["before_sha256"], "IU original source differs: " + path
        expected = original.decode("utf-8")
        for edit in patch["edits"]:
            assert edit["old"] and expected.count(edit["old"]) == 1, "IU exact edit anchor drifted: " + path
            expected = expected.replace(edit["old"], edit["new"], 1)
        actual = canonical(pathlib.Path(path).read_bytes())
        assert actual == expected.encode("utf-8"), "IU source changed beyond exact reviewed patch: " + path
        assert digest(actual) == patch["after_sha256"], "IU reviewed result digest differs: " + path
    subprocess.run([sys.executable, "scripts/ci/iu-module-pin.py", BASELINE], check=True)
    subprocess.run([sys.executable, "scripts/ci/iu-workflow-preserve.py"], check=True)
    print(git("log", "-1", "--format=IU tested commit: %H%nIU tested tree: %T").strip())
    print(f"IU exact source boundary: PASS added={len(new)} modified={len(patches)} metadata=1")
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Permit only the reviewed independent lifecycle module dependency addition."""
import argparse
import json
import os
import pathlib
import subprocess
import tempfile

MODULE = "github.com/Kome-Lab/Autostream-Contracts/v3lifecycle"
VERSION = "v0.0.0-20261115145906-d32ea3514b1c"
COMMIT = "d32ea3514b1c1010391caae0348b0ff9e751f447"
ZIP_SUM = "h1:HfswOPIR7VcFoko8a5LL0YqtRP1UGc6wfuFyggBOIXo="
MOD_SUM = "h1:4mdtG844J5jWEQgB78LBHQEjzbhK+qc0ACLS7+Vlmzw="


def command(*args):
    environment = dict(os.environ, GOWORK="off", GOTOOLCHAIN="local", GOTELEMETRY="off")
    return subprocess.check_output(args, env=environment).decode("utf-8")


def parsed_module(contents, directory, name):
    path = pathlib.Path(directory, name + ".mod")
    path.write_text(contents, encoding="utf-8", newline="\n")
    return json.loads(command("go", "mod", "edit", "-json", "-modfile=" + str(path)))


def verify_delta(baseline):
    original = command("git", "show", baseline + ":go.mod")
    current = pathlib.Path("go.mod").read_text(encoding="utf-8")
    with tempfile.TemporaryDirectory(prefix="autostream-iu-module-") as temporary:
        before = parsed_module(original, temporary, "before")
        after = parsed_module(current, temporary, "after")
    assert before.get("Go") == after.get("Go") == "1.26.5", "Go version changed"
    assert not any(item["Path"] == MODULE for item in before.get("Require", [])), "baseline unexpectedly contains lifecycle module"
    added = [item for item in after.get("Require", []) if item["Path"] == MODULE]
    assert added == [{"Path": MODULE, "Version": VERSION}], "lifecycle require is absent, indirect or unpinned"
    after["Require"] = [item for item in after["Require"] if item["Path"] != MODULE]
    assert before == after, "existing module/API dependency pin changed"
    old_sums = command("git", "show", baseline + ":go.sum").splitlines()
    new_sums = pathlib.Path("go.sum").read_text(encoding="utf-8").splitlines()
    expected = [f"{MODULE} {VERSION} {ZIP_SUM}", f"{MODULE} {VERSION}/go.mod {MOD_SUM}"]
    assert all(new_sums.count(line) == 1 for line in expected), "lifecycle checksums changed or missing"
    assert [line for line in new_sums if line not in expected] == old_sums, "existing go.sum pin changed"


def verify_download():
    selected = json.loads(command("go", "list", "-m", "-json", MODULE))
    assert selected.get("Version") == VERSION and not selected.get("Replace"), "selected module is replaced or unpinned"
    downloaded = json.loads(command("go", "mod", "download", "-json", MODULE + "@" + VERSION))
    assert not downloaded.get("Error"), "module download failed"
    assert downloaded.get("Sum") == ZIP_SUM and downloaded.get("GoModSum") == MOD_SUM, "downloaded lifecycle bytes differ"
    if downloaded.get("Origin"):
        assert downloaded["Origin"].get("Hash") == COMMIT, "module origin commit differs"
        assert downloaded["Origin"].get("Subdir") == "v3lifecycle", "module origin subdirectory differs"
    print(f"IU lifecycle module: {VERSION} commit={COMMIT} zip={ZIP_SUM} go.mod={MOD_SUM}")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("baseline")
    parser.add_argument("--download", action="store_true")
    args = parser.parse_args()
    verify_delta(args.baseline)
    if args.download:
        verify_download()
    print("IU module addition and every existing pin: PASS")


if __name__ == "__main__":
    main()

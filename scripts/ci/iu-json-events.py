#!/usr/bin/env python3
"""Require actual Go JSON root execution; expectations are reviewed inventory data."""
import argparse
import json
import pathlib
import sys


def load_inventory(path):
    required = []
    for number, line in enumerate(pathlib.Path(path).read_text(encoding="utf-8").splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        fields = line.split()
        if len(fields) != 2 or not fields[1].startswith("Test") or "/" in fields[1]:
            raise ValueError(f"invalid package/root inventory line {number}")
        item = tuple(fields)
        if item in required:
            raise ValueError(f"duplicate inventory root at line {number}")
        required.append(item)
    if not required:
        raise ValueError("empty required-root inventory")
    return required


def verify(required, events):
    executions = {}
    package_passes = set()
    for number, line in enumerate(events, 1):
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except (TypeError, ValueError) as error:
            raise ValueError(f"invalid Go JSON event at line {number}") from error
        if not isinstance(event, dict):
            raise ValueError(f"non-object Go JSON event at line {number}")
        action = event.get("Action")
        package = event.get("Package")
        test = event.get("Test")
        if not isinstance(action, str) or not isinstance(package, str):
            raise ValueError(f"missing Action/Package at event line {number}")
        # A skipped subtest or failed package must fail even when every root has
        # a pass event. Cancellation, build errors and panics cannot hide here.
        if action in {"skip", "fail"}:
            raise ValueError(f"{action} event: {package} {test or '(package)'}")
        if action == "pass" and test is None:
            package_passes.add(package)
        if test is not None:
            if not isinstance(test, str):
                raise ValueError(f"invalid Test at event line {number}")
            executions.setdefault((package, test), []).append(action)
    for package, root in required:
        actions = executions.get((package, root), [])
        if actions.count("run") != 1 or actions.count("pass") != 1:
            raise ValueError(f"required root did not run and pass exactly once: {package} {root}")
        if actions.index("run") > actions.index("pass"):
            raise ValueError(f"root pass preceded execution: {package} {root}")
        if package not in package_passes:
            raise ValueError(f"required package did not finish successfully: {package}")
    return len(required)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("inventory")
    parser.add_argument("events")
    args = parser.parse_args()
    try:
        required = load_inventory(args.inventory)
        with pathlib.Path(args.events).open(encoding="utf-8") as stream:
            count = verify(required, stream)
    except (OSError, ValueError) as error:
        print(f"IU required execution: FAIL: {error}", file=sys.stderr)
        return 1
    print(f"IU required execution: PASS roots={count} skip=0 fail=0")
    return 0


if __name__ == "__main__":
    sys.exit(main())

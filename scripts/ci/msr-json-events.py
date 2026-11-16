#!/usr/bin/env python3
"""IU-WS2-10 exact required execution, independent of test-produced results."""
import argparse
import collections
import json
import pathlib
import re
import sys


def inventory(path, subtests=False):
    required = []
    for number, line in enumerate(pathlib.Path(path).read_text(encoding="utf-8").splitlines(), 1):
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        fields = line.split()
        name_pattern = r"Test[A-Za-z0-9_]+/[A-Za-z0-9_/-]+" if subtests else r"Test[A-Za-z0-9_]+"
        if len(fields) != 2 or not re.fullmatch(r"github\.com/[A-Za-z0-9_./-]+", fields[0]) or not re.fullmatch(name_pattern, fields[1]):
            raise ValueError(f"invalid package/root inventory line {number}")
        item = tuple(fields)
        if item in required:
            raise ValueError(f"duplicate required root at line {number}")
        required.append(item)
    if not required:
        raise ValueError("empty required MSR inventory")
    return required


def verify(required, lines, required_subtests=()):
    wanted = set(required)
    actions = collections.defaultdict(list)
    packages = collections.Counter()
    observed_roots = set()
    for number, line in enumerate(lines, 1):
        if not line.strip():
            continue
        try:
            event = json.loads(line)
        except (TypeError, ValueError) as error:
            raise ValueError(f"invalid JSON event at line {number}") from error
        if not isinstance(event, dict) or not isinstance(event.get("Package"), str) or not isinstance(event.get("Action"), str):
            raise ValueError(f"missing Package/Action at line {number}")
        package, action, test = event["Package"], event["Action"], event.get("Test")
        if action in {"skip", "fail", "build-fail"}:
            raise ValueError(f"{action} event: {package} {test or '(package)'}")
        if package not in {item[0] for item in wanted}:
            raise ValueError(f"unexpected package: {package}")
        if test is None:
            if action == "pass":
                packages[package] += 1
            continue
        if not isinstance(test, str) or not test:
            raise ValueError(f"invalid Test at line {number}")
        root = test.split("/", 1)[0]
        if (package, root) not in wanted:
            raise ValueError(f"unreviewed root execution: {package} {root}")
        actions[(package, test)].append(action)
        if "/" not in test:
            observed_roots.add((package, test))
    if observed_roots != wanted:
        raise ValueError("required root inventory differs from actual root execution")
    for item in [*required, *required_subtests]:
        observed = actions[item]
        if observed.count("run") != 1 or observed.count("pass") != 1 or observed.index("run") > observed.index("pass"):
            raise ValueError(f"root did not run/pass exactly once: {item}")
        if packages[item[0]] != 1:
            raise ValueError(f"package did not complete exactly once: {item[0]}")
    return len(required)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--plan", action="store_true")
    parser.add_argument("--subtests")
    parser.add_argument("inventory")
    parser.add_argument("events", nargs="?")
    args = parser.parse_args()
    try:
        required = inventory(args.inventory)
        required_subtests = inventory(args.subtests, subtests=True) if args.subtests else []
        if any((package, name.split("/", 1)[0]) not in required for package, name in required_subtests):
            raise ValueError("required subtest does not belong to a reviewed root")
        if args.plan:
            if args.events:
                raise ValueError("plan takes an inventory only")
            for package in dict.fromkeys(item[0] for item in required):
                names = [root for owner, root in required if owner == package]
                print(package + "\t^(" + "|".join(names) + ")$")
            return 0
        if not args.events:
            raise ValueError("actual JSON event file is required")
        with pathlib.Path(args.events).open(encoding="utf-8") as events:
            count = verify(required, events, required_subtests)
    except (OSError, ValueError) as error:
        print(f"MSR required execution: FAIL: {error}", file=sys.stderr)
        return 1
    print(f"MSR required execution: PASS roots={count} required_subtests={len(required_subtests)} skip=0 fail=0")
    return 0


if __name__ == "__main__":
    sys.exit(main())

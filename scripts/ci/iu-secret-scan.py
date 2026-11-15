#!/usr/bin/env python3
"""Check the reviewed publication delta; never print matching secret bytes."""
import json
import pathlib
import re

manifest = json.loads(pathlib.Path("scripts/ci/iu-source-patches.json").read_text(encoding="utf-8"))
patterns = {
    "GitHub token material": re.compile(rb"\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{40,})\b"),
    "AWS access key material": re.compile(rb"\b(?:AKIA|ASIA)[A-Z0-9]{16}\b"),
    "private key material": re.compile(rb"-----BEGIN (?:RSA |EC |OPENSSH |DSA |ENCRYPTED )?PRIVATE KEY-----"),
    "bcrypt password hash": re.compile(rb"\$2[aby]\$[0-9]{2}\$[./A-Za-z0-9]{53}"),
    "personal Windows path": re.compile(rb"[A-Za-z]:\\Users\\[^\\\s\"']+\\"),
    "credential-bearing connection URL": re.compile(rb"(?:postgres(?:ql)?|mysql|mariadb|https?)://[^\s/:@]+:[^\s/@]+@"),
}
failures = []
for name in sorted(set(manifest["new"]) | set(manifest["patches"])):
    data = pathlib.Path(name).read_bytes()
    for reason, pattern in patterns.items():
        if pattern.search(data):
            failures.append(f"{name}: {reason}")
assert not failures, "publication secret/redaction scan failed:\n" + "\n".join(failures)
print("IU publication delta secret/redaction patterns: PASS (no matched bytes emitted)")

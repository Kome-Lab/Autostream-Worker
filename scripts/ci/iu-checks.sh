#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly GOTELEMETRY=off
: "${RUNNER_TEMP:?IU checks require an explicit disposable output directory}"
baseline=381790940e0fed8532d9396f0a2a40c0bfd0a612
test "$(go env GOVERSION)" = go1.26.5
python3 -B -m unittest discover -s scripts/ci -p 'test_iu_*.py' -v
python3 scripts/ci/iu-source-boundary.py
python3 scripts/ci/iu-module-pin.py "$baseline" --download
python3 scripts/ci/iu-secret-scan.py
mapfile -t files < <(git diff --name-only --diff-filter=AM "$baseline" HEAD -- '*.go')
test "${#files[@]}" -gt 0
test -z "$(gofmt -l "${files[@]}")"
for mode in native cgo-free race; do
  flags=()
  if test "$mode" = race; then flags=(-race); fi
  if test "$mode" = cgo-free; then export CGO_ENABLED=0; else export CGO_ENABLED=1; fi
  go test -p 1 -count=1 -timeout=10m "${flags[@]}" -json ./internal/jobs -run '^TestIUWS3' | tee "$RUNNER_TEMP/iu-$mode.json"
  python3 scripts/ci/iu-json-events.py scripts/ci/iu-required-tests.txt "$RUNNER_TEMP/iu-$mode.json"
done
go vet -p 1 ./...
mkdir -p "$RUNNER_TEMP/iu-cross"
for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -p 1 -trimpath ./internal/jobs
  output="$RUNNER_TEMP/iu-cross/jobs-linux-$arch.test"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go test -p 1 -trimpath -c ./internal/jobs -o "$output"
  go version -m "$output"
  sha256sum "$output"
done
# Existing go job retains full package tests/build and its decomposed Race suite.
# The amd64/arm64 binaries above are compilation evidence, not native execution.
git diff --exit-code -- go.mod go.sum
python3 scripts/ci/iu-source-boundary.py

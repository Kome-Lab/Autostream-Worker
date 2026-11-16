#!/usr/bin/env bash
set -euo pipefail
export GOTOOLCHAIN=local GOWORK=off GOFLAGS=-mod=readonly GOTELEMETRY=off
: "${RUNNER_TEMP:?MSR requires disposable evidence storage}"
test "$(go env GOVERSION)" = go1.26.5
baseline="$(python3 -c 'import json; print(json.load(open("scripts/ci/iu-source-patches.json"))["baseline"])')"
python3 -B -m unittest discover -s scripts/ci -p 'test_msr_*.py' -v
python3 scripts/ci/iu-source-boundary.py
python3 scripts/ci/iu-module-pin.py "$baseline" --download
python3 scripts/ci/iu-secret-scan.py
mapfile -t changed_go < <(git diff --name-only --diff-filter=AM "$baseline" HEAD -- '*.go')
test "${#changed_go[@]}" -gt 0
test -z "$(gofmt -l "${changed_go[@]}")"
module="$(go list -m)"
case "$module" in
  github.com/example/autostream-control-panel)
    test "${AUTOSTREAM_MSR_REQUIRED:-}" = 1
    : "${AUTOSTREAM_MARIADB_TEST_DSN:?strict MSR requires a disposable schema}"
    : "${AUTOSTREAM_MSR_ENCODER_TEST_BINARY:?actual isolated Encoder process is required}"
    : "${AUTOSTREAM_MSR_WORKER_TEST_BINARY:?actual isolated Worker process is required}"
    : "${AUTOSTREAM_MSR_BOT_TEST_BINARY:?actual isolated Bot process is required}"
    : "${AUTOSTREAM_MSR_ENCODER_RACE_TEST_BINARY:?Race-instrumented Encoder process is required}"
    : "${AUTOSTREAM_MSR_WORKER_RACE_TEST_BINARY:?Race-instrumented Worker process is required}"
    : "${AUTOSTREAM_MSR_BOT_RACE_TEST_BINARY:?Race-instrumented Bot process is required}"
    ;;
  github.com/example/autostream-discord-bot|github.com/example/autostream-encoder-recorder|github.com/example/autostream-worker) ;;
  *) echo 'Unreviewed MSR module' >&2; exit 1 ;;
esac
for mode in native cgo-free race; do
  flags=()
  inventory=scripts/ci/msr-required-tests.txt
  if test "$mode" = race; then flags=(-race); fi
  if test "$module" = github.com/example/autostream-control-panel && test "$mode" = race; then
    export AUTOSTREAM_MSR_ENCODER_TEST_BINARY="$AUTOSTREAM_MSR_ENCODER_RACE_TEST_BINARY"
    export AUTOSTREAM_MSR_WORKER_TEST_BINARY="$AUTOSTREAM_MSR_WORKER_RACE_TEST_BINARY"
    export AUTOSTREAM_MSR_BOT_TEST_BINARY="$AUTOSTREAM_MSR_BOT_RACE_TEST_BINARY"
  fi
  if test "$mode" = cgo-free; then
    export CGO_ENABLED=0
    if test "$module" = github.com/example/autostream-discord-bot; then inventory=scripts/ci/msr-pure-required-tests.txt; fi
  else export CGO_ENABLED=1; fi
  plan="$RUNNER_TEMP/msr-$mode-plan.txt"
  python3 scripts/ci/msr-json-events.py --plan "$inventory" > "$plan"
  result="$RUNNER_TEMP/msr-$mode.json"
  : > "$result"
  while IFS=$'\t' read -r package roots; do
    go test -p 1 -count=1 -timeout=15m "${flags[@]}" -json "$package" -run "$roots" | tee -a "$result"
  done < "$plan"
  extra=()
  if test -f scripts/ci/msr-required-subtests.txt; then extra=(--subtests scripts/ci/msr-required-subtests.txt); fi
  python3 scripts/ci/msr-json-events.py "${extra[@]}" "$inventory" "$result"
done
export CGO_ENABLED=1
go vet -p 1 ./...
go build -p 1 ./...
for arch in amd64 arm64; do
  if test "$module" = github.com/example/autostream-discord-bot; then
    # DAVE is a pinned native prerequisite. Pure product adapters are compiled
    # here; this is not a CGO-free or arm64 qualification of the voice binary.
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build ./internal/control ./internal/manualhold
  else
    mkdir -p "$RUNNER_TEMP/msr-build-$arch"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -p 1 -o "$RUNNER_TEMP/msr-build-$arch/" ./cmd/...
  fi
done
git diff --exit-code -- go.mod go.sum
python3 scripts/ci/iu-source-boundary.py

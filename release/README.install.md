# AutoStream Worker Host Install

This archive contains the Linux binary, systemd example, and placeholder environment file for AutoStream Worker.

## Requirements

- Linux amd64 or arm64 matching the archive name.
- A dedicated `autostream` user and group.
- Authenticated `gh`, `jq`, `sha256sum`, and `curl` for release verification.
- Network access to the Control Panel and worker event targets.
- Runtime config and service token values supplied outside Git.

## Install a verified managed release

The systemd unit runs the binary through `/opt/autostream/worker/current`. Seed
that link from the same immutable release manifest and checksum that supplied
the archive. `autostream-updater` refuses an unseeded target because it would
have no verified rollback release.

```bash
set -euo pipefail
VERSION="${VERSION:?export VERSION=vX.Y.Z before continuing}"
ARCH="${ARCH:-amd64}"
ASSET="autostream-worker_${VERSION}_linux_${ARCH}.tar.gz"
ARTIFACT_ROOT=/opt/autostream/releases

sudo install -d -o root -g root -m 0755 "$ARTIFACT_ROOT"
sudo install -d -o "$USER" -g "$USER" -m 0755 "$ARTIFACT_ROOT/artifacts"
gh release download "$VERSION" \
  --repo Kome-Lab/Autostream-Worker \
  --pattern "$ASSET" \
  --pattern "$ASSET.sha256" \
  --pattern release-manifest.json \
  --pattern release-manifest.json.sha256 \
  --dir "$ARTIFACT_ROOT/artifacts" \
  --clobber
(cd "$ARTIFACT_ROOT/artifacts" && sha256sum --check --strict "$ASSET.sha256")
(cd "$ARTIFACT_ROOT/artifacts" && sha256sum --check --strict release-manifest.json.sha256)

DIGEST="$(awk 'NR == 1 { print $1 }' "$ARTIFACT_ROOT/artifacts/$ASSET.sha256")"
[[ "$DIGEST" =~ ^[0-9a-f]{64}$ ]]
jq -e --arg version "$VERSION" --arg asset "$ASSET" --arg sha "$DIGEST" \
  '.schema_version == 1 and .release_id == $version and .channel == "host" and
   ([.components[] | select(.service == "worker" and .source_version == $version) |
     .artifacts[] | select(.name == $asset and .sha256 == $sha)] | length == 1)' \
  "$ARTIFACT_ROOT/artifacts/release-manifest.json"

RELEASE_ROOT=/opt/autostream/worker/releases
RELEASE_DIR="$RELEASE_ROOT/${VERSION}-${DIGEST:0:12}"
CURRENT_LINK=/opt/autostream/worker/current
sudo test ! -e "$RELEASE_DIR"
sudo install -d -o root -g root -m 0755 "$RELEASE_DIR"
sudo tar --no-same-owner --strip-components=1 -xzf "$ARTIFACT_ROOT/artifacts/$ASSET" -C "$RELEASE_DIR"
(cd "$RELEASE_DIR" && sha256sum --check --strict checksums.txt)
printf '%s\n' "$DIGEST" | sudo tee "$RELEASE_DIR/.artifact-sha256" >/dev/null
printf '%s\n' "$VERSION" | sudo tee "$RELEASE_DIR/.version" >/dev/null
sudo chown root:root "$RELEASE_DIR/.artifact-sha256" "$RELEASE_DIR/.version"
sudo chmod 0444 "$RELEASE_DIR/.artifact-sha256" "$RELEASE_DIR/.version"
sudo /usr/sbin/runuser -u autostream -- "$RELEASE_DIR/bin/autostream-worker" --version | grep -F -- "$VERSION"

sudo ln -s "$RELEASE_DIR" "${CURRENT_LINK}.next"
sudo mv -Tf "${CURRENT_LINK}.next" "$CURRENT_LINK"
sudo ln -sfn "$CURRENT_LINK/bin/autostream-worker" /usr/local/bin/autostream-worker
sudo ln -sfn /usr/local/bin/autostream-worker /usr/local/bin/worker
sudo install -d -o autostream -g autostream /var/lib/autostream/worker
sudo install -d -o root -g root -m 0750 /etc/autostream
sudo install -o root -g root -m 0644 "$RELEASE_DIR/systemd/autostream-worker.service.example" /etc/systemd/system/autostream-worker.service
if ! sudo test -e /etc/autostream/worker.env; then
  sudo install -o root -g root -m 0640 "$RELEASE_DIR/.env.example" /etc/autostream/worker.env
else
  echo "preserving existing /etc/autostream/worker.env; review .env.example for new settings"
fi
```

Edit `/etc/autostream/worker.env` with real environment-specific values.
Keep this file root-owned and set `AUTOSTREAM_CONFIG_REVISION=1` for the first
applied configuration. Increment the value after each configuration change.
Only an unpadded integer greater than or equal to `1` is accepted; invalid
values make the worker fail closed before it serves HTTP.

`AUTOSTREAM_BIND_ADDR` accepts an arbitrary unprivileged port from `1024`
through `65535`; the shipped systemd env uses the standard IPv4 loopback value
`127.0.0.1:8084`. The binary retains the legacy `127.0.0.1:8080` fallback only
when the variable is absent, so upgrading an older installation does not move
its port. The systemd unit does not hard-code a port. An invalid address or an
out-of-range port makes the worker fail closed during startup.

Set `WORKER_PORT` below to the port component of `AUTOSTREAM_BIND_ADDR`, then
run:

```bash
set -euo pipefail
VERSION="${VERSION:?export VERSION=vX.Y.Z before continuing}"
WORKER_PORT="${WORKER_PORT:-8084}"
PROBE_HOST="${PROBE_HOST:-127.0.0.1}"
[[ "$WORKER_PORT" =~ ^[0-9]+$ ]]
(( WORKER_PORT >= 1024 && WORKER_PORT <= 65535 ))
sudo systemctl daemon-reload
sudo systemctl enable autostream-worker
sudo systemctl restart autostream-worker
PID="$(sudo systemctl show --property=MainPID --value autostream-worker)"
EXPECTED="$(sudo readlink -f /opt/autostream/worker/current/bin/autostream-worker)"
test "$(sudo readlink -f "/proc/$PID/exe")" = "$EXPECTED"
curl --fail --silent --show-error --max-time 10 "http://${PROBE_HOST}:${WORKER_PORT}/health" >/dev/null
test "$(curl --fail --silent --show-error --max-time 10 \
  "http://${PROBE_HOST}:${WORKER_PORT}/updater/version" | jq -r '.version')" = "$VERSION"
```

The standard systemd/local-executor profile uses IPv4 loopback. For an explicit
IPv6 loopback bind such as `AUTOSTREAM_BIND_ADDR=[::1]:18084`, run the smoke
check with `PROBE_HOST='[::1]' WORKER_PORT=18084`; the brackets are required in
the URL.

`/updater/version` is the unauthenticated, identity-bound local executor probe.
Its exact response fields are version, service_id, service_type, and config_revision.
The service ID comes from the same Control Panel node config used by
registration. Block this exact path at any public reverse proxy.

Do not fabricate `.artifact-sha256` or `.version` from an unverified local
binary. Releases without `release-manifest.json` remain manual-only; publish a
new release instead of modifying an existing release asset.

Do not commit real `.env` files, provider credentials, tokens, logs, screenshots, or verification record.

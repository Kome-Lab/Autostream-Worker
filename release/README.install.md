# AutoStream Worker Host Install

The release installer performs the verified managed installation for AutoStream
Worker. Operators keep using the stable paths below:

- Binary: `/usr/local/bin/autostream-worker`
- Compatibility command: `/usr/local/bin/worker`
- Settings: `/etc/autostream/worker.env`
- State: `/var/lib/autostream/worker`

The versioned release directories and the `current` link under
`/opt/autostream/worker` are installer-owned. Do not create or edit those
internal paths by hand.

## Requirements

- Linux amd64 or arm64 matching the downloaded archive.
- Root access through `sudo`.
- `jq`, `sha256sum`, `tar`, and standard Ubuntu system tools.
- An existing `/etc/autostream/worker.env`, when present, owned by `root:root`
  with mode `0600` or `0640`.
- Only the matching
  `autostream-worker_<VERSION>_linux_<ARCH>.tar.gz` archive is required on the
  server. Keep it next to the extracted directory while the installer runs.

## Verify before upload

On an administration workstation with authenticated GitHub CLI access, verify
the one archive before transferring it to the server:

```bash
gh attestation verify autostream-worker_vX.Y.Z_linux_amd64.tar.gz \
  --repo Kome-Lab/Autostream-Worker \
  --signer-workflow Kome-Lab/Autostream-Worker/.github/workflows/release-host.yml \
  --deny-self-hosted-runners
```

Do not upload or execute an archive if verification fails. Internal checksums
detect accidental corruption but are not a substitute for this provenance
check. Transfer the verified archive over an authenticated channel.

## Install

On the server, copy the one uploaded archive into a root-owned staging
directory, extract it, and run the bundled installer:

```bash
sudo install -d -o root -g root -m 0755 /opt/autostream/releases
sudo install -d -o root -g root -m 0755 /opt/autostream/releases/artifacts
sudo install -o root -g root -m 0644 /tmp/autostream-worker_vX.Y.Z_linux_amd64.tar.gz /opt/autostream/releases/artifacts/autostream-worker_vX.Y.Z_linux_amd64.tar.gz
cd /opt/autostream/releases/artifacts
sudo test ! -e autostream-worker_vX.Y.Z_linux_amd64
sudo test ! -L autostream-worker_vX.Y.Z_linux_amd64
sudo tar --no-same-owner --no-same-permissions -xzf autostream-worker_vX.Y.Z_linux_amd64.tar.gz
cd autostream-worker_vX.Y.Z_linux_amd64
sudo ./install-autostream-worker
```

No `.sha256` sidecar or external `release-manifest.json` is needed for this
manual path. Those files remain published for compatible automatic updaters,
but this installer ignores them even when they are present.

The installer takes a stable copy of the adjacent archive, records its
SHA-256 digest, and verifies archive path safety, the exact inner checksum file
set, `artifact-manifest.json`, host architecture, and the binary version,
commit, and build date. It then:

- creates the `autostream` system account when absent;
- installs the verified rollback baseline under `/opt/autostream/worker`;
- creates the stable `/usr/local/bin` links;
- installs the systemd unit;
- creates `/etc/autostream/worker.env` only when it is absent;
- preserves an existing environment file byte-for-byte;
- reloads systemd without starting or restarting the service.

Existing regular binaries at the stable public paths are copied to a durable
root-only backup under `/var/backups/autostream/install-migrations/worker`,
outside the service-writable state directory. A staged symlink then atomically
replaces each regular path without an absent-path window. The deterministic
backup is retained after success. A failed public-path migration restores the
original files.

## Configure and start

For a fresh installation, review the settings and start the service:

```bash
sudo vi /etc/autostream/worker.env
sudo systemctl enable --now autostream-worker
sudo systemctl status autostream-worker
```

For an existing running installation, review the settings and explicitly
restart the old process so it uses the new release:

```bash
sudo vi /etc/autostream/worker.env
sudo systemctl restart autostream-worker
sudo systemctl status autostream-worker
```

Set `AUTOSTREAM_CONFIG_REVISION=1` for the first applied configuration and
increment it after each configuration change. `AUTOSTREAM_BIND_ADDR` accepts an
unprivileged port from `1024` through `65535`; the example uses
`127.0.0.1:8084`.

The host must provide FFmpeg with the `libx264` encoder and `mpegts` muxer, and
a Japanese Noto font. Install the Debian/Ubuntu packages `ffmpeg`, `fontconfig`,
and `fonts-noto-cjk`. Worker uses the path below when the variable is unset;
keep it in `worker.env` only when an explicit override is desired:

```text
AUTOSTREAM_SCENE_FONT_FILE=/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc
```

The Worker fails closed if the selected path is missing, unreadable, or
invalid. It does not silently fall back to a basic font.

Verify the installed command and health endpoint, replacing the port if needed:

```bash
autostream-worker --version
curl --fail --silent --show-error http://127.0.0.1:8084/health
```

Automation may still initialize its IPv4 probe with
`PROBE_HOST="${PROBE_HOST:-127.0.0.1}"`, or use `PROBE_HOST='[::1]'` for an IPv6
loopback bind. These variables are not needed for the direct installation
commands above. The `/updater/version` response contains exactly
version, service_id, service_type, and config_revision. Block this
unauthenticated local-executor probe at any public reverse proxy.

The installer deliberately does not enable, start, restart, or stop the
service. It also does not install Docker or change Docker/Compose resources.

Do not commit real environment files, credentials, tokens, logs, screenshots,
or verification records.

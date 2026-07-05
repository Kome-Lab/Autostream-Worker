# AutoStream Worker Host Install

This archive contains the Linux binary, systemd example, and placeholder environment file for AutoStream Worker.

## Requirements

- Linux amd64 or arm64 matching the archive name.
- A dedicated `autostream` user and group.
- Network access to the Control Panel and worker event targets.
- Runtime config and service token values supplied outside Git.

## Install

```bash
sudo install -o root -g root -m 0755 bin/worker /usr/local/bin/worker
sudo ln -sf /usr/local/bin/worker /usr/local/bin/autostream-worker
sudo install -d -o autostream -g autostream /var/lib/autostream/worker
sudo install -o root -g root -m 0644 systemd/autostream-worker.service.example /etc/systemd/system/autostream-worker.service
sudo install -o root -g root -m 0640 .env.example /etc/autostream/worker.env
```

Edit `/etc/autostream/worker.env` with real environment-specific values, then run:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now autostream-worker
```

Do not commit real `.env` files, provider credentials, tokens, logs, screenshots, or verification record.

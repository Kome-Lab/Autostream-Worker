# autostream-worker

AutoStream の Worker service です。

## 役割

- Control Panel から stream job context を受け取ります。
- overlay、caption、participant、active-speaker、current-time event を生成します。
- Discord Bot の Discord Opus packet を受け取り、Deepgram のリアルタイム字幕を生成します。
- 生成した event を Encoder/Recorder へ送信します。
- Control Panel へ heartbeat と signal を送信し、Control Panel 経由で Observability に反映します。

video layer stream は MVP 後の後続タスクです。

## 主な環境変数

```text
AUTOSTREAM_NODE_CONFIG=/etc/autostream-worker/config.yml
AUTOSTREAM_ENV=production
AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true
AUTOSTREAM_BIND_ADDR=127.0.0.1:8084
AUTOSTREAM_CONFIG_REVISION=1
TZ=Asia/Tokyo
```

`AUTOSTREAM_CONFIG_REVISION` is a root-owned positive integer used by the local
executor to bind `/updater/version` to the applied service configuration.
It defaults to `1`; increment it after a configuration change. Invalid, signed,
fractional, padded, zero, or negative values stop the Worker before it starts
serving HTTP.

The Control Panel local executor writes managed bind/revision overrides to
`/opt/autostream/local-executor/ports/worker.env`. systemd loads this optional root-owned
sidecar after `worker.env`, so managed values win without breaking existing
hosts where the sidecar does not exist.

systemd 版の待受ポートは `/etc/autostream/worker.env` の
`AUTOSTREAM_BIND_ADDR` で変更できます。ポートは非特権範囲の
`1024`～`65535` を指定してください。標準の env ファイルは IPv4
loopback の `127.0.0.1:8084` を明示します。変数自体がない既存環境では、
アップグレードだけでポートを移動しないようバイナリの従来値
`127.0.0.1:8080` を維持します。
例えば `127.0.0.1:18084` に変更した場合、`/health` と
`/updater/version` も同じ `18084` で待ち受けます。不正な形式、範囲外、
または特権ポートを指定した場合は Worker が起動時に安全側で停止します。
IPv6 loopback を明示的に使う場合は `[::1]:18084` のように角括弧を含めて
指定し、プローブURLも `http://[::1]:18084/...` とします。

Docker 版のホスト公開ポートは Compose 実行時の
`AUTOSTREAM_WORKER_PORT` で変更できます。コンテナ内ポートも変更する場合は
`AUTOSTREAM_WORKER_CONTAINER_PORT` を併せて指定します。どちらも
`1024`～`65535` を使用してください。

Compose published ports are a host/reverse-proxy responsibility. The Control
Panel Updater manages only host ports `1024` through `65535`; manually
publishing a privileged or conflicting Docker host port is outside the managed
update contract.

The production health authority is the host Local Executor. These Compose files
intentionally omit an in-container `healthcheck`: the runtime image has no
purpose-built HTTP probe client, and the image contract does not add or repurpose `curl`, `wget`, or another unrelated executable solely for container health.
For managed Docker changes, the Local Executor probes the loopback published port for both `/health` and `/updater/version`; the published port is the health port.
A recreate is accepted only when health, service identity, version, and
`AUTOSTREAM_CONFIG_REVISION` match; otherwise the executor rolls back or reports
`rollback_failed`.

```powershell
$env:AUTOSTREAM_WORKER_PORT = "18084"
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

`AUTOSTREAM_NODE_CONFIG` には Control Panel の Node登録で生成した `config.yml` を指定します。Node Runtime Token と stream-scoped token の検証に使う `stream_ingest.signing_key` はこのファイルに入り、標準構成では `CONTROL_PANEL_TOKEN`、`AUTOSTREAM_STREAM_INGEST_SIGNING_KEY`、`OBSERVABILITY_TOKEN` を env に手入力しません。Worker から Observability へ直接送る互換fallbackを使う場合だけ、`OBSERVABILITY_URL` と `OBSERVABILITY_TOKEN=<OBSERVABILITY_INGEST_TOKEN>` を追加します。

Encoder/Recorder への送信先 URL と worker-event token は、通常は Control Panel の stream job context で `encoder_recorder_url` / `stream_ingest_token` として渡されます。`ENCODER_RECORDER_URL` と `ENCODER_RECORDER_TOKEN` は local migration / dry-run 互換 fallback のみで使い、本番 env には置きません。

`AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true` または `AUTOSTREAM_ENV=production` の場合、Worker は Control Panel registration と runtime config 取得に失敗すると起動を停止します。runtime config に含まれる自 service の primary assignment だけを受け付け、standby または別 Worker に割り当てられた stream の `/jobs/start` は拒否します。

字幕は stream job context の `caption_profile_id` が選択されている場合だけ有効です。Worker は対応する runtime-config の caption profile を厳密に選び、job start 時に Node Runtime Token と対象 `stream_id` で Control Panel の `/services/runtime-secrets/resolve` を一度だけ呼び出して `deepgram_api_key` を取得します。`DEEPGRAM_API_KEY` などの環境変数fallbackはありません。profile 未選択時の `POST /streams/{id}/audio/opus` は `409`、secret取得失敗時の `/jobs/start` は `503` で失敗します。

Deepgram 接続先は `wss://api.deepgram.com/v1/listen` 固定です。Discord Opus は SSRC / speaker ごとの接続へ binary frame で転送し、interim result は `caption.telop`、final result は `caption.final` として Encoder/Recorder へ送信します。

## 開発

```powershell
go test ./...
go build ./...
```

## Security

- Node Runtime Token、Encoder token、Observability token を log / API response に出しません。
- Discord Bot には Worker の Node Runtime Token を渡さず、用途別の stream-scoped `worker_events` / `caption_audio` token だけを受け付けます。
- Discord音声endpointは stream-scoped `caption_audio` tokenを受け付け、`service_type=discord_bot`、`audience=worker`、`stream_id` を検証します。
- Deepgram API key は browser、Discord Bot、log、API response、URL queryへ出しません。
- Encoder/Recorder への token-bearing request は redirect と unsafe HTTP を安全側で拒否します。
- event payload に secret-like value を入れないでください。

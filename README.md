# autostream-worker

AutoStream の Worker service です。

## 役割

- Control Panel から stream job context を受け取ります。
- overlay、caption、participant、active-speaker、current-time event を生成します。
- 生成した event を Encoder/Recorder へ送信します。
- Control Panel へ heartbeat、Observability へ signal を送信します。

video layer stream は MVP 後の後続タスクです。

## 主な環境変数

```text
SERVICE_ID=worker-01
SERVICE_NAME=Worker 01
SERVICE_PUBLIC_URL=https://worker.example.com
CONTROL_PANEL_URL=https://control.example.com
CONTROL_PANEL_TOKEN=<SERVICE_TOKEN>
SERVICE_CONTROL_TOKEN_SHA256=<SHA256_OF_SERVICE_CALL_TOKEN>
AUTOSTREAM_ENV=production
AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true
OBSERVABILITY_URL=https://observability.example.com
OBSERVABILITY_TOKEN=<OBSERVABILITY_INGEST_TOKEN>
TZ=Asia/Tokyo
```

Inbound Control Panel dispatch uses `SERVICE_CONTROL_TOKEN` or `SERVICE_CONTROL_TOKEN_SHA256`. `CONTROL_PANEL_TOKEN` is outbound-only; in production or runtime-config-required mode it must not authorize `/jobs/start` or event mutation endpoints.

Encoder/Recorder への送信先 URL と worker-event token は、通常は Control Panel の stream job context で `encoder_recorder_url` / `stream_ingest_token` として渡されます。`ENCODER_RECORDER_URL` と `ENCODER_RECORDER_TOKEN` は local migration / dry-run 互換 fallback のみで使い、本番 env には置きません。

`AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true` または `AUTOSTREAM_ENV=production` の場合、Worker は Control Panel registration と runtime config 取得に失敗すると起動を停止します。runtime config に含まれる自 service の primary assignment だけを受け付け、standby または別 Worker に割り当てられた stream の `/jobs/start` は拒否します。

## 開発

```powershell
go test ./...
go build ./...
```

## Security

- service token、Encoder token、Observability token を log / API response に出しません。
- Encoder/Recorder への token-bearing request は redirect と unsafe HTTP を安全側で拒否します。
- event payload に secret-like value を入れないでください。

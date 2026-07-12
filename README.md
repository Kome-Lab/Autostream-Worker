# autostream-worker

AutoStream の Worker service です。

## 役割

- Control Panel から stream job context を受け取ります。
- overlay、caption、participant、active-speaker、current-time event を生成します。
- 生成した event を Encoder/Recorder へ送信します。
- Control Panel へ heartbeat と signal を送信し、Control Panel 経由で Observability に反映します。

video layer stream は MVP 後の後続タスクです。

## 主な環境変数

```text
AUTOSTREAM_NODE_CONFIG=/etc/autostream-worker/config.yml
AUTOSTREAM_ENV=production
AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true
TZ=Asia/Tokyo
```

`AUTOSTREAM_NODE_CONFIG` には Control Panel の Node登録で生成した `config.yml` を指定します。Node Runtime Token と stream-scoped token の検証に使う `stream_ingest.signing_key` はこのファイルに入り、標準構成では `CONTROL_PANEL_TOKEN`、`AUTOSTREAM_STREAM_INGEST_SIGNING_KEY`、`OBSERVABILITY_TOKEN` を env に手入力しません。Worker から Observability へ直接送る互換fallbackを使う場合だけ、`OBSERVABILITY_URL` と `OBSERVABILITY_TOKEN=<OBSERVABILITY_INGEST_TOKEN>` を追加します。

Encoder/Recorder への送信先 URL と worker-event token は、通常は Control Panel の stream job context で `encoder_recorder_url` / `stream_ingest_token` として渡されます。`ENCODER_RECORDER_URL` と `ENCODER_RECORDER_TOKEN` は local migration / dry-run 互換 fallback のみで使い、本番 env には置きません。

`AUTOSTREAM_REQUIRE_CONTROL_PANEL_RUNTIME_CONFIG=true` または `AUTOSTREAM_ENV=production` の場合、Worker は Control Panel registration と runtime config 取得に失敗すると起動を停止します。runtime config に含まれる自 service の primary assignment だけを受け付け、standby または別 Worker に割り当てられた stream の `/jobs/start` は拒否します。

## 開発

```powershell
go test ./...
go build ./...
```

## Security

- Node Runtime Token、Encoder token、Observability token を log / API response に出しません。
- Discord Bot には Worker の Node Runtime Token を渡さず、stream-scoped `worker_events` token だけを受け付けます。
- Encoder/Recorder への token-bearing request は redirect と unsafe HTTP を安全側で拒否します。
- event payload に secret-like value を入れないでください。

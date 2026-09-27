# kibitz

<img src="assets/kibitz-app-logo-512.png" alt="" width="96" align="right">

GitHub / GitLab / Azure DevOps の Pull Request (Merge Request) に対して、
AI による **コードレビュー** と **問い合わせへの回答** を行うマルチプラットフォーム対応ボット。

> kibitz = 横から口を出す / 助言する

## 構成

```
  GitHub          GitLab        Azure DevOps
     │               │               │
     └───────────────┴───────────────┘
                     │ Webhook (HTTPS)
                     ▼
        ┌──────────────────────────────┐
        │  kibitz-server (Go)          │
        │  - 署名 / トークン検証       │
        │  - 正規化イベントへ変換      │
        │  - トリガ判定・フィルタ      │
        │  - worker を起こす           │
        │  - 即座に 202 を返す         │
        └──────────────┬───────────────┘
                       │ publish
          ┌────────────┴────────────┐
          ▼                         ▼
  Cloud Pub/Sub              Amazon SQS (FIFO)
          │                         │
          └────────────┬────────────┘
                       │ subscribe / receive
                       ▼
        ┌──────────────────────────────┐
        │  kibitz-worker (Go)          │◀── kibitz-scaler
        │  - 冪等性チェック / ロック   │    (バックログから台数を決める)
        │  - リポジトリ取得 (shallow)  │
        │  - OpenCode 実行 (agent)     │──▶ 外部サービス / MCP サーバー
        │  - 結果を Forge API へ投稿   │
        └──────────────┬───────────────┘
                       │ REST API
                       ▼
            PR コメント / レビュー / 返信
```

- **kibitz-server**: Webhook 受信専用。ステートレスで、検証・正規化・publish だけを行い高速に応答する。
- **kibitz-worker**: キューを subscribe し、[OpenCode](https://opencode.ai) をヘッドレス実行してレビュー本文を生成、各プラットフォームの API に投稿する。MCP サーバー経由で外部サービス (Issue トラッカー、ドキュメント検索、Sentry など) を参照できる。
- **kibitz-mcp**: ワーカーと同じイメージに同梱する小さな MCP サーバー。PR の差分やコメントをエージェントにツールとして渡す。**Forge のトークンを持たず**、ワーカーが書き出したジョブ単位のファイルを読むだけ。単体 CLI としても動く。
- **kibitz-scaler**: キューの滞留数からワーカーの台数を決める。**レビューが無い間は 0 インスタンス**で、溜まれば増える。ワーカーはリクエストを受けないので Cloud Run の **worker pool** として動かしており、worker pool にはオートスケールが無い (台数は書き込むもの) ため、これを別に用意している ([deployment.md](docs/deployment.md#ワーカーのオートスケール))。

いずれも Go で実装し、キュー・ストレージ・Forge API はすべてインターフェースで抽象化して
GCP / AWS のどちらでも、また GitHub / GitLab / Azure DevOps のどれでも同じコードパスで動かす。

## ドキュメント

| ドキュメント | 内容 |
| --- | --- |
| [docs/adr/](docs/adr/) | **設計判断の記録 (ADR)**。なぜそう作られているか、何を捨てたか |
| [docs/architecture.md](docs/architecture.md) | 全体アーキテクチャ、コンポーネント、Go のインターフェース定義、ディレクトリ構成 |
| [docs/review-flow.md](docs/review-flow.md) | Webhook から投稿までの処理の流れ (シーケンス図)、スキップの判断順序 |
| [docs/event-schema.md](docs/event-schema.md) | 3 プラットフォームの Webhook 差異と正規化イベントスキーマ |
| [docs/queue.md](docs/queue.md) | Pub/Sub / SQS 抽象化、順序制御、冪等性、リトライ、DLQ、Claim Check |
| [docs/worker.md](docs/worker.md) | OpenCode のヘッドレス実行、MCP 統合、プロンプト設計、構造化出力 |
| [docs/triage.md](docs/triage.md) | 大きすぎる変更を絞るトリアージ。3 つの diff の使い分け、全体に戻る経路 |
| [docs/agent-engine.md](docs/agent-engine.md) | エージェントエンジンの選定 (OpenCode / pi の比較と決定) |
| [docs/security.md](docs/security.md) | 署名検証、シークレット管理、プロンプトインジェクション、fork PR の扱い |
| [docs/configuration.md](docs/configuration.md) | サーバー / ワーカーの環境変数、リポジトリ設定 `.kibitz.yaml` |
| [docs/deployment.md](docs/deployment.md) | GCP へのデプロイ手順、初回の動作確認、運用とトラブルシュート |
| [docs/onboarding.md](docs/onboarding.md) | **リポジトリを 1 つ載せる手順**。GitHub / GitLab / Azure DevOps それぞれの Webhook 設定 |
| [docs/runbook.md](docs/runbook.md) | **障害対応とシークレットのローテーション**。アラート別の対応、止めかたと戻しかた |
| [docs/roadmap.md](docs/roadmap.md) | フェーズ別実装計画、受け入れ条件、テスト戦略、未決事項 |

## 前提

- **クラウドは GCP をメイン**とする (Cloud Pub/Sub / Firestore / Cloud Run)。
  AWS (SQS) 対応はインターフェースとして残し、実装の優先度は下げる。
- **単一組織での利用**を前提とする (マルチテナント分離は行わない)。
- **レビューと回答は読み取りのみ。** Issue からの指示でコードを書く実装モードは
  別枠の機能で、**既定は無効**。有効にするには運用側とリポジトリ側の両方が必要で、
  書いた変更は資格情報を持たない別ジョブでビルドとテストが通ってから draft PR になる
  ([docs/worker.md](docs/worker.md#9-実装モード-phase-8既定は無効)、
  [ADR-0019](docs/adr/0019-run-repository-code-in-a-credential-less-job.md))。
- エージェントエンジンは **OpenCode** を採用 ([docs/agent-engine.md](docs/agent-engine.md))。
- GitHub は **GitHub App** で認証する (PAT は使わない)。
- モデルは **Vertex AI 経由の Claude** (既定 `claude-opus-5`)。認証は Workload Identity。
- レビューと回答の**出力は日本語**。

## 開発

ツールのバージョンは [mise](https://mise.jdx.dev) で固定している
(Go は go.mod、golangci-lint は Makefile が持つので、mise が持つのは Terraform だけ)。

```bash
mise install       # mise.toml に書かれたバージョンを入れる
```

```bash
make test          # go test -race -cover ./...
make lint          # golangci-lint (初回は自動でインストール)
make ci            # vet + lint + test + govulncheck + build
make build         # bin/kibitz-server, bin/kibitz-worker, bin/kibitz-scaler
make docker-build  # イメージをホストのアーキテクチャでビルドする
make up            # docker compose で両方を起動し /healthz を待つ
make up-pubsub     # Pub/Sub と Firestore のエミュレータも起動する
make test-integration  # エミュレータが必要なテスト
make down
```

ローカルでバイナリを直接動かす場合は、最低限のバックエンドを指定する。

```bash
KIBITZ_QUEUE_BACKEND=memory KIBITZ_GITHUB_WEBHOOK_SECRETS=dev make run-server
KIBITZ_QUEUE_BACKEND=memory KIBITZ_STATE_BACKEND=memory make run-worker

curl -s localhost:8080/healthz    # {"status":"ok","version":"dev"}
curl -s localhost:8081/healthz    # worker
```

Webhook を手元で試す (署名付きで送る):

```bash
BODY=$(cat testdata/webhooks/github/pull_request.opened.json)
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac dev | awk '{print $2}')"
curl -i -X POST localhost:8080/webhook/github \
  -H "X-GitHub-Event: pull_request" \
  -H "X-GitHub-Delivery: $(uuidgen)" \
  -H "X-Hub-Signature-256: $SIG" \
  --data-binary "$BODY"
# 202 Accepted   … レビュー対象としてキューに載った
# 204 No Content … 対象外 (ping、ラベル変更、kibitz 自身の発言など)
# 401 Unauthorized … 署名不一致
```

設定が足りない場合は起動時に**不足しているものを全部まとめて**報告して終了する。

```
kibitz-server: configuration:
KIBITZ_PUBSUB_PROJECT_ID: is required when KIBITZ_QUEUE_BACKEND is pubsub
KIBITZ_LOG_LEVEL: must be one of debug, info, warn, error, got "loud"
```

設定項目の一覧は [docs/configuration.md](docs/configuration.md)。

## ステータス

**Phase 3 まで実装完了。デプロイ一式あり、実機検証はこれから。**
手順は [docs/deployment.md](docs/deployment.md)、進捗は [docs/roadmap.md](docs/roadmap.md) を参照。

| 項目 | 状態 |
| --- | --- |
| `cmd/kibitz-server` / `cmd/kibitz-worker` の骨格、graceful shutdown | 完了 |
| 設定の読み込みと起動時検証 (`internal/config`) | 完了 |
| 構造化ログとシークレットのマスク (`internal/telemetry`) | 完了 |
| HTTP ミドルウェアとヘルスチェック (`internal/httpx`) | 完了 |
| Dockerfile / docker compose / CI | 完了 |
| 正規化イベント (`internal/event`) | 完了 |
| GitHub Webhook の検証・正規化 (`internal/webhook/github`) | 完了 |
| トリガ判定とコマンド解析 (`internal/policy`) | 完了 |
| キュー抽象化とインメモリ実装 (`internal/queue`) | 完了 |
| Cloud Pub/Sub の publish / subscribe (`internal/queue/pubsub`) | 完了 |
| GitHub App 認証と API クライアント (`internal/forge/github`) | 完了 |
| PR の shallow clone (`internal/workspace`) | 完了 |
| OpenCode の実行と出力検証 (`internal/reviewer`) | 完了 |
| レビュージョブ (`internal/worker`) | 完了 |
| 状態ストア (`internal/store`: memory / Firestore) | 完了 |
| 冪等性・PR ロック・再試行上限・失敗通知 (`internal/worker.Guard`) | 完了 |
| メトリクス (Prometheus) とトレース (OpenTelemetry) | 完了 |
| Terraform (GCP) とデプロイ手順 | 完了 |
| 実機での疎通確認 | 未了 ([手順](docs/deployment.md#6-初回の動作確認)) |
| GitLab / Azure DevOps | Phase 4 / 5 |

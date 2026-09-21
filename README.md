# kibitz

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
        │  kibitz-worker (Go)          │
        │  - 冪等性チェック / ロック   │
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

両者とも Go で実装し、キュー・ストレージ・Forge API はすべてインターフェースで抽象化して
GCP / AWS のどちらでも、また GitHub / GitLab / Azure DevOps のどれでも同じコードパスで動かす。

## ドキュメント

| ドキュメント | 内容 |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | 全体アーキテクチャ、コンポーネント、Go のインターフェース定義、ディレクトリ構成 |
| [docs/event-schema.md](docs/event-schema.md) | 3 プラットフォームの Webhook 差異と正規化イベントスキーマ |
| [docs/queue.md](docs/queue.md) | Pub/Sub / SQS 抽象化、順序制御、冪等性、リトライ、DLQ、Claim Check |
| [docs/worker.md](docs/worker.md) | OpenCode のヘッドレス実行、MCP 統合、プロンプト設計、構造化出力 |
| [docs/security.md](docs/security.md) | 署名検証、シークレット管理、プロンプトインジェクション、fork PR の扱い |
| [docs/configuration.md](docs/configuration.md) | サーバー / ワーカーの環境変数、リポジトリ設定 `.kibitz.yaml` |
| [docs/roadmap.md](docs/roadmap.md) | フェーズ別実装計画、受け入れ条件、テスト戦略、未決事項 |

## ステータス

設計フェーズ。実装は [docs/roadmap.md](docs/roadmap.md) の Phase 0 から順に進める。

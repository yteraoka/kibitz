# 設定

## 1. kibitz-server (環境変数)

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `KIBITZ_LISTEN_ADDR` | `:8080` | HTTP リッスンアドレス |
| `KIBITZ_METRICS_ADDR` | `:9090` | メトリクス用 (別リスナー) |
| `KIBITZ_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `KIBITZ_MAX_BODY_BYTES` | `26214400` | Webhook ボディ上限 (25 MB) |
| `KIBITZ_QUEUE_BACKEND` | `pubsub` | `pubsub` / `sqs` / `memory` |
| `KIBITZ_PUBSUB_PROJECT_ID` | - | GCP プロジェクト |
| `KIBITZ_PUBSUB_TOPIC` | `kibitz-events` | トピック名 |
| `KIBITZ_SQS_QUEUE_URL` | - | FIFO キューの URL |
| `KIBITZ_AWS_REGION` | - | リージョン |
| `KIBITZ_BLOBSTORE_BACKEND` | `none` | `gcs` / `s3` / `none` |
| `KIBITZ_BLOBSTORE_BUCKET` | - | Claim Check 用バケット |
| `KIBITZ_CLAIM_CHECK_THRESHOLD` | `65536` | この byte 数を超える生 payload は退避 |
| `KIBITZ_GITHUB_WEBHOOK_SECRETS` | - | カンマ区切り (ローテーション用) |
| `KIBITZ_GITLAB_WEBHOOK_TOKENS` | - | 同上 |
| `KIBITZ_AZDO_BASIC_USER` / `_PASSWORDS` | - | Azure DevOps Service Hooks の Basic 認証 |
| `KIBITZ_BOT_LOGINS` | - | 自分自身の発言を無視するためのアカウント名 (プラットフォーム別) |
| `KIBITZ_ALLOWED_REPOS` | `*` | 受け付けるリポジトリのグロブ (カンマ区切り) |
| `KIBITZ_MENTION` | `@kibitz` | コマンドのメンション名 |

## 2. kibitz-worker (環境変数)

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `KIBITZ_QUEUE_BACKEND` | `pubsub` | サーバーと同じ |
| `KIBITZ_PUBSUB_SUBSCRIPTION` | `kibitz-worker` | pull サブスクリプション |
| `KIBITZ_STATE_BACKEND` | `firestore` | `firestore` / `dynamodb` / `memory` |
| `KIBITZ_IMPLEMENT_ENABLED` | `false` | Issue 起点の実装モード (Phase 8) |
| `KIBITZ_CONCURRENCY` | `2` | 同時に走らせる OpenCode の数 |
| `KIBITZ_JOB_TIMEOUT` | `15m` | ジョブ全体のタイムアウト |
| `KIBITZ_WORKSPACE_DIR` | `/var/tmp/kibitz` | クローン先 |
| `KIBITZ_CLONE_DEPTH` | `50` | shallow clone の深さ |
| `KIBITZ_AGENT_ENGINE` | `opencode` | `opencode` / `pi` (将来) / `fake` (テスト用) |
| `KIBITZ_OPENCODE_BIN` | `opencode` | バイナリパス |
| `KIBITZ_OPENCODE_MODE` | `run` | `run` / `attach` |
| `KIBITZ_OPENCODE_SERVER_URL` | `http://127.0.0.1:4096` | `attach` 時の接続先 |
| `KIBITZ_MODEL` | `anthropic/claude-sonnet-4-5` | `provider/model` 形式 |
| `KIBITZ_MODEL_FALLBACK` | - | 主モデル障害時の代替 |
| `KIBITZ_MAX_COMMENTS` | `20` | 1 PR あたりの投稿上限 |
| `KIBITZ_MAX_DIFF_LINES` | `10000` | 超過時は triage モード |
| `KIBITZ_MCP_ALLOWLIST` | - | 有効化を許す MCP 名 (カンマ区切り) |
| `KIBITZ_GITHUB_APP_ID` / `_PRIVATE_KEY` / `_INSTALLATION_*` | - | GitHub App 認証 |
| `KIBITZ_GITLAB_BASE_URL` / `_TOKEN` | - | GitLab 認証 |
| `KIBITZ_AZDO_ORG_URL` / `_TOKEN` | - | Azure DevOps 認証 |

シークレットは環境変数に直接ではなく、Secret Manager / Secrets Manager から
起動時 + 定期リフレッシュで取得する (`KIBITZ_*_SECRET_REF` に参照名を置く形も用意する)。

## 3. リポジトリ設定 `.kibitz.yaml`

リポジトリのデフォルトブランチ側から読む (PR 側の変更は反映しない — [security.md](security.md))。

```yaml
version: 1

review:
  enabled: true
  # どのイベントでレビューするか
  triggers: [pr_opened, pr_ready_for_review, pr_updated, command]
  # draft のうちはレビューしない
  skip_draft: true
  # レビュー対象から外す
  paths_ignore:
    - "**/*.md"
    - "**/testdata/**"
    - "go.sum"
    - "**/*_generated.go"
  # 観点
  focus: [correctness, security, performance]
  # 出力言語
  language: ja
  # 投稿する最小 severity
  min_severity: medium
  max_comments: 15
  # 承認 / 変更要求を出すか (既定 false)
  allow_verdict: false
  model: anthropic/claude-sonnet-4-5

answer:
  enabled: true
  mention: "@kibitz"

# レビュー時に守らせたい追加ルール (プロンプトに載る)
guidelines: |
  - エラーは必ず呼び出し元にラップして返すこと (fmt.Errorf("...: %w", err))
  - 新しい公開 API には doc コメントを付けること

# 運用側の許可リストに載っているものだけ有効になる
mcp:
  allow: [jira, sentry]

budget:
  monthly_tokens: 20000000

# Phase 8 で追加予定。既定は無効
implement:
  enabled: false
  # 実装を指示できるユーザー (これ以外の指示は無視する)
  allowed_actors: [yteraoka]
  # 触ってよい範囲
  paths_allow: ["internal/**", "cmd/**"]
  # 実行を許すコマンド
  commands_allow: ["go build ./...", "go test ./...", "golangci-lint run"]
  branch_prefix: "kibitz/"
  # 常に draft PR として作る
  draft: true
```

設定の優先順位 (後が優先):

```
1. kibitz のビルトイン既定値
2. 運用側のグローバル設定 (環境変数 / 管理用 YAML)
3. 組織 / グループ単位の設定 (任意。管理用ストアに保持)
4. リポジトリの .kibitz.yaml (許可されたキーのみ)
5. PR コメントのコマンド引数 (その実行にのみ適用)
```

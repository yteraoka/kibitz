# 設定

## 1. kibitz-server (環境変数)

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `KIBITZ_LISTEN_ADDR` | `:8080` | HTTP リッスンアドレス |
| `KIBITZ_METRICS_ADDR` | `:9090` | メトリクス用 (別リスナー)。`off` またはリッスンアドレスと同値にすると、メインのリスナーで `/metrics` を提供する (Cloud Run のように公開ポートが 1 つの環境向け) |
| `KIBITZ_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `KIBITZ_LOG_FORMAT` | `json` | `json` / `text` |
| `KIBITZ_OTEL_ENDPOINT` | - | OTLP (gRPC) のコレクタ。未設定ならトレースは無効 |
| `KIBITZ_OTEL_INSECURE` | `false` | コレクタへ TLS なしで送る (サイドカー用) |
| `KIBITZ_OTEL_SAMPLE_RATIO` | `1` | サンプリング率 (0〜1) |
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
| `KIBITZ_MAX_EVENT_AGE` | `0` (無効) | これより古い配送を破棄する。0 は無効 (下記) |

## 2. kibitz-worker (環境変数)

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `KIBITZ_QUEUE_BACKEND` | `pubsub` | サーバーと同じ |
| `KIBITZ_PUBSUB_SUBSCRIPTION` | `kibitz-worker` | pull サブスクリプション |
| `KIBITZ_STATE_BACKEND` | `firestore` | `firestore` / `dynamodb` / `memory` |
| `KIBITZ_FIRESTORE_PROJECT_ID` | - | Firestore のプロジェクト (backend=firestore で必須) |
| `KIBITZ_FIRESTORE_DATABASE` | `(default)` | Firestore のデータベース ID |
| `KIBITZ_MAX_DELIVERIES` | `5` | この回数失敗したら再試行をやめて PR に通知する |
| `KIBITZ_MAX_POSTS_PER_HOUR` | `10` | 1 PR あたり 1 時間の投稿上限 (ループ防止) |
| `KIBITZ_IMPLEMENT_ENABLED` | `false` | Issue 起点の実装モード (Phase 8) |
| `KIBITZ_CONCURRENCY` | `2` | 同時に走らせる OpenCode の数 |
| `KIBITZ_JOB_TIMEOUT` | `15m` | ジョブ全体のタイムアウト |
| `KIBITZ_WORKSPACE_DIR` | `/var/tmp/kibitz` | クローン先 |
| `KIBITZ_CLONE_DEPTH` | `50` | shallow clone の深さ |
| `KIBITZ_AGENT_ENGINE` | `opencode` | `opencode` / `pi` (将来) / `fake` (テスト用) |
| `KIBITZ_OPENCODE_BIN` | `opencode` | バイナリパス |
| `KIBITZ_OPENCODE_MODE` | `run` | `run` / `attach` |
| `KIBITZ_OPENCODE_SERVER_URL` | `http://127.0.0.1:4096` | `attach` 時の接続先 |
| `KIBITZ_MODEL` | `google-vertex/gemini-3.1-pro-preview` | `provider/model` 形式。既定は Vertex AI の Gemini (申請不要) |
| `KIBITZ_PROVIDER_ENV` | - | API キーで認証するプロバイダ用に、エージェントへ渡す環境変数名 (カンマ区切り)。例: `ZHIPU_API_KEY`。**名前だけを設定し、値は環境から読む** |
| `KIBITZ_TRIAGE_MODEL` | (未設定なら `KIBITZ_MODEL`) | 巨大 PR の選抜など補助タスク用。安くしたい場合に `claude-sonnet-5` 等を指定 |
| `KIBITZ_MODEL_FALLBACK` | - | 主モデル障害時の代替 |
| `GOOGLE_CLOUD_PROJECT` | - | Vertex AI のプロジェクト ID (OpenCode が参照する) |
| `VERTEX_LOCATION` | `global` | Vertex AI のリージョン。データ所在地要件があれば `asia-northeast1` 等を指定 |
| `KIBITZ_VERTEX_MAAS_PROVIDER_ID` | `vertex-maas` | Vertex Model Garden のパートナーモデル (GLM など) 用に宣言するプロバイダ ID |
| `KIBITZ_VERTEX_MAAS_BASE_URL` | (プロジェクトとリージョンから生成) | 上記の OpenAI 互換エンドポイントを上書きする |
| `KIBITZ_MAX_COMMENTS` | `20` | 1 PR あたりの投稿上限 |
| `KIBITZ_MIN_SEVERITY` | `medium` | これ未満の指摘は投稿しない |
| `KIBITZ_SKIP_DRAFT` | `true` | draft PR はレビューしない (明示コマンドがあれば実行) |
| `KIBITZ_LANGUAGE` | `日本語` | レビューと回答の出力言語 |
| `KIBITZ_OPENCODE_REVIEW_AGENT` | `kibitz-review` | レビュー用エージェント定義名 |
| `KIBITZ_OPENCODE_ANSWER_AGENT` | `kibitz-answer` | 回答用エージェント定義名 |
| `KIBITZ_MAX_DIFF_LINES` | `10000` | 超過時は triage モード |
| `KIBITZ_MCP_ALLOWLIST` | - | 有効化を許す MCP 名 (カンマ区切り) |
| `KIBITZ_GITHUB_APP_ID` / `_PRIVATE_KEY` / `_INSTALLATION_*` | - | GitHub App 認証 (PAT は使わない) |
| `KIBITZ_GITLAB_BASE_URL` / `_TOKEN` | - | GitLab 認証 |
| `KIBITZ_AZDO_ORG_URL` / `_TOKEN` | - | Azure DevOps 認証 |

シークレットは環境変数に直接ではなく、Secret Manager / Secrets Manager から
起動時 + 定期リフレッシュで取得する (`KIBITZ_*_SECRET_REF` に参照名を置く形も用意する)。

ワーカーの `KIBITZ_BOT_LOGINS` はサーバーと同じ値を設定する。
サーバー側のフィルタをすり抜けた場合の二重チェックに使う。

Vertex AI の認証はサービスアカウント鍵ファイルを配置せず、
**GKE の Workload Identity (または Cloud Run のサービスアカウント) による ADC** を使う。
ワーカーのサービスアカウントに必要なのは `roles/aiplatform.user` のみ。

> **確認済み**: Vertex AI のプロバイダ ID は `google-vertex` で、Gemini と Claude の
> 両方を提供する (`google-vertex-anthropic` も別に存在する)。Vertex 上の Claude は
> `google-vertex/claude-opus-5@default` のように版の接尾辞が付き、**利用申請が必要**。
> GLM は `zai/glm-5.3` で、`ZHIPU_API_KEY` を見る。
> 一覧はワーカーのイメージ内で `opencode models <provider>` で引ける
> ([deployment.md](deployment.md#モデルの選び方))。

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
  # 出力言語 (既定は ja)
  language: ja
  # 投稿する最小 severity
  min_severity: medium
  max_comments: 15
  # 承認 / 変更要求を出すか (既定 false)
  allow_verdict: false
  model: google-vertex/gemini-3.1-pro-preview

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

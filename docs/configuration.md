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
| `KIBITZ_GITLAB_WEBHOOK_TOKENS` | - | GitLab の共有トークン (`X-Gitlab-Token`)。カンマ区切り |
| `KIBITZ_GITLAB_SIGNING_TOKENS` | - | GitLab 19.0+ の署名トークン (`whsec_...`)。**body まで検証できるのでこちらを推奨**。両方設定した場合、署名が来ていれば署名を検証する |
| `KIBITZ_AZDO_BASIC_USER` / `_PASSWORDS` | - | Azure DevOps Service Hooks の Basic 認証 |
| `KIBITZ_GITHUB_APP_ID` | - | 自分の発言を判別するための GitHub App id。**秘密情報ではない** (App の設定 URL に含まれる数字)。GitHub がコメントに付ける `performed_via_github_app.id` と突き合わせる |
| `KIBITZ_BOT_LOGINS` | - | 追加で無視したいアカウント名 (カンマ区切り)。通常は不要 — App id での判別が効かないイベント (レビューコメントなど) の保険 |
| `KIBITZ_ALLOWED_REPOS` | `*` | 受け付けるリポジトリのグロブ (カンマ区切り) |
| `KIBITZ_MENTION` | `/kibitz` | コメントで kibitz に話しかけるときのトークン。**`@` 付きにすると同名の GitHub アカウントへ通知が飛ぶ**ため既定は `/` ([security.md](security.md#31-メンション名と通知)) |
| `KIBITZ_TRIGGER_KEYWORDS` | - | レビュー依頼のキーワード (カンマ区切り)。設定すると PR 系イベントはタイトルか本文にこれらかメンションを含むときだけ publish する。コメントは常に対象 ([event-schema.md](event-schema.md#31-キーワードによる-publish-の絞り込み)) |
| `KIBITZ_MAX_EVENT_AGE` | `0` (無効) | これより古い配送を破棄する。0 は無効 (下記) |
| `KIBITZ_SCALE_BACKEND` | `none` | `cloudrun` にすると publish 直後にワーカーのインスタンス数を 1 に引き上げる |
| `KIBITZ_SCALE_PROJECT_ID` | `KIBITZ_PUBSUB_PROJECT_ID` | ワーカーがいるプロジェクト |
| `KIBITZ_SCALE_REGION` | - | ワーカーのリージョン (backend=cloudrun で必須) |
| `KIBITZ_SCALE_WORKER_SERVICE` | - | ワーカーの Cloud Run サービス名 (同上) |
| `KIBITZ_SCALE_WAKE_COOLDOWN` | `30s` | 起動要求をまとめる間隔。初回は待たない |

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
| `KIBITZ_MODEL_PRICES` | - | サマリコメントに概算コストを出すための単価。`モデル=入力/出力[/キャッシュ読み[/キャッシュ書き]]` を**100 万トークンあたり**でカンマ区切り。`*` は既定値。未設定ならトークン数だけを出し、金額は出さない |
| `KIBITZ_MODEL_PRICE_CURRENCY` | `$` | 上記の単価の通貨記号 |
| `KIBITZ_MODEL_FALLBACK` | - | 主モデル障害時の代替 |
| `GOOGLE_CLOUD_PROJECT` | - | Vertex AI のプロジェクト ID (OpenCode が参照する) |
| `VERTEX_LOCATION` | `global` | Vertex AI のリージョン。データ所在地要件があれば `asia-northeast1` 等を指定 |
| `KIBITZ_VERTEX_MAAS_PROVIDER_ID` | `vertex-maas` | Vertex Model Garden のパートナーモデル (GLM など) 用に宣言するプロバイダ ID |
| `KIBITZ_VERTEX_MAAS_BASE_URL` | (プロジェクトとリージョンから生成) | 上記の OpenAI 互換エンドポイントを上書きする |
| `KIBITZ_MAX_COMMENTS` | `20` | 1 PR あたりの投稿上限 |
| `KIBITZ_MIN_SEVERITY` | `medium` | これ未満の指摘は投稿しない |
| `KIBITZ_SKIP_DRAFT` | `true` | draft PR はレビューしない (明示コマンドがあれば実行) |
| `KIBITZ_LANGUAGE` | `日本語` | レビューと回答の出力言語 |
| `KIBITZ_SESSION_TTL` | `168h` (7 日) | エージェントのセッション ID と `/kibitz ignore` の保持期間。PR のクローズ / マージでも破棄される |
| `KIBITZ_MENTION` | `/kibitz` | ヘルプ本文に出す呼びかた。サーバーと同じ値にする (判定はサーバー側で行う) |
| `KIBITZ_OPENCODE_REVIEW_AGENT` | `kibitz-review` | レビュー用エージェント定義名 |
| `KIBITZ_OPENCODE_ANSWER_AGENT` | `kibitz-answer` | 回答用エージェント定義名 |
| `KIBITZ_MAX_DIFF_LINES` | `10000` | この行数を超えたら triage パスでレビュー対象を選抜する。0 で無効 ([worker.md](worker.md#巨大な-pr-の-triage)) |
| `KIBITZ_OPENCODE_TRIAGE_AGENT` | `kibitz-triage` | 選抜用エージェント定義名 |
| `KIBITZ_MCP_ALLOWLIST` | - | 有効化を許す MCP 名 (カンマ区切り) |
| `KIBITZ_GITHUB_APP_ID` / `_PRIVATE_KEY` / `_INSTALLATION_*` | - | GitHub App 認証 (PAT は使わない) |
| `KIBITZ_GITLAB_BASE_URL` | `https://gitlab.com` | GitLab インスタンス。self-managed はここを変える (`/api/v4` は付けても付けなくてもよい) |
| `KIBITZ_GITLAB_TOKEN` | - | personal / group / project access token (`api` スコープ)。**GitLab には GitHub App のインストールトークンに相当するものが無く、長命な資格情報になる** |
| `KIBITZ_AZDO_ORG_URL` / `_TOKEN` | - | Azure DevOps 認証 |

シークレットは環境変数に直接ではなく、Secret Manager / Secrets Manager から
起動時 + 定期リフレッシュで取得する (`KIBITZ_*_SECRET_REF` に参照名を置く形も用意する)。

ワーカーは起動時に `GET /app` で自分の `<slug>[bot]` を解決するので、
`KIBITZ_BOT_LOGINS` は通常設定しなくてよい (設定した値は解決結果に加算される)。
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

## 2.1 kibitz-scaler (環境変数)

Pub/Sub のバックログからワーカーのインスタンス数を決める小さなジョブ。
Cloud Run ジョブとして Cloud Scheduler から毎分起動する
([deployment.md](deployment.md#ワーカーのオートスケール))。

| 変数 | 既定 | 説明 |
| --- | --- | --- |
| `KIBITZ_PUBSUB_PROJECT_ID` | - | 監視するサブスクリプションのプロジェクト (必須) |
| `KIBITZ_PUBSUB_SUBSCRIPTION` | `kibitz-worker` | 監視するサブスクリプション |
| `KIBITZ_SCALE_BACKEND` | - | `cloudrun` (必須) |
| `KIBITZ_SCALE_REGION` / `_WORKER_SERVICE` | - | 対象のワーカーサービス (必須) |
| `KIBITZ_SCALE_MIN_INSTANCES` | `0` | キューが空のときのインスタンス数 |
| `KIBITZ_SCALE_MAX_INSTANCES` | `3` | 上限 |
| `KIBITZ_SCALE_MESSAGES_PER_INSTANCE` | `2` | 1 インスタンスが引き受けるメッセージ数。通常は `KIBITZ_CONCURRENCY` と同じ |
| `KIBITZ_SCALE_IDLE_AFTER` | `15m` | この時間ずっとキューが空なら最小まで下げる |
| `KIBITZ_SCALE_INTERVAL` | `1m` | `-loop` で常駐させたときの間隔 (ジョブ実行では未使用) |

`-loop` を付けなければ 1 回調整して終了する (Cloud Run ジョブ向け)。

必要な権限は `roles/monitoring.viewer` と、**ワーカーサービスに対する**
`roles/run.developer`。プロジェクト全体の権限は要らない。

### コストの概算について

kibitz は**単価を知らない**。プロバイダ・リージョン・契約で違い、しかも変わるため、
コード側に持たせていない。設定した場合だけ金額を出す。

```
KIBITZ_MODEL_PRICES=google-vertex-anthropic/claude-opus-5=3/15/0.3/3.75,*=2/8
```

1 モデルあたり `入力/出力[/キャッシュ読み[/キャッシュ書き]]` の 2〜4 個。
キャッシュの単価を省くと**入力と同じ単価**で計算する
(キャッシュを割り引かないプロバイダの実費であり、誰も書いていない割引を
勝手に当てるよりは高めに出るほうがまし)。

サマリコメントの末尾はこうなる。

```
レビュー対象: `3f7a1c2` / 所要 1m58s
トークン: 入力 50,000 (うちキャッシュ 48,000) / 出力 1,000 (うち推論 100) / 概算 $0.035
```

- **triage パスの分も合算する。** 巨大 PR では選抜と本レビューで 2 回モデルを呼ぶので、
  片方だけ報告すると過少申告になる (失敗した triage の分も含む)
- **キャッシュから読んだ分は入力に含めたうえで内訳を出す。** 2 回目以降のレビューは
  入力のほとんどがキャッシュで、単価は 1/10 程度。丸めて 1 つの数字にすると
  金額が一桁ずれる
- **推論 (reasoning) トークンは出力として課金される。** 表示されないが請求はされるので
  出力に含め、内訳を出す
- 単価が未設定のモデルでは**トークン数だけ**を出す。「無料だった」と「誰も設定していない」は別のこと
- 通貨記号は `KIBITZ_MODEL_PRICE_CURRENCY` で変えられる (既定 `$`)。
  円建ての単価を入れて `$` のまま出すより、記号を合わせるほうがよい

## 3. リポジトリ側の設定 (`.kibitz.yaml`)

リポジトリの**デフォルトブランチ側から**読む。PR 側の変更は反映しない
([security.md](security.md)) — PR を開いた人が、その PR のレビューのされ方を
決められてしまうため。

ファイルが無ければ環境変数の設定だけで動く。読めない・壊れているときも
**レビューは実行する** (既定値で)。設定ファイルの誤字でレビューが止まるほうが
困るため。ただしサマリコメントにその旨を書く。

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
  # 運用側の設定より小さくするときだけ効く
  max_comments: 15
  model: google-vertex/gemini-3.1-pro-preview

answer:
  enabled: true

# レビュー時に守らせたい追加ルール (プロンプトに載る)。
# 運用側の guidelines に追記される
guidelines: |
  - エラーは必ず呼び出し元にラップして返すこと (fmt.Errorf("...: %w", err))
  - 新しい公開 API には doc コメントを付けること
```

### 効くキーと、まだ効かないキー

**効くキー** (これ以外は取り込まない):

| キー | 効果 |
| --- | --- |
| `version` | スキーマ版。`1` のみ。これより大きいと**ファイル全体を拒否**する (黙って一部だけ効くよりよい) |
| `review.enabled` | `false` でレビューを止める。アンインストールせずに黙らせる手段 |
| `review.triggers` | `pr_opened` / `pr_updated` / `pr_ready_for_review` / `pr_review_requested` / `command`。未指定は全部 |
| `review.skip_draft` | draft の間はレビューしない |
| `review.paths_ignore` | 除外するファイル。下記の glob |
| `review.focus` | 重点的に見る観点。プロンプトに載る |
| `review.language` | 出力言語 |
| `review.min_severity` | 投稿する最小 severity |
| `review.max_comments` | 投稿数の上限。**運用側の値より小さくするときだけ効く**。`0` はサマリのみ (インライン指摘を出さない) |
| `review.model` | 使うモデル |
| `answer.enabled` | `false` で質問への回答を止める |
| `guidelines` | 運用側の guidelines に**追記**される (置き換えではない) |

**まだ効かないキー**: `mcp` (Phase 7 の残り)、`budget` (Phase 9)、`implement` (Phase 8)、
`review.allow_verdict`、`answer.mention`。書いてもログとサマリに「効きません」と出るだけ。

`answer.mention` をリポジトリ側で変えられないのは、メンションの判定が
**publish 前のサーバー側**で行われ、そこでは `.kibitz.yaml` を読んでいないため。
リポジトリごとに変えても、そもそもイベントがキューに載らない。

### `paths_ignore` の glob

`.gitignore` や CI の設定で書くのと同じ書き方。

| 記法 | 意味 |
| --- | --- |
| `*` | パス区切り (`/`) を跨がない任意の文字列 |
| `**` | 任意個のセグメント |
| `?` | 区切り以外の 1 文字 |

- `**/` で始まるパターンはルート直下にも当たる (`**/*.md` は `README.md` にも当たる)
- `a/**/b` は `a/b` にも当たる
- **パターンはその配下も含む。** `vendor` も `vendor/` も `vendor/github.com/x/y.go` に当たる
  (ディレクトリを書いたのに何も除外されない、という無言の失敗を避けるため)
- 先頭の `/` は書かない (常にリポジトリルートからの相対)

除外したファイルは**エージェントに渡さない**だけでなく、指摘の検証にも使わない。
エージェントが何らかの方法で読んで指摘してきても投稿されない。

設定の優先順位 (後が優先):

```
1. kibitz のビルトイン既定値
2. 運用側のグローバル設定 (環境変数 / 管理用 YAML)
3. 組織 / グループ単位の設定 (未実装。管理用ストアに保持する予定)
4. リポジトリの .kibitz.yaml (許可されたキーのみ)
5. PR コメントのコマンド引数 (その実行にのみ適用)
```

ただし一方通行にしている箇所が 2 つある。`guidelines` は**追記**
(運用側のルールをリポジトリ側から落とせないように)、`max_comments` は
**小さくする方向にだけ**効く (1 つの PR がコメントで埋まるのを防ぐのは運用側の判断)。
`max_comments: 0` も「小さくする方向」なので通る — サマリだけを出したいときに使う。

5 の例: `/kibitz review --focus security,performance` と書くと、その 1 回だけ
`review.focus` を上書きする。

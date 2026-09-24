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
| `KIBITZ_AZDO_BASIC_USER` / `_PASSWORDS` | - | Azure DevOps Service Hooks の Basic 認証。`_PASSWORDS` はカンマ区切り (ローテーション用)。**Azure DevOps は署名しない**ので、これが唯一の資格情報になる |
| `KIBITZ_AZDO_HEADER_NAME` / `_VALUES` | - | 任意。Service Hooks に設定した固定ヘッダも検証する。Basic 認証と**両方**一致しないと受け付けない。パスワードだけが漏れたときの被害を狭める |
| `KIBITZ_GITHUB_APP_ID` | - | 自分の発言を判別するための GitHub App id。**秘密情報ではない** (App の設定 URL に含まれる数字)。GitHub がコメントに付ける `performed_via_github_app.id` と突き合わせる |
| `KIBITZ_BOT_LOGINS` | - | 追加で無視したいアカウント名 (カンマ区切り)。通常は不要 — App id での判別が効かないイベント (レビューコメントなど) の保険 |
| `KIBITZ_ALLOWED_REPOS` | `*` | 受け付けるリポジトリのグロブ (カンマ区切り) |
| `KIBITZ_MENTION` | `/kibitz` | コメントで kibitz に話しかけるときのトークン。**`@` 付きにすると同名の GitHub アカウントへ通知が飛ぶ**ため既定は `/` ([security.md](security.md#31-メンション名と通知)) |
| `KIBITZ_TRIGGER_KEYWORDS` | - | レビュー依頼のキーワード (カンマ区切り)。設定すると PR 系イベントはタイトルか本文にこれらかメンションを含むときだけ publish する。コメントは常に対象 ([event-schema.md](event-schema.md#31-キーワードによる-publish-の絞り込み)) |
| `KIBITZ_MAX_EVENT_AGE` | `0` (無効) | これより古い配送を破棄する。0 は無効 (下記) |
| `KIBITZ_SCALE_BACKEND` | `none` | `cloudrun` にすると publish 直後にワーカーのインスタンス数を 1 に引き上げる |
| `KIBITZ_SCALE_PROJECT_ID` | `KIBITZ_PUBSUB_PROJECT_ID` | ワーカーがいるプロジェクト |
| `KIBITZ_SCALE_REGION` | - | ワーカーのリージョン (backend=cloudrun で必須) |
| `KIBITZ_SCALE_WORKER_POOL` | - | ワーカーの Cloud Run worker pool 名 (同上) |
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
| `KIBITZ_AGENT_ENV_PASSTHROUGH` | - | エージェントのプロセスへ追加で渡す環境変数名。**エージェントの環境は固定リストから組み立てる**ので、それ以外が必要なときの逃げ道 (下記) |
| `KIBITZ_TRIAGE_MODEL` | (未設定なら `KIBITZ_MODEL`) | 巨大 PR の選抜など補助タスク用。安くしたい場合に `claude-sonnet-5` 等を指定 |
| `KIBITZ_MODEL_PRICES` | - | サマリコメントに概算コストを出すための単価。`モデル=入力/出力[/キャッシュ読み[/キャッシュ書き]]` を**100 万トークンあたり**でカンマ区切り。`*` は既定値。未設定ならトークン数だけを出し、金額は出さない |
| `KIBITZ_REPO_BUDGETS` | - | リポジトリ 1 か月あたりの上限額。`パターン=金額` をカンマ区切り。パターンは `KIBITZ_ALLOWED_REPOS` と同じワイルドカードで `owner/name` に照合し、**最初に一致したものが効く**ので具体的なものを `*` より前に置く。金額の通貨は `KIBITZ_MODEL_PRICES` と同じ。未設定なら上限なし。**デプロイ側の設定で、リポジトリ側からは変更できない** |
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
| `KIBITZ_REFERENCE_DOCS` | `docs/adr/**/*.md,docs/decisions/**/*.md,adr/**/*.md` | 設計文書 (ADR) の場所。索引をプロンプトに載せ、本文はツールで読ませる。`off` で無効 (下記) |
| `KIBITZ_REPO_GUIDELINE_FILES` | `AGENTS.md,.kibitz/guidelines.md` | リポジトリの規約ファイル。**デフォルトブランチ側から**読む。`off` で無効 (下記) |
| `KIBITZ_MCP_CONTEXT_BIN` | `kibitz-mcp` | kibitz 自身の MCP サーバーのパス。`off` で無効 |
| `KIBITZ_MCP_SERVERS` | - | この kibitz が提供する MCP サーバーの定義。名前 → サーバーの JSON オブジェクト (下記) |
| `KIBITZ_MCP_ALLOWLIST` | - | そのうちリポジトリが有効化してよい名前 (カンマ区切り)。未設定なら定義したものすべて |
| `KIBITZ_GITHUB_APP_ID` / `_PRIVATE_KEY` / `_INSTALLATION_*` | - | GitHub App 認証 (PAT は使わない) |
| `KIBITZ_GITLAB_BASE_URL` | `https://gitlab.com` | GitLab インスタンス。self-managed はここを変える (`/api/v4` は付けても付けなくてもよい) |
| `KIBITZ_GITLAB_TOKEN` | - | personal / group / project access token (`api` スコープ)。**GitLab には GitHub App のインストールトークンに相当するものが無く、長命な資格情報になる** |
| `KIBITZ_AZDO_ORG_URL` | - | Azure DevOps の組織。Services なら `https://dev.azure.com/{org}`、Server なら `https://{server}/{collection}`。**イベントから導出できないので設定が要る** (Server はどのアドレスにも置ける)。`_TOKEN` と**両方**揃っていないと起動時に落ちる |
| `KIBITZ_AZDO_TOKEN` | - | PAT (Code の読み取り + Pull Request Threads の読み書き) か Entra ID のアクセストークン |
| `KIBITZ_AZDO_TOKEN_IS_BEARER` | `false` | トークンが Entra ID のアクセストークンであることを示す。**PAT は空ユーザーの Basic 認証のパスワード、Entra ID は Bearer** と送る場所が違い、入れ替えるとどちらも拒否される |

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
| `KIBITZ_SCALE_REGION` / `_WORKER_POOL` | - | 対象のワーカー (worker pool) (必須) |
| `KIBITZ_SCALE_MIN_INSTANCES` | `0` | キューが空のときのインスタンス数 |
| `KIBITZ_SCALE_MAX_INSTANCES` | `3` | 上限 |
| `KIBITZ_SCALE_MESSAGES_PER_INSTANCE` | `2` | 1 インスタンスが引き受けるメッセージ数。通常は `KIBITZ_CONCURRENCY` と同じ |
| `KIBITZ_SCALE_IDLE_AFTER` | `15m` | この時間ずっとキューが空なら最小まで下げる |
| `KIBITZ_SCALE_INTERVAL` | `1m` | `-loop` で常駐させたときの間隔 (ジョブ実行では未使用) |

`-loop` を付けなければ 1 回調整して終了する (Cloud Run ジョブ向け)。

必要な権限は `roles/monitoring.viewer` と、**ワーカーの worker pool に対する**
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

### リポジトリ別の予算

上限を設定すると、1 か月にそのリポジトリへ使う金額を止められる。

```
KIBITZ_REPO_BUDGETS=acme/payments=200,acme/*=50,*=10
```

**最初に一致したパターンが効く**ので、具体的なものを `*` より前に置く。
金額の通貨は `KIBITZ_MODEL_PRICES` と同じ。未設定のリポジトリは上限なし。

| 性質 | 理由 |
| --- | --- |
| **デプロイ側の設定にしかない** | リポジトリ側から上限を上げられるなら、それは上限ではない。`.kibitz.yaml` では設定できない ([ADR-0011](adr/0011-repository-settings-from-the-default-branch.md)) |
| **暦月で数え、月初に自動で戻る** | 集計キーに年月が入っているだけなので、リセットする操作は要らない |
| **止めるのは「次の」レビュー** | 1 回のレビューの費用は払い終えるまで分からない。途中で打ち切っても請求はされるので、超過分を少し許して次から止める |
| **止めたことを PR に書く** | 黙って止まるボットは壊れたボットと見分けが付かない。通知は 1 PR につき 1 つで、push のたびには増えない |
| **質問への回答も止まる** | レビューと同じモデルを呼ぶため |

止まっているときの PR コメントはこうなる。

```
今月の予算を使い切ったため、このリポジトリのレビューを停止しています。

使用 $50.31 / 予算 $50.00 (2026-09-23 04:12 UTC 時点)。来月の初日に自動で再開します。
```

> **単価が未設定のモデルは予算に計上されない。**
> 計上できないものを 0 円として数えると、上限に**到達しないまま超過する**。
> 予算を設定していて単価が無い場合は警告ログを出す。
> `KIBITZ_MODEL_PRICES` に `*=入力/出力` のフォールバックを入れておくのが確実。

### 実装モード (Phase 8、既定は無効)

Issue の指示でコードを書くモード。**4 つの条件がすべて揃ったときだけ**動く。

| 条件 | どこで決まるか |
| --- | --- |
| `KIBITZ_IMPLEMENT_ENABLED=true` | **運用側**。false ならリポジトリ側で何を書いても動かない |
| `implement.enabled: true` | リポジトリの `.kibitz.yaml`（デフォルトブランチ） |
| 指示者が `implement.allowed_actors` に含まれる | 同上。**空なら誰も許可されない** |
| `implement.paths_allow` が 1 つ以上ある | 同上。**空なら何も書けない** |

```yaml
version: 1
implement:
  enabled: true
  allowed_actors: [alice, bob]
  paths_allow:
    - "internal/**"
    - "cmd/*/main.go"
  commands_allow:
    - "go build ./..."
    - "go test ./..."
  branch_prefix: kibitz/
```

**指示者を見る。Issue を書いた人ではない。** Issue の本文を書いた人物と
`/kibitz implement` と指示した人物が違う場合、許可を判定するのは**指示した側**で、
本文はあくまでデータとして扱う（[ADR-0010](adr/0010-data-and-instruction-positions.md)）。

#### 設定に関わらず編集できないもの

`paths_allow` に `**` と書いても、次は編集できない。

| 分類 | 例 |
| --- | --- |
| CI 設定 | `.github/workflows/**`、`.gitlab-ci.yml`、`azure-pipelines.yml`、`Jenkinsfile` |
| kibitz 自身の設定 | `.kibitz.yaml`、`.kibitz/**`、`AGENTS.md` |
| 依存定義 | `go.mod`、`package.json`、`Cargo.toml`、各種ロックファイル |
| 資格情報 | `**/*.pem`、`**/*.key`、`.env` |

**これらを書き換えられると、以後の実行環境そのものを乗っ取られる。**
とくに `.kibitz.yaml` は「編集してよいパスの一覧」そのものなので、
リポジトリ側から上書きできる設定にすると意味を失う。だから設定ではなく固定。

照合は**大文字小文字を区別しない**。大文字小文字を区別しないチェックアウトでは
`.github/Workflows/ci.yml` が本物のワークフローファイルになるため。

#### この kibitz での実装状況

**判定までが入っている。** 上記の条件判定・パスの拒否・Issue への応答は動く。
ブランチ作成・エージェント実行・サンドボックス・draft PR の作成は未実装で、
条件を満たした指示にはその旨を Issue に返す。

### 設計文書 (ADR) の参照

リポジトリに ADR があれば、**パスとタイトルだけ**をプロンプトに載せ、
本文は `search_docs` / `get_doc` ツールで必要なものだけ読ませる。

```
KIBITZ_REFERENCE_DOCS=docs/adr/**/*.md,docs/decisions/**/*.md
KIBITZ_REFERENCE_DOCS=off
```

**索引だけを載せるのが肝。** エージェントは `rg` も `read` も持っているので検索能力は
元からあるが、`docs/adr/` を見る理由がなければ引かない。タイトルの一覧が
「読める」を「読もうと思う」に変え、同時に**検索語彙**を与える
(ADR は「ordering key」と書き、diff には `PublishOrdered` としか出てこない)。

本文を全部載せない理由は逆で、ADR が 30 件あるリポジトリでは毎回数万トークンになり、
肝心の差分を押し出す。

- **既定は ADR の置き場所だけ。** `docs/**/*.md` まで広げると索引が長くなるリポジトリが出る。
  必要なら明示的に指定できる
- 索引は 50 件、タイトルは 90 文字、本文取得は 48 KiB で打ち切る
- **索引に載っているパスしかツールは返さない。** 「文書を読む」が「任意のファイルを読む」に
  ならないようにするため (パスはモデルが PR を読んで組み立てるものなので)
- 索引が空なら**セクションもツールも出ない**。ADR を置いていないリポジトリのコストはゼロ
- 内容は `<<<` `>>>` で囲って返す。**これが `read` ツールとの違い**で、
  `read` の出力は囲えない ([security.md](security.md))

チェックアウト側から索引を作るので、**その PR が追加した ADR も載る**。
規約ファイル (下記) と違って内容は**データ**として扱うので、PR 側で構わない。

kibitz 自身の ADR は [docs/adr/](adr/) にあり、既定のパターンに一致するので、
このリポジトリのレビューでは設定なしで索引に載る。

**効いているかどうかは計測できる。** 実際に検索・参照された回数が
`kibitz_reference_docs_consulted_total` に、1 件のレビューで読まれたパスが
ログの `docs_read` に出る
([deployment.md](deployment.md#設計文書の索引が効いているかを見る))。
どちらもずっと 0 なら、索引は毎回のプロンプトの行を買えていない。

### リポジトリの規約ファイル

リポジトリが自分の規約を書いたファイルを、**デフォルトブランチ側から**読んで
guidelines に追記する。既定は `AGENTS.md` と `.kibitz/guidelines.md`。

```
KIBITZ_REPO_GUIDELINE_FILES=AGENTS.md,.kibitz/guidelines.md,docs/review-rules.md
KIBITZ_REPO_GUIDELINE_FILES=off      # リポジトリ側から指示を一切入れない
```

優先順位は **運用側の guidelines → 規約ファイル → `.kibitz.yaml` の guidelines**。
後のものが前のものに追記される (置き換えはしない)。

- **PR 側からは読まない。** これらは**指示**としてプロンプトに入るので、
  コミット権のある人しか書けない経路からしか入れない
  ([security.md](security.md))。opencode 自身がチェックアウトから `AGENTS.md` を
  読み込む挙動も `OPENCODE_DISABLE_PROJECT_CONFIG=1` で止めている
- 1 ファイル 24 KiB で打ち切る。超えた場合はサマリコメントにその旨を書く
  (切られた規約は適用されなかった規約なので)
- どのファイル由来かをコメントで明示するので、レビューが引いたルールの出どころを辿れる
- `CONTRIBUTING.md` は**既定に入れていない**。初めて PR を出す人間向けに書かれていることが多く、
  毎回のレビューで払うトークンに見合わない。必要なら明示的に足せる

### MCP サーバー

kibitz 自身の `kibitz-mcp` は**設定なしで毎回有効**になる
(資格情報を持たず、PR について答えるだけなので。[worker.md](worker.md#8-独自-mcp-サーバー-kibitz-mcp))。
以下は**外部サービス** (Jira、Sentry など) を MCP 経由でレビューに参加させる話。
**運用側が定義し、リポジトリが名前で有効化する**という二段構え。

```
KIBITZ_MCP_SERVERS='{
  "jira":   {"type": "remote", "url": "https://jira.example.com/mcp",
             "headers": {"Authorization": "Bearer {env:JIRA_TOKEN}"}},
  "sentry": {"type": "local", "command": ["sentry-mcp"],
             "environment": {"SENTRY_TOKEN": "{env:SENTRY_TOKEN}"}},
  "docs":   {"type": "remote", "url": "https://docs.example.com/mcp",
             "allow_fork": true}
}'
```

| 有効化の条件 | |
| --- | --- |
| `KIBITZ_MCP_SERVERS` に定義がある | 存在する |
| `KIBITZ_MCP_ALLOWLIST` に名前がある (未設定なら全部) | リポジトリが要求してよい |
| `.kibitz.yaml` の `mcp.allow` に名前がある | 実際に有効になる |

**何も指定しなければ何も有効にならない。** MCP はツール定義だけで毎回トークンを
消費するので、「使えるから載せておく」は誰も同意していない請求になる。

`{env:NAME}` は **opencode 自身が展開する**。kibitz は設定ファイルに
プレースホルダのまま書き、その変数だけをエージェントのプロセスへ渡す。
**シークレットがディスク上の設定ファイルに書かれることはない。**

- `type` は `local` (`command` 必須、`cwd` / `environment` 可) または
  `remote` (`url` 必須、`headers` 可)。`timeout` はミリ秒
- **fork からの PR では既定で有効にならない。** fork のブランチはコミット権の無い人が
  書いたもので、それをエージェントが読む。資格情報を持つサーバーがそこから
  到達可能であってはいけない ([security.md](security.md))。
  公開情報しか返さないサーバーは `"allow_fork": true` を付ければ fork でも有効になる
  (この項目は kibitz 側の判断材料で、opencode に渡す設定には書かれない)
- 定義と許可リストが食い違う (定義したのに許可リストに無い) 場合は**起動時にエラー**。
  片方だけ直したつもりの設定ミスを黙って通さない
- リポジトリが要求した名前をこの kibitz が提供していない場合は、
  ログに警告を出し、**サマリコメントにもその旨を書く**。
  頼んだのに来なかったことは、頼んだ側に伝わらないと気づけない
- **triage パスには MCP を渡さない。** ファイル名だけ見て読む対象を選ぶパスなので、
  ツール定義を載せたら節約するはずのものを使ってしまう

### エージェントのプロセス環境

エージェントの環境は**固定リストから組み立てる**。ワーカーの環境をそのまま
渡すことはしない。

ワーカーの環境には Webhook シークレット、GitHub App の秘密鍵、GitLab トークンが
入っている。一方 **local な MCP サーバーは opencode が起動する別プロセスで、
opencode の環境をそのまま継承する** — kibitz が書いたわけではないバイナリに
それらを渡す理由は無い。

エージェントに渡すもの:

1. 基本的な変数 (`PATH` / `HOME` / `TMPDIR` / プロキシ / CA / ADC 関連など)
2. `KIBITZ_PROVIDER_ENV` で指定したプロバイダの資格情報
3. **有効になった MCP サーバーの定義が `{env:NAME}` で参照している変数だけ**
4. `KIBITZ_AGENT_ENV_PASSTHROUGH` で明示的に指定した変数

3 が要点で、`jira` を有効にしたジョブには `JIRA_TOKEN` が渡るが、
`sentry` だけを有効にしたジョブには渡らない。

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

# 運用側が定義し、許可リストに載せたものだけ有効になる
mcp:
  allow: [jira, sentry]

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
| `mcp.allow` | 有効にする MCP サーバー名。運用側が定義し許可したものだけ ([上記](#mcp-サーバー)) |

**まだ効かないキー**: `budget` (Phase 9)、`implement` (Phase 8)、
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

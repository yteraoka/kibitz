# デプロイ手順 (GCP)

GitHub + Cloud Run + Cloud Pub/Sub + Firestore + Vertex AI の構成で kibitz を動かす手順。
Terraform は [deploy/terraform/gcp](../deploy/terraform/gcp) にある。

## 0. 前提

| 必要なもの | 備考 |
| --- | --- |
| Google Cloud プロジェクト | 課金有効。Firestore のロケーションは後から変更できない |
| `gcloud` / `terraform` / `docker` | Terraform は [mise](https://mise.jdx.dev) で固定してある。リポジトリのルートで `mise install` を実行すると `mise.toml` に書かれた 1.16.3 が入る |
| GitHub App を作成できる権限 | 組織の Owner、または App の作成権限 |
| モデルが使える状態 | 既定は Vertex AI の Gemini。**Anthropic のモデルは Vertex では利用申請が必要**なので、申請を通していない場合は Gemini か、API キーで使えるプロバイダ (GLM など) を選ぶ。下の「モデルの選び方」を参照 |

Vertex AI のモデルはリージョンによって提供状況が違う。`vertex_location` を
`global` 以外にする場合は、**先に Model Garden でそのリージョンに対象モデルがあることを確認する**。

### モデルの選び方

プロバイダ ID とモデル ID は opencode が models.dev から取得する一覧に従う。
**ワーカーのイメージの中で実際に引ける**ので、推測せずここで確認する。

```bash
# 認証情報が検出できたプロバイダのモデルだけが出る
docker run --rm --entrypoint opencode \
  -e GOOGLE_APPLICATION_CREDENTIALS=/dev/null -e GOOGLE_CLOUD_PROJECT=dummy \
  YOUR_WORKER_IMAGE models google-vertex
```

確認済みの対応関係:

| 使いたいもの | `model` の値 | 認証 | 備考 |
| --- | --- | --- | --- |
| Gemini (Vertex) | `google-vertex/gemini-3.1-pro-preview` | サービスアカウント (ADC) | **既定。申請不要、シークレット不要** |
| Gemini (Vertex, 安価) | `google-vertex/gemini-3.8-flash` | 同上 | 速くて安い。レビュー品質は落ちる |
| Claude (Vertex) | `google-vertex/claude-opus-5@default` | 同上 | **Vertex での Anthropic モデル利用申請が必要**。ID に `@default` が付く点に注意 |
| **GLM (Vertex Model Garden)** | `vertex-maas/zai-org/glm-5.2-maas` | サービスアカウント (ADC) | **シークレット不要**。opencode のカタログには無いので kibitz が自動でプロバイダを宣言する (下記) |
| GLM (Z.AI 直) | `zai/glm-5.3` | `ZHIPU_API_KEY` | `model_api_key_env_name = "ZHIPU_API_KEY"` を設定する |
| GLM (Coding Plan) | `zai-coding-plan/glm-5.3` | `ZHIPU_API_KEY` | GLM Coding Plan の契約がある場合 |
| OpenRouter 経由 | `openrouter/...` | `OPENROUTER_API_KEY` | `model_api_key_env_name = "OPENROUTER_API_KEY"` |

`google-vertex` は **Gemini と Claude の両方**を提供する。Claude 側だけが申請を要する。

### Vertex Model Garden のパートナーモデル (GLM など)

Vertex AI は Model Garden でパートナーのモデルを MaaS として提供している
(例: [GLM 5.2](https://console.cloud.google.com/agent-platform/publishers/zai-org/model-garden/glm-5.2-maas))。
これらは **opencode のモデルカタログ (models.dev) には載っていない**ため、
プロバイダとして宣言してやる必要がある。kibitz はこれを自動で行う。

`KIBITZ_MODEL` が `vertex-maas/` で始まっていると、ワーカーはジョブごとに

1. ADC から OAuth アクセストークンを発行し
2. Vertex の OpenAI 互換エンドポイントを指すプロバイダ定義を生成して
3. opencode の設定に書き込む

トークンは 1 時間で失効するが、**設定はジョブごとに生成し直す**ので問題にならない
(ジョブの上限は 15 分)。API キーは不要で、認証は Gemini と同じサービスアカウントのまま。

```hcl
model = "vertex-maas/zai-org/glm-5.2-maas"
# model_api_key_env_name は不要
```

**先にエンドポイントを 1 回確認することを強く勧める。** kibitz が組み立てる URL の形と
モデル ID が実際と合っているかは、次の 1 コマンドで分かる。

```bash
PROJECT=$(gcloud config get-value project)
LOCATION=global   # または asia-northeast1 など
HOST=$([ "$LOCATION" = global ] && echo aiplatform.googleapis.com || echo $LOCATION-aiplatform.googleapis.com)

curl -sS -X POST \
  "https://$HOST/v1beta1/projects/$PROJECT/locations/$LOCATION/endpoints/openapi/chat/completions" \
  -H "Authorization: Bearer $(gcloud auth print-access-token)" \
  -H "Content-Type: application/json" \
  -d '{"model":"zai-org/glm-5.2-maas","messages":[{"role":"user","content":"hi"}]}'
```

応答が返れば `vertex-maas/zai-org/glm-5.2-maas` がそのまま使える。
URL の形やモデル ID が違った場合は、コードを直さなくても設定で合わせられる。

| 環境変数 / 変数 | 用途 |
| --- | --- |
| `KIBITZ_VERTEX_MAAS_BASE_URL` | 組み立てた URL を上書きする (`/chat/completions` は opencode が付けるので、その手前まで) |
| `KIBITZ_VERTEX_MAAS_PROVIDER_ID` | プロバイダ ID を変える (既定 `vertex-maas`) |

Model Garden 側で対象モデルを**有効化 (Enable) しておく**必要がある点は、
Anthropic のモデルと同じ。

### API キーが必要なプロバイダを使う場合

`model_api_key_env_name` にそのプロバイダが見る環境変数名を入れると、
Secret Manager のシークレットが作られ、ワーカーにその名前で注入される。
値は Terraform には入らない。

```hcl
model                  = "zai/glm-5.3"
model_api_key_env_name = "ZHIPU_API_KEY"
```

```bash
# 手順 4 と同じタイミングで値を入れる
printf '%s' 'YOUR_ZHIPU_API_KEY' | \
  gcloud secrets versions add kibitz-model-api-key --data-file=-
```

```bash
gcloud auth login
gcloud auth application-default login
gcloud config set project YOUR_PROJECT_ID
```

## 1. GitHub App を作る

kibitz は GitHub App としてのみ認証する (PAT は使わない)。

App の所有者によって作成ページが違う。**所有者は後から変更できない**ので先に決める。

| 所有者 | 作成ページ | 向いている場合 |
| --- | --- | --- |
| 個人アカウント | `https://github.com/settings/apps/new` | 個人のリポジトリで使う。設定画面からは Settings → Developer settings → GitHub Apps → New GitHub App |
| Organization | `https://github.com/organizations/YOUR_ORG/settings/apps/new` | チームで使う。担当者が抜けても組織の管理者が引き継げる |

**Repository permissions**

| 権限 | レベル | 用途 |
| --- | --- | --- |
| Contents | Read-only | PR のコードを clone する |
| Pull requests | Read and write | 差分の取得、レビューとコメントの投稿 |
| Issues | Read and write | PR のコメント (GitHub では PR も issue として扱われる) |
| Metadata | Read-only | 必須 (自動で付く) |

**Subscribe to events**

- Pull request
- Issue comment
- Pull request review comment

**その他の設定**

- Webhook: **Active** にチェック
- Webhook URL: 一旦 `https://example.com/placeholder` (手順 5 で実際の URL に変える)
- Webhook secret: 長いランダム値を生成して控える

```bash
openssl rand -hex 32   # この値を控える
```

作成後:

1. **App ID** を控える (App の設定ページ上部)
2. **Generate a private key** で `.pem` をダウンロード
3. **Install App** で対象のアカウント (または組織) にインストールし、
   インストール後の URL `.../installations/<INSTALLATION_ID>` から
   **Installation ID** を控える。ここで対象リポジトリを選ぶ
   (選び忘れると `git fetch` が失敗する。§8 を参照)

## 2. Terraform の変数を用意する

```bash
cd deploy/terraform/gcp
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars   # project_id, github_app_id, github_installation_id など
```

**`bot_logins` は通常は空のままでよい。** kibitz は自分の発言を自動で判別する:

- **サーバー**は、GitHub がコメントに付ける **App の id** (`performed_via_github_app.id`)
  と `github_app_id` を突き合わせる。名前ではないのでリネームしても壊れない
- **ワーカー**は起動時に GitHub へ問い合わせ (`GET /app`)、`<slug>[bot]` を得る。
  ログに `msg="resolved the bot account" login=...` が出る

App をリネームした後に旧アカウント名も無視したい、といった場合にだけ `bot_logins`
に足す。設定した値は自動判別の結果に**加算**される。

`server_image` / `worker_image` / `scaler_image` は次の手順で作るので、
いったん仮の値で構わない。

レビューを依頼された PR だけをレビューしたい場合は `trigger_keywords` も
設定する。キューが空の時間が伸びるぶん、ワーカーが 0 インスタンスでいられる
時間も伸びる ([event-schema.md](event-schema.md#31-キーワードによる-publish-の絞り込み))。

## 3. イメージ置き場とシークレットの箱を先に作る

イメージを push する先と、シークレットの入れ物が先に要る。3 段階に分けて apply する。

```bash
terraform init

# 段階 1: Artifact Registry とシークレット (の箱) だけ
terraform apply \
  -target=google_project_service.required \
  -target=google_artifact_registry_repository.images \
  -target=google_secret_manager_secret.github_webhook \
  -target=google_secret_manager_secret.github_private_key
```

## 4. シークレットを入れてイメージを push する

シークレットは Terraform の state に載せたくないので、`gcloud` で直接入れる。

```bash
# 手順 1 で生成した Webhook secret
printf '%s' 'YOUR_WEBHOOK_SECRET' | \
  gcloud secrets versions add kibitz-github-webhook-secrets --data-file=-

# 手順 1 でダウンロードした秘密鍵
gcloud secrets versions add kibitz-github-app-private-key \
  --data-file=/path/to/your-app.private-key.pem
```

イメージをビルドして push する。

```bash
cd ../../..   # リポジトリルート
gcloud auth configure-docker asia-northeast1-docker.pkg.dev

make push IMAGE_REPO=asia-northeast1-docker.pkg.dev/YOUR_PROJECT/kibitz TAG=v0.1.0
```

最後に表示される 3 行を `terraform.tfvars` の
`server_image` / `worker_image` / `scaler_image` に書く。

### イメージの中身とビルド引数

| イメージ | ベース | 中身 |
| --- | --- | --- |
| `kibitz-server` | distroless | Go バイナリのみ。リポジトリを触らずエージェントも動かさないため |
| `kibitz-worker` | `node:22-slim` | Go バイナリ + `git` + `ripgrep` + `opencode` (バージョン固定) + エージェント定義。MCP サーバーをローカルプロセスとして起動するため Node ランタイムが要る |
| `kibitz-scaler` | distroless | Go バイナリのみ。Cloud Monitoring を読んで Cloud Run のインスタンス数を書くだけ |

| ビルド引数 | 既定値 | 用途 |
| --- | --- | --- |
| `VERSION` | `make` が git から生成 | `/healthz` とログに出るバージョン |
| `OPENCODE_VERSION` | `deploy/docker/Dockerfile.worker` に記載 | エージェントの挙動は kibitz の出力そのものなので固定している。上げるときは意図的に |

```bash
# opencode を上げて試す
make push IMAGE_REPO=... TAG=v0.2.0-rc1 OPENCODE_VERSION=1.19.0
```

### ビルドするアーキテクチャに注意

**Cloud Run は amd64 で動く。** Apple Silicon などの arm64 マシンで普通に
`docker build` すると arm64 のイメージができ、Cloud Run で起動しない。

`make push` は `docker buildx build --platform linux/amd64 --push` を使うので、
arm64 マシンからでも正しいイメージが push される。別のアーキテクチャに出す場合は
`PLATFORM` を上書きする。

```bash
make push IMAGE_REPO=... TAG=v0.1.0 PLATFORM=linux/arm64
```

ローカルで動かすだけなら `make docker-build` (ホストのアーキテクチャでビルド)
または `make up` (docker compose) を使う。

## 5. 全体を apply して Webhook URL を設定する

```bash
cd deploy/terraform/gcp
terraform apply

terraform output webhook_url
# => https://kibitz-server-xxxxx-an.a.run.app/webhook/github
```

この URL を GitHub App の **Webhook URL** に設定する (手順 1 の placeholder を置き換える)。

## 6. 初回の動作確認

ここが本番で初めて確認される部分なので、順番に潰していく。

### 6-1. サーバーが応答する

```bash
curl -i "$(terraform output -raw server_url)/healthz"
# HTTP/2 200 / {"status":"ok","version":"v0.1.0"}
```

### 6-2. Webhook が届く

GitHub App の設定ページ **Advanced** タブに配送履歴がある。
`ping` イベントが **204** なら署名検証まで通っている。401 なら Webhook secret が
シークレットの中身と一致していない (手順 7 の「401 の切り分け」)。

サーバー側は**配送 1 件につき 1 行**、何を受け取って publish したかどうかを出す。

```bash
gcloud run services logs read kibitz-server --region asia-northeast1 --limit 50 | \
  grep -E 'event published|event skipped'
```

```
msg="event published" published=true  event_name=pull_request.opened kind=pr.opened repository=my-org/app pr=42 actor=yteraoka delivery_id=... message_id=...
msg="event skipped"   published=false reason=no_keyword event_name=pull_request.synchronize kind=pr.updated repository=my-org/app pr=42 actor=yteraoka delivery_id=...
```

### 6-3. ワーカーが起動している

既定ではワーカーは**キューが空なら 0 インスタンス**なので、apply 直後は何も
動いていないのが正常。PR を 1 つ作る (あるいは手で 1 台上げる) と起動する。

ワーカーは Cloud Run の**サービスではなく worker pool** なので、コマンドは
`gcloud run worker-pools` を使う。

```bash
# いま何台か
gcloud run worker-pools describe kibitz-worker --region asia-northeast1 \
  --format="value(metadata.annotations['run.googleapis.com/manualInstanceCount'])"

# 手で 1 台上げる (scaler が次の判定で戻す)
gcloud run worker-pools update kibitz-worker --region asia-northeast1 --instances=1

gcloud run worker-pools logs read kibitz-worker --region asia-northeast1 --limit 50
```

`msg=starting` に続けて `msg=listening` が出ていれば良い。
ここで落ちている場合、よくある原因は次の 2 つ:

- **モデル ID が違う** — 上の「モデルの選び方」の表と `model` の値が合っているか。
  Vertex の Claude は利用申請が通っていないと使えない
- **Firestore のデータベースが無い / 権限不足** — `datastore.user` が付いているか

### 6-4. 実際にレビューさせる

対象リポジトリで小さな PR を作る。数分以内に kibitz のサマリコメントが付けば成功。

```bash
# 流れを追う
gcloud run worker-pools logs read kibitz-worker --region asia-northeast1 --limit 100 | \
  grep -E 'job started|job finished|review produced findings|job failed'
```

**期待するログの並び**:

```
msg="job started"                 event_id=github:... kind=pr.opened
msg="review produced findings"    findings=3 dropped=1 input_tokens=... 
msg="job finished"                duration=1m58s
```

### 6-5. Phase 2・3 で実機未検証だった点の確認

| 確認項目 | 確認方法 | 失敗したときの症状 |
| --- | --- | --- |
| モデル ID とプロバイダ | 6-4 が通る | `no provider configured` や `model not found` で失敗 |
| OpenCode の JSON イベント形式 | `input_tokens` が 0 でない | 動くがトークン数が 0 のまま (opencode 1.18.31 で確認済み) |
| `--file` でのプロンプト添付 | 6-4 が通る | 指摘が的外れ、または空 |
| `--agent` の解決 | 6-4 が通る | `unknown agent` で失敗 |
| GitHub App のトークン発行 | 6-2 と 6-4 が通る | `401` / `404` が worker ログに出る |
| `refs/pull/N/head` の fetch | 6-4 が通る | `git fetch` の失敗がログに出る |
| Firestore 実装 | 同じ PR に 2 回 push しても二重投稿されない | 同じ指摘が 2 回付く |

`input_tokens` が 0 のままの場合は [worker.md](worker.md#stdout-の-json-イベント) の
イベント解析が実際の形式と合っていない。opencode を上げたときに起きうる。
`opencode run --format json` の出力を 1 回手元で取って、
`internal/reviewer/opencode/events.go` の拾うキーを合わせる。

トークン数は `step_finish` イベントの `part.tokens` からしか取れない。
実際にこれを取り違えていて、v0.3.4 までトークン数が常に 0 だった。

### 6-6. 冪等性の確認

GitHub App の **Advanced** タブから同じ配送を **Redeliver** する。
コメントが増えなければ重複排除が効いている。

```bash
gcloud run worker-pools logs read kibitz-worker --region asia-northeast1 --limit 20 | \
  grep "delivery was already handled"
```

## 7. 運用

### メトリクス

サーバーは Cloud Run の制約 (公開ポートが 1 つ) のため `/metrics` を
メインのリスナーで公開している。カウンタにリポジトリ名やユーザー名は含めていない。

```bash
curl -s "$(terraform output -raw server_url)/metrics" | grep kibitz_
```

ワーカーの `/metrics` は内部からのみ到達できる。Managed Prometheus に
取り込む場合は、Cloud Run のサイドカーとして OTel collector を追加する。

#### 設計文書の索引が効いているかを見る

`docs/adr/` を持つリポジトリでは、索引が**毎回のプロンプトの行**と
**毎回のリクエストのツール定義 2 つ**を消費している
([ADR-0017](adr/0017-index-decision-records-serve-bodies-as-tools.md))。
それが買えているものは、この 2 つのカウンタに出る。

```bash
curl -s "$(terraform output -raw server_url)/metrics" | grep kibitz_reference_docs_consulted_total
# kibitz_reference_docs_consulted_total{action="search"} 41
# kibitz_reference_docs_consulted_total{action="read"} 12
```

| `action` | 意味 |
| --- | --- |
| `search` | `search_docs` で横断検索した回数。抜粋で答えが付いた場合はここだけが増える |
| `read` | `get_doc` で全文を読んだ**文書の数** (1 回のレビューで同じ文書は 1 つ) |

**ADR を持つリポジトリをレビューしていて、どちらもずっと 0 なら索引は何も買っていない。**
パターンを絞る (`KIBITZ_REFERENCE_DOCS`) か、`off` にするかの判断材料になる。

ツールの呼び出し全体は `kibitz_agent_tool_calls_total{tool,outcome}` に出る。
`tool` は**エンジンが報告した名前**で、opencode は MCP サーバーのツールに
サーバー名を前置するため、kibitz 自身のツールは `kibitz_get_doc` のように見える。

1 件のレビューの内訳はログのほうが速い。`review produced findings` の行に
`reference_docs` (索引に載せた数) と `docs_read` (実際に読んだパス) が並ぶので、
**「18 件出して 0 件読まれた」がクエリを 2 つ繋がなくても読める**。

### 配送のログ

「PR を作ったのにレビューが来ない」を最初に切り分ける場所。サーバーは配送 1 件に
つき 1 行を info で出し、**publish したかどうかを `published` で明示**する。

| フィールド | 内容 |
| --- | --- |
| `published` | Pub/Sub に載せたか (`true` / `false`) |
| `reason` | 載せなかった理由 ([event-schema.md](event-schema.md#3-トリガ判定-internalpolicy)) |
| `event_name` | GitHub 側の呼び名 (`pull_request.synchronize` など)。**Advanced タブの配送履歴と同じ語彙** |
| `kind` | kibitz 側の種別 (`pr.updated` など) |
| `repository` / `pr` / `actor` | どの PR の、誰の操作か |
| `delivery_id` | GitHub の配送 ID。Advanced タブで検索して Redeliver できる |
| `event_id` | 正規化イベントの ID。ワーカー側のログと突き合わせられる |
| `message_id` | Pub/Sub のメッセージ ID (publish したときだけ) |

```bash
# publish されなかったものだけ、理由つきで
gcloud logging read \
  'resource.labels.service_name="kibitz-server" AND jsonPayload.published=false' \
  --limit 50 --format='value(jsonPayload.reason,jsonPayload.event_name,jsonPayload.repository,jsonPayload.pr)'

# 1 件の配送を端から端まで追う (サーバーとワーカーの両方に出る)
gcloud logging read 'jsonPayload.delivery_id="<配送ID>"' --limit 20
```

`reason` の主なもの:

| reason | 意味 |
| --- | --- |
| `unsupported_event` | kibitz が扱わないイベント (`ping`、`labeled`、`edited` など) |
| `no_keyword` | `trigger_keywords` を設定していて、PR のタイトル / 本文に無い |
| `no_mention` | コメントだが `/kibitz` が入っていない (コードスパンやコードブロックの中は数えない) |
| `self_authored` | kibitz 自身の発言 (無限ループ防止) |
| `repo_not_allowed` | `allowed_repos` に合わない |
| `stale` | `KIBITZ_MAX_EVENT_AGE` より古い配送 (既定では無効) |

PR のタイトルや本文はログに出していない。`repository` と `pr` があれば PR 自体を
見に行けるので、ログに残す理由が無いため。

### アラート

`terraform.tfvars` の `alert_notification_channels` を設定していないと、
**アラートは作られるが誰にも通知されない**。先にチャンネルを作る。

```bash
gcloud alpha monitoring channels create \
  --display-name="kibitz alerts" --type=email \
  --channel-labels=email_address=team@example.com
# 出力の name (projects/.../notificationChannels/123) を tfvars に入れる
```

設定されるアラートは 4 つ ([queue.md](queue.md) の分類に対応):

| アラート | 意味 |
| --- | --- |
| dead letter topic にメッセージ | デコードできないメッセージ、または ack されなかったメッセージ。レビューが 1 件失われている |
| バックログが滞留 | ワーカーが追いついていない、または落ちている |
| サーバーが 5xx | publish に失敗して配送を拒否している。**GitHub は自動再送しない**ので、直したあと手動で Redeliver する |
| オートスケーラが失敗し続けている | インスタンス数が放置される。1 台上がりっぱなしで課金され続けるか、キューが処理されない |

### ワーカーのオートスケール

ワーカーは Pub/Sub の pull サブスクライバなので、**リクエストが 1 件も来ない**。
そのため Cloud Run の**サービスではなく worker pool** で動かしている
(ingress もポートもプローブも無く、CPU は常時割り当てられる)。
worker pool にオートスケールは無く、インスタンス数は書き込むものなので、
放っておくと「常時 N 台」か「永久に 0 台」のどちらかにしかならない。
その数を kibitz が両側から決める。

```
PR 作成 ──> kibitz-server ──publish──> Pub/Sub
                  │
                  └─ インスタンス数を 1 に引き上げ (即時)
                                          │
                                          ↓
Cloud Scheduler ──毎分──> kibitz-scaler ──> バックログを読む
                                          └─ 溜まっていれば増やす
                                             15 分空なら 0 に戻す
```

- **起動はサーバーが行う。** メトリクスは数分遅れるので、それを待つと
  レビュー開始が遅れる。publish したサーバー自身が知っているので直接伝える
  (`KIBITZ_SCALE_*`、30 秒のクールダウンで API 呼び出しをまとめる)。
- **増減は kibitz-scaler が行う。** `num_undelivered_messages` を読み、
  `ceil(未処理数 / worker_messages_per_instance)` を
  `[1, worker_max_instances]` に収めた数にする。
- **0 に戻すのは「一定時間ずっと空」のときだけ。** このメトリクスは
  *ack されていない配送済みメッセージも含む*ので、レビュー実行中の
  ワーカーはメッセージを掴んだままになり、作業中に消されることはない。
  それでもメトリクス自体が遅れるため、既定では 15 分 (`worker_idle_after`)
  空が続いてから下げる。
- **メトリクスが読めないときは下げない。** Monitoring 障害で
  「空に見える」ことがあるので、読めなければ 1 台維持する。

変更するのは **worker pool の** instance count (`scaling.manualInstanceCount`) で、
リビジョンテンプレート側ではない。テンプレートを触ると新しいリビジョンが作られ、
実行中のレビューが中断されるため。Terraform は初期値だけ設定して以降は
`lifecycle { ignore_changes = [scaling] }` で手を出さない。

上限 (`worker_max_instances`) は worker pool 側には設定しない。この数を動かすのは
kibitz だけなので、上限も数を決める側 (scaler の
`KIBITZ_SCALE_MAX_INSTANCES`) が持っている。

```bash
# いまの台数と、scaler が何を見て決めたか
gcloud run worker-pools describe kibitz-worker --region asia-northeast1 \
  --format="value(metadata.annotations['run.googleapis.com/manualInstanceCount'])"
gcloud run jobs executions list --job kibitz-scaler --region asia-northeast1 --limit 5
gcloud logging read \
  'resource.type=cloud_run_job AND resource.labels.job_name=kibitz-scaler' \
  --limit 20 --format='value(jsonPayload.msg,jsonPayload.reason,jsonPayload.backlog)'
```

常時 1 台温めておきたい場合は `worker_min_instances = 1` にする。
増減自体は動いたまま、下限だけが 1 になる。

#### Cloud Run サービスからの移行

以前のバージョンではワーカーも Cloud Run **サービス**だった。worker pool は
別のリソースなので、`terraform apply` は `kibitz-worker` サービスを削除して
worker pool を作り直す (リネームでは移せない)。ワーカーは pull サブスクライバ
なので、切り替えの間に届いたイベントはキューに残り、ack されなかった配送は
再配送される。作業中のレビューを落としたくない場合は、キューが空になってから
apply する。

**無駄なメッセージを減らす**のも同じ話の一部で、`trigger_keywords` を設定すると
レビューを依頼された PR しか publish されないため、キューが空の時間が伸びて
ワーカーが 0 のままでいられる ([event-schema.md](event-schema.md#31-キーワードによる-publish-の絞り込み))。

### コスト

主なコストはモデルの利用料。効く順に:

1. `worker_max_instances` — 同時に走るレビューの上限。実質的な支出の上限
2. `KIBITZ_MAX_DIFF_LINES` / `KIBITZ_MAX_COMMENTS` — 1 回のレビューの大きさ
3. `KIBITZ_MIN_SEVERITY` — 投稿する指摘の下限 (トークンではなくノイズに効く)
4. `model` — 補助タスクだけ安いモデルにする場合は `KIBITZ_TRIAGE_MODEL`

```bash
# トークン消費の実測
curl -s "$(terraform output -raw server_url)/metrics" | grep kibitz_agent_tokens_total
```

### 更新 (タグを push する)

**タグを push すると GitHub Actions がビルドしてデプロイする。**

```bash
git tag v0.4.0
git push origin v0.4.0
```

`.github/workflows/release.yml` が次を行う。

1. `go vet` と `go test -race` (タグが指すコミットを誰も検証していない、という事故を防ぐ)
2. 3 つのイメージを `linux/amd64` でビルドし、**タグと同じ名前**で Artifact Registry へ push
3. `gcloud run services update` / `worker-pools update` / `jobs update` で 3 つを差し替え
4. 実行中のリビジョン名をジョブサマリに出す

イメージに `latest` は付けない。**「どのコミットが動いているか」を言えないデプロイは
ロールバックできない**ので、動くタグは使わない。

#### 初回だけ必要な設定

認証は **Workload Identity Federation** で、サービスアカウントキーは作らない。
プール自体は既存のもの (既定では `github-pool`) を**参照するだけ**で、Terraform は
作らない。別の id を使っているなら `workload_identity_pool_id` で指定する。
プールの中に作られる provider と「このリポジトリのタグだけ」という条件が kibitz の
持ち物で、`terraform apply` のあと出力をリポジトリ変数に入れる。

```bash
terraform output github_actions
```

出た 5 つを GitHub の **Settings → Secrets and variables → Actions → Variables**
に登録する (どれも秘密情報ではないので Secrets ではなく Variables)。

| 変数 | 内容 |
| --- | --- |
| `GCP_PROJECT_ID` | プロジェクト ID |
| `GCP_REGION` | リージョン |
| `GCP_WORKLOAD_IDENTITY_PROVIDER` | `projects/.../workloadIdentityPools/github-pool/providers/kibitz-github` |
| `GCP_DEPLOY_SERVICE_ACCOUNT` | `kibitz-deployer@...` |
| `GCP_IMAGE_REPOSITORY` | `REGION-docker.pkg.dev/PROJECT/kibitz` |

未設定のまま tag を push すると、**最初のステップで「どれが足りないか」を出して止まる**
(権限エラーで午後を溶かさないため)。

このリポジトリの**タグからしか**トークンを交換できない。ブランチ上のワークフローは
—— Pull Request が持ち込んだものも含めて —— デプロイできない。

#### 手で push する場合

```bash
make push IMAGE_REPO=... TAG=v0.4.0
gcloud run worker-pools update kibitz-worker --region asia-northeast1 \
  --image .../kibitz-worker:v0.4.0
```

**イメージは Terraform の管理外**になっている (`lifecycle { ignore_changes }`)。
tfvars の `*_image` は初回の値であって、以降の実体はデプロイしたものになる。
特定のイメージに戻したいときだけ tfvars を直して `terraform apply` する。

### ロールバック

```bash
# 直前のリビジョンに戻す (worker pool は「トラフィック」ではなく
# インスタンスの割り当てを動かす)
gcloud run worker-pools update-instance-split kibitz-worker \
  --region asia-northeast1 --to-revisions=PREVIOUS_REVISION=100

# サーバーはサービスなので従来どおり
gcloud run services update-traffic kibitz-server \
  --region asia-northeast1 --to-revisions=PREVIOUS_REVISION=100
```

Cloud Run のリビジョンは残るので、割り当てを戻すのが最短。
古いタグを再デプロイしたい場合は、そのタグのイメージを指定して
`gcloud run ... update --image` する (タグを打ち直す必要は無い)。

### 止める

```bash
# 受信だけ止める (キューに残った分は処理される)
# GitHub App の Webhook を Active から外す

# ワーカーを止める。scaler が次の実行で戻すので、先に止める
gcloud scheduler jobs pause kibitz-scaler --location asia-northeast1
gcloud run worker-pools update kibitz-worker --region asia-northeast1 --instances=0

# 再開
gcloud scheduler jobs resume kibitz-scaler --location asia-northeast1
```

### 401 の切り分け

`signature does not match` は「攻撃」ではなくほぼ必ず**値の不一致**で、
しかも GitHub も kibitz もシークレットの中身を表示しないので、
そのままでは「どちらが違うのか」が分からない。そのためログに
**フィンガープリント** (SHA-256 の先頭 12 文字) を出している。

```
msg="github webhook secrets loaded" count=1 fingerprints=1dac8899aa71
msg="webhook rejected" error="webhook: signature does not match (kibitz holds 1 secret(s), fingerprint 1dac8899aa71; ...)"
```

この値を、GitHub App の設定に入れたはずの文字列から手元で計算して比べる。

```bash
# 1. 動いているコンテナが持っている値 (ログから)
gcloud run services logs read kibitz-server --region asia-northeast1 --limit 100 | \
  grep -E 'secrets loaded|signature does not match'

# 2. 手元の「正しいはずの値」
printf '%s' 'YOUR_WEBHOOK_SECRET' | sha256sum | cut -c1-12

# 3. Secret Manager に入っている値
gcloud secrets versions access latest --secret=kibitz-github-webhook-secrets | \
  sha256sum | cut -c1-12
```

結果の読み方:

| 1 (コンテナ) | 3 (Secret Manager) | 原因 |
| --- | --- | --- |
| 2 と一致しない | 2 と一致する | **コンテナが古い値のまま**。`version = "latest"` はインスタンス起動時に解決されるので、シークレットを更新したら新しいリビジョンをデプロイする (下記) |
| 2 と一致しない | 2 と一致しない | Secret Manager の値が違う。新しいバージョンを追加する |
| 2 と一致する | 2 と一致する | kibitz 側は正しい。**GitHub App 側の値**が違う (よくあるのは、App の設定画面でシークレットを入れ直したつもりで保存されていない、別の App / 別の Webhook を見ている、組織の Webhook と App の Webhook を取り違えている) |

シークレットを更新したあとコンテナに反映させる:

```bash
printf '%s' 'YOUR_WEBHOOK_SECRET' | \
  gcloud secrets versions add kibitz-github-webhook-secrets --data-file=-

# 新しいインスタンスに読み直させる
gcloud run services update kibitz-server --region asia-northeast1 --no-traffic --tag=tmp \
  && gcloud run services update-traffic kibitz-server --region asia-northeast1 --to-latest
```

その他、値そのものが原因になりやすいもの:

- **末尾の改行** — `echo` ではなく `printf '%s'` を使う (kibitz は前後の空白を落とすので、
  実際には両方通る。GitHub App 側に改行付きで貼っている場合は GitHub 側が違う値になる)
- **カンマを含むシークレット** — `KIBITZ_GITHUB_WEBHOOK_SECRETS` はローテーション用に
  カンマ区切りのリストとして読む。カンマを含む値も 1 つの値として受け付けるようにしてあるが、
  避けたほうが分かりやすい
- **App の Webhook と リポジトリ / Organization の Webhook の取り違え** — 別々の設定で、
  それぞれ別のシークレットを持つ

payload そのものから検証したい場合は、GitHub App の **Advanced** タブで配送の
Request body と `X-Hub-Signature-256` を取り、手元で HMAC を計算して比べる。

```bash
printf '%s' "$(cat body.json)" | \
  openssl dgst -sha256 -hmac 'YOUR_WEBHOOK_SECRET' -r | cut -d' ' -f1
# 配送ログの X-Hub-Signature-256 の sha256= 以降と一致すれば GitHub 側は正しい
```

## 8. よくある失敗

| 症状 | 原因 | 対処 |
| --- | --- | --- |
| Webhook が 401 | Webhook secret が不一致 | 手順 7 の「401 の切り分け」。**値が正しく見えても、動いているコンテナが持っている値は別**のことがある |
| Webhook が 500 | サーバーに Webhook secret が渡っていない | `gcloud secrets versions list` で版があるか確認 |
| Webhook が 503 | Pub/Sub へ publish できない | サーバーの SA に `pubsub.publisher` があるか |
| コメントが二重に付く (自分に反応している) | サーバーに `KIBITZ_GITHUB_APP_ID` が渡っていない | サーバーの環境変数を確認する。ワーカー側は起動ログの `resolved the bot account` を見る |
| 同じ PR に何度もレビューが付く | Firestore に書けていない | ワーカーの SA に `datastore.user` があるか |
| レビューが来ない・ログも無い | ワーカーが 0 インスタンスのまま起きていない | サーバーのログに `worker wake-up is enabled` が出ているか、サーバーの SA にワーカー (worker pool) の `roles/run.developer` があるか |
| PR を作ってもイベントが publish されない | `trigger_keywords` を設定したがキーワードが無い | サーバーのログの `reason=no_keyword`。**コメントの先頭に** `/kibitz review` と書けば実行される |
| ワーカーが 1 台上がりっぱなし | scaler が失敗している、またはメトリクスが読めていない | `gcloud run jobs executions list --job kibitz-scaler`。SA に `roles/monitoring.viewer` があるか |
| コメントしたのにレビューが走らない (回答だけ返る) | コマンドがコメントの先頭に無い | 引用や説明文の途中のメンションは質問として扱う。先頭に書く ([event-schema.md](event-schema.md#41-コマンドはコメントの先頭だけ)) |
| レビュー中にワーカーが落ちる | `worker_idle_after` を短くしすぎている | メトリクスの遅延より長くする (既定 15 分)。ジョブは再配送されるのでレビューは失われない |
| `git fetch` が失敗する | App のインストール先にリポジトリが含まれていない | GitHub App の Install 設定でリポジトリを追加 |
| worker のビルドが `opencode-ai's postinstall script was not run` で失敗 | `--ignore-scripts` で opencode の postinstall が動いていない | `npm rebuild -g opencode-ai` を後続で実行する (修正済み。古い Dockerfile を使っている場合は更新する) |

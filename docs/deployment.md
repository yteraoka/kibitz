# デプロイ手順 (GCP)

GitHub + Cloud Run + Cloud Pub/Sub + Firestore + Vertex AI の構成で kibitz を動かす手順。
Terraform は [deploy/terraform/gcp](../deploy/terraform/gcp) にある。

## 0. 前提

| 必要なもの | 備考 |
| --- | --- |
| Google Cloud プロジェクト | 課金有効。Firestore のロケーションは後から変更できない |
| `gcloud` / `terraform` / `docker` | Terraform は [mise](https://mise.jdx.dev) で固定してある。リポジトリのルートで `mise install` を実行すると `mise.toml` に書かれた 1.16.3 が入る |
| GitHub App を作成できる権限 | 組織の Owner、または App の作成権限 |
| Vertex AI で Claude が有効 | Model Garden で対象モデルを有効化しておく |

Vertex AI のモデルはリージョンによって提供状況が違う。`vertex_location` を
`global` 以外にする場合は、**先に Model Garden でそのリージョンに対象モデルがあることを確認する**。

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
$EDITOR terraform.tfvars   # project_id, github_app_id, github_installation_id, bot_logins など
```

`bot_logins` は App が投稿するアカウント名。GitHub App の場合は
`<app-slug>[bot]` になる (例: App の slug が `kibitz` なら `kibitz[bot]`)。
**ここを間違えると kibitz が自分のコメントに反応し続ける**ので、手順 6 で必ず確認する。

`server_image` と `worker_image` は次の手順で作るので、いったん仮の値で構わない。

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

最後に表示される 2 行を `terraform.tfvars` の `server_image` / `worker_image` に書く。

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
シークレットの中身と一致していない。

### 6-3. ワーカーが起動している

```bash
gcloud run services logs read kibitz-worker --region asia-northeast1 --limit 50
```

`msg=starting` に続けて `msg=listening` が出ていれば良い。
ここで落ちている場合、よくある原因は次の 2 つ:

- **Vertex AI のプロバイダ ID が違う** — `model=...` のログと OpenCode が知っている
  プロバイダ名が一致しているか。実機で `opencode models | grep -i vertex` を
  ワーカーのイメージ内で実行して確認する
  (`docker run --rm --entrypoint opencode IMAGE models`)
- **Firestore のデータベースが無い / 権限不足** — `datastore.user` が付いているか

### 6-4. 実際にレビューさせる

対象リポジトリで小さな PR を作る。数分以内に kibitz のサマリコメントが付けば成功。

```bash
# 流れを追う
gcloud run services logs read kibitz-worker --region asia-northeast1 --limit 100 | \
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
| Vertex AI のプロバイダ ID | 6-4 が通る | ワーカーが `no provider configured` で失敗 |
| OpenCode の JSON イベント形式 | `input_tokens` が 0 でない | 動くがトークン数が 0 のまま |
| `--file` でのプロンプト添付 | 6-4 が通る | 指摘が的外れ、または空 |
| `--agent` の解決 | 6-4 が通る | `unknown agent` で失敗 |
| GitHub App のトークン発行 | 6-2 と 6-4 が通る | `401` / `404` が worker ログに出る |
| `refs/pull/N/head` の fetch | 6-4 が通る | `git fetch` の失敗がログに出る |
| Firestore 実装 | 同じ PR に 2 回 push しても二重投稿されない | 同じ指摘が 2 回付く |

`input_tokens` が 0 のままの場合は [worker.md](worker.md) のイベント解析が
実際の形式と合っていない。`opencode run --format json` の出力を 1 回手元で取って、
`internal/reviewer/opencode/events.go` の拾うキーを合わせる。

### 6-6. 冪等性の確認

GitHub App の **Advanced** タブから同じ配送を **Redeliver** する。
コメントが増えなければ重複排除が効いている。

```bash
gcloud run services logs read kibitz-worker --region asia-northeast1 --limit 20 | \
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

### アラート

`terraform.tfvars` の `alert_notification_channels` を設定していないと、
**アラートは作られるが誰にも通知されない**。先にチャンネルを作る。

```bash
gcloud alpha monitoring channels create \
  --display-name="kibitz alerts" --type=email \
  --channel-labels=email_address=team@example.com
# 出力の name (projects/.../notificationChannels/123) を tfvars に入れる
```

設定されるアラートは 3 つ ([queue.md](queue.md) の分類に対応):

| アラート | 意味 |
| --- | --- |
| dead letter topic にメッセージ | デコードできないメッセージ、または ack されなかったメッセージ。レビューが 1 件失われている |
| バックログが滞留 | ワーカーが追いついていない、または落ちている |
| サーバーが 5xx | publish に失敗して配送を拒否している。**GitHub は自動再送しない**ので、直したあと手動で Redeliver する |

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

### 更新

```bash
make push IMAGE_REPO=... TAG=v0.2.0
# tfvars の server_image / worker_image を更新して
terraform apply
```

### ロールバック

```bash
# 直前のリビジョンに戻す
gcloud run services update-traffic kibitz-worker \
  --region asia-northeast1 --to-revisions=PREVIOUS_REVISION=100
```

Cloud Run のリビジョンは残るので、tfvars を戻して `terraform apply` でも良い。

### 止める

```bash
# 受信だけ止める (キューに残った分は処理される)
# GitHub App の Webhook を Active から外す

# ワーカーを止める
gcloud run services update kibitz-worker --region asia-northeast1 --min-instances=0
```

## 8. よくある失敗

| 症状 | 原因 | 対処 |
| --- | --- | --- |
| Webhook が 401 | Webhook secret が不一致 | シークレットの最新バージョンと GitHub App の設定を揃え、Cloud Run を再デプロイ |
| Webhook が 500 | サーバーに Webhook secret が渡っていない | `gcloud secrets versions list` で版があるか確認 |
| Webhook が 503 | Pub/Sub へ publish できない | サーバーの SA に `pubsub.publisher` があるか |
| コメントが二重に付く | `bot_logins` が実際のアカウント名と違う | 投稿されたコメントの作者名を見て tfvars を修正 |
| 同じ PR に何度もレビューが付く | Firestore に書けていない | ワーカーの SA に `datastore.user` があるか |
| レビューが来ない・ログも無い | ワーカーが 0 インスタンス | `cpu_idle = false` と `min_instance_count = 1` が効いているか確認 |
| `git fetch` が失敗する | App のインストール先にリポジトリが含まれていない | GitHub App の Install 設定でリポジトリを追加 |

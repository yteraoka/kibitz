# Runbook (障害対応とローテーション)

平常時の操作は [deployment.md §7](deployment.md#7-運用)。この文書は**何かが
おかしいとき**と、**定期的に資格情報を入れ替えるとき**のためのもの。

## 0. まず見る 2 つ

kibitz の壊れかたは 2 通りしかない。**仕事を受け取らなくなる**か、
**受け取って落とす**か。どちらも PR 側からは「レビューが来ない」としか見えない。

```bash
# 受け取れているか (サーバー)
curl -s "$(terraform output -raw server_url)/metrics" | grep kibitz_webhooks_received_total

# 落としていないか (publish に失敗した数。0 以外ならレビューが失われている)
curl -s "$(terraform output -raw server_url)/metrics" | grep kibitz_publish_failures_total
```

`kibitz_publish_failures_total` が 0 でない時点で、**その数だけレビューが
失われている**。GitHub は自動で再送しないので、リポジトリの Webhook 設定から
手で再送する必要がある。

## 1. シークレットのローテーション

### 1-1. 無停止で入れ替えられるもの

**受信を検証する側の資格情報はすべてリスト**を取る。これはローテーションの
ためにそうしてある。

| 変数 | 使う側 |
| --- | --- |
| `KIBITZ_GITHUB_WEBHOOK_SECRETS` | GitHub の HMAC |
| `KIBITZ_GITLAB_WEBHOOK_TOKENS` | GitLab の共有トークン |
| `KIBITZ_GITLAB_SIGNING_TOKENS` | GitLab の署名トークン |
| `KIBITZ_AZDO_BASIC_PASSWORDS` | Azure DevOps の Basic 認証 |
| `KIBITZ_AZDO_HEADER_VALUES` | Azure DevOps の固定ヘッダ |

**新旧を並べて、送信側を切り替えてから、古いほうを消す。**

```bash
# 1. 新しい値を作る
NEW=$(openssl rand -hex 32)

# 2. 新旧を並べて Secret Manager に入れる (カンマ区切り)
OLD=$(gcloud secrets versions access latest --secret kibitz-github-webhook-secrets)
printf '%s,%s' "$OLD" "$NEW" | \
  gcloud secrets versions add kibitz-github-webhook-secrets --data-file=-

# 3. 新しいインスタンスに読み直させる (下記の注意)
gcloud run services update kibitz-server --region "$REGION" \
  --update-env-vars "KIBITZ_SECRET_REFRESH=$(date +%s)"

# 4. 送信側 (GitHub App の Webhook secret) を新しい値に変える

# 5. 数日置いて、配送が新しい値で通っていることを確認してから古いほうを消す
printf '%s' "$NEW" | gcloud secrets versions add kibitz-github-webhook-secrets --data-file=-
```

> **手順 3 を飛ばすと、手順 2 は効かない。**
> Cloud Run はシークレットを **インスタンスの起動時に 1 度だけ**読む。
> `version = "latest"` は「次に起動するインスタンスが最新を読む」という意味で、
> いま動いているインスタンスは古い値を持ち続ける。
> 何らかの環境変数を変えて新しいリビジョンを作るのが、確実な読み直しかた。

### 1-2. 交換の瞬間があるもの

**投稿する側の資格情報は 1 つしか持てない。** 入れ替えの瞬間に失敗しうる。

| 変数 | 影響 | やりかた |
| --- | --- | --- |
| `KIBITZ_GITHUB_PRIVATE_KEY` | clone と投稿 | GitHub App は**鍵を複数持てる**。新しい鍵を生成 → Secret 更新 → リビジョン更新 → 古い鍵を削除。**この順なら無停止** |
| `KIBITZ_GITLAB_TOKEN` | 同上 | 新しいトークンを発行 → Secret 更新 → リビジョン更新 → 古いトークンを失効。切り替え中に走っていたジョブは失敗しうるが、**再試行で拾われる** |
| `KIBITZ_AZDO_TOKEN` | 同上 | 同上 |

いずれも**キューを止めてから**やると、失敗するジョブが出ない。

```bash
# ワーカーを 0 台にする (キューには溜まる)
gcloud run worker-pools update kibitz-worker --region "$REGION" --min-instances 0 --max-instances 0
# ... 入れ替え ...
gcloud run worker-pools update kibitz-worker --region "$REGION" --max-instances 3
```

> **scaler が次の実行で台数を戻す。** 先に scaler のジョブを止めるか、
> 入れ替えを 1 分以内に終える。

### 1-3. 失効させるべきとき

| 出来事 | やること |
| --- | --- |
| Webhook secret が漏れた | 偽の配送が作れる。**即座に**入れ替え、`kibitz_webhooks_received_total` の急増を確認する |
| GitLab / Azure DevOps の API トークンが漏れた | **コードの読み取りと書き込み**ができる。先に失効させてから新しいものを入れる。順序が逆 |
| GitHub App の秘密鍵が漏れた | 同上。App の設定ページから該当の鍵を削除する |

## 2. アラート別の対応

Terraform が作る 4 つのアラート ([monitoring.tf](../deploy/terraform/gcp/monitoring.tf))。

### 2-1. `messages in the dead letter topic`

**レビューが失われている。** DLQ に入るのは、デコードできないメッセージか、
ワーカーが最後まで ack しなかったものだけ。

```bash
# 中身を見る (ack しないので消えない)
gcloud pubsub subscriptions pull kibitz-events-dead-hold --limit=10 --format=json
```

| 中身 | 原因 | 対応 |
| --- | --- | --- |
| `schema_version` が新しい | ワーカーが古い | ワーカーを先にデプロイする。イベントのスキーマは**前方互換ではない** |
| 正常に見える | ワーカーが繰り返し落ちた | ワーカーのログで同じ `event_id` を追う。原因を直してから再投入 |

再投入はトピックに publish し直す。**重複排除が効く**ので、二重にレビューされる
心配はない (`delivery:` キーで判定する)。

### 2-2. `review backlog is not draining`

イベントは来ているが処理されていない。

```bash
# 台数と、scaler が何を見て決めたか
gcloud run worker-pools describe kibitz-worker --region "$REGION" --format='value(scaling)'
gcloud run jobs executions list --job kibitz-scaler --region "$REGION" --limit 5
```

| 見え方 | 原因 |
| --- | --- |
| 台数 0 のまま | scaler が失敗している (§2-4)、または publish 時の起動が効いていない |
| 台数はあるが減らない | 1 件が長い。`KIBITZ_JOB_TIMEOUT` を超えていないか。モデルが遅い、または応答していない |
| 同じ PR で詰まる | ロックが解放されていない (§3-3) |

### 2-3. `the webhook server is refusing deliveries`

**サーバーが 5xx を返している = 配送が拒否されている。**
GitHub は自動で再送しないので、**直したあと手で再送する**必要がある。

原因はほぼ publish の失敗 (Pub/Sub への到達性、権限、トピックの消失)。

```bash
gcloud run services logs read kibitz-server --region "$REGION" --limit 50 | grep -i publish
```

再送はリポジトリの Webhook 設定 (GitHub なら App → Advanced → Recent Deliveries →
Redeliver)。

### 2-4. `the worker autoscaler is failing`

**すぐには見えない障害。** 台数が最後の値のまま固まるので、1 台で課金され続けるか、
溜まったキューを誰も捌かないかのどちらかになる。

よくある原因は 2 つだけ。

- 権限不足 — `roles/monitoring.viewer` と、**ワーカーの worker pool に対する**
  `roles/run.developer`
- サブスクリプション名の不一致

## 3. アラートにならない事故

### 3-0. ジョブが失敗した理由を調べる

PR に「レビューに失敗しました」と書かれたら、その末尾にある `event_id` で
ワーカーのログを検索する。**ジョブが書いたログはすべてこのキーを持っている**。

```bash
gcloud logging read 'jsonPayload.event_id="github:0f3c…"' --limit 50 --format json
```

エージェント (opencode) が失敗した場合は `msg="the agent failed"` のレコードが
1 件あり、そこにすべて揃っている。

| フィールド | 中身 |
| --- | --- |
| `cause` | **原因**。opencode 自身のログから、エラーイベントの `ref` で突き合わせたもの |
| `error_name` / `error_ref` | stdout に出たエラーイベント。メッセージは常に「サーバーログを見よ」なので、`cause` のほうを読む |
| `notices` | opencode が人向けに出した行。`Session not found`、エージェント定義が無くて既定のエージェントで動いた、など |
| `problems` | opencode のログの WARN / ERROR 行 (最大 20 行) |
| `status` / `exit_code` / `timed_out` / `duration` | プロセスの終わり方 |
| `finish` | 最後のステップをモデルがどう終えたか (`stop` / `tool-calls` / `unknown` など)。`unknown` は下の「途中で打ち切られた応答」 |
| `events` | stdout のイベントを種類ごとに数えたもの。**中身は残さない** (エージェントが読んだコードと書いた文章なので) |
| `stderr` | stderr の末尾 32 KB |

`cause` が空で `notices` も無いときは、`KIBITZ_OPENCODE_LOG_LEVEL=INFO` か `DEBUG` に
上げて再実行すると、opencode のログがもっと残る。

> **エージェント定義が見つからないと、opencode は既定のエージェントで黙って動く。**
> 失敗にはならないので、`msg="the agent reported problems but finished"` の
> `notices` に `agent "kibitz-…" not found. Falling back to default agent` と
> 出ていないかを見る。ワーカーのイメージに `deploy/opencode/agents/` が
> 入っていないと、こうなる。

> **`Requests ending with a model turn are not supported.` は自動で立て直す。**
> Gemini が opencode の知らない理由 (`OTHER`、`UNEXPECTED_TOOL_CALL`、
> `TOO_MANY_TOOL_CALLS` など) で応答を終えると、opencode はそれを `unknown` と
> 記録し、**何も足さずにもう一度問い合わせる**。その要求は最後がモデル自身の
> 発言になっていて、Vertex AI に拒否される。opencode 側の問題で、kibitz からは
> 直せない。
>
> kibitz はこの失敗だけを見分けて、**同じセッションに「続けて」と送り直す** (最大 2 回)。
> それまでの作業はセッションに残っているので、最初からやり直すより安い。
> 立て直したときは `msg="the model stopped for a reason opencode does not handle; resuming the session"`
> が WARN で出る。直前の `the agent failed` は立て直しの前の記録なので、
> 続けて成功していれば対応は不要。2 回とも失敗したらジョブの失敗になり、
> ジョブごと再試行される。

エージェントは完了したのにレビュー結果のファイルが無い場合は
`msg="the agent finished without writing its output"` で、`said_instead` に
エージェントが代わりに書いた文章の末尾 1000 字が入る。

### 3-1. モデルが応答しない

ジョブは `KIBITZ_MAX_DELIVERIES` 回まで再試行され、そこで諦めて **PR に失敗を
書く**。黙って消えることはない。

暫定対応は `KIBITZ_MODEL` の差し替え。`KIBITZ_MODEL_FALLBACK` は読み込まれるが、
**まだどこでも使われていない** (代替モデルへの自動切り替えは未実装)。

### 3-2. 同じ PR にコメントが増え続ける

ループ。`KIBITZ_MAX_POSTS_PER_HOUR` (既定 10) が最後の砦で、それ以上は投稿
されない。原因はほぼ**自分の投稿に反応している**こと。

```bash
gcloud run worker-pools logs read kibitz-worker --region "$REGION" --limit 50 | \
  grep "dropping an event kibitz authored itself"
```

これが出ていない場合、自分の判別ができていない。`KIBITZ_GITHUB_APP_ID` が
設定されているか、`KIBITZ_BOT_LOGINS` に実際の投稿者名が入っているかを確認する。

**止めかた**: 該当 PR に `/kibitz ignore` とコメントする。

### 3-3. 1 つの PR で詰まる

ワーカーが死ぬとロックが残る。リースには TTL があるので**自然に解放される**。
その長さは `KIBITZ_JOB_TIMEOUT` と同じ (既定 15 分) — ジョブより短いリースは
走っている最中に奪われるので、意図的に同じ値にしてある。

待てない場合は Firestore の該当ドキュメントを消す。**ドキュメント ID は
キーの `/` を `~` に置き換えたもの**で、URL エンコードではない。

```bash
# キー lock:github:acme/web:42 → ドキュメント ID lock:github:acme~web:42
gcloud firestore documents delete \
  "projects/$PROJECT/databases/(default)/documents/kibitz/lock:github:acme~web:42"
```

> **動いているワーカーがいる間に消してはいけない。** 2 台が同じ PR を
> 同時にレビューする。先に台数を 0 にする。

### 3-4. 予算で止まっている

PR に「今月の予算を使い切った」と出ている状態。**故障ではない**
([configuration.md](configuration.md#リポジトリ別の予算))。

```bash
gcloud run worker-pools logs read kibitz-worker --region "$REGION" --limit 50 | \
  grep "spent its budget"
```

引き上げるなら `KIBITZ_REPO_BUDGETS` を変えてワーカーのリビジョンを更新する。
**集計は翌月 1 日に自動で 0 に戻る** (キーに年月が入っているだけなので、
リセット操作は無い)。

### 3-5. 指摘が 1 件も出ない

モデルは動いているのにコメントが付かない場合、**差分の外だと判定されて
落ちている**可能性がある。

```bash
curl -s "$(terraform output -raw server_url)/metrics" | grep kibitz_findings_dropped_total
# kibitz_findings_dropped_total{reason="out_of_diff"} 37   ← これが多い
```

`out_of_diff` が多いのは、モデルが行番号を間違えているか、差分の解釈が
ずれている。Azure DevOps では差分を kibitz 自身が計算しているので、
そちらを疑う ([ADR-0004](adr/0004-platform-differences-live-in-two-places.md))。

## 4. 止める・戻す

```bash
# 受信だけ止める (キューに残った分は処理される)
#   GitHub: App の Webhook を Active から外す
#   GitLab: プロジェクトの Webhook を無効化
#   Azure DevOps: Service Hook を Disable

# 処理も止める (scaler を先に止めないと戻される)
gcloud scheduler jobs pause kibitz-scaler --location "$REGION"
gcloud run worker-pools update kibitz-worker --region "$REGION" --max-instances 0

# 再開
gcloud run worker-pools update kibitz-worker --region "$REGION" --max-instances 3
gcloud scheduler jobs resume kibitz-scaler --location "$REGION"
```

ロールバックは [deployment.md](deployment.md#ロールバック)。

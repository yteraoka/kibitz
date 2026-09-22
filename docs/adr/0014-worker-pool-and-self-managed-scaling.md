# worker は Cloud Run worker pool で動かし、台数は自分で決める

- **状態**: 採用 (2026-09-22、`52b6d94` の設計を PR #24 で置き換え)
- **関連**: `52b6d94` / PR #24 (`3fb0497`) / `docs/deployment.md`

## 背景

worker は Pub/Sub の pull subscriber である。**リクエストを受け取らない。**

これを Cloud Run **サービス**として動かすと、
プラットフォームの前提 (リクエストが来る) と実態が食い違い、
辻褄合わせが全方向に必要になった。

- リビジョンを ready にするための HTTP リスナと起動プローブ
- インスタンスが idle とみなされないようにする `concurrency = 1`
- 来ないリクエストの合間に CPU が絞られないようにする `cpu_idle = false`
- 誰も要らないドアを閉じるための internal-only ingress

そしてもう 1 つ。**pull subscriber は Cloud Run にスケールの材料を与えない。**
リクエスト数がゼロなので、プラットフォームは常にゼロか常に 1 かしか選べない。
結果、worker は 24 時間動き続けていた。

## 決定

**worker を Cloud Run worker pool として動かし、台数は kibitz 自身が決める。**

- worker pool は ingress もポートもプローブも無く、
  CPU はインスタンスの生存期間中ずっと割り当てられる。**辻褄合わせ 5 つが全部消える。**
- 台数は 2 つの仕組みで動く。
  - **サーバーは publish した瞬間に下限を 1 に上げる。**
  - **`kibitz-scaler`** が Cloud Run ジョブとして 1 分ごとに走り、
    Cloud Monitoring の `num_undelivered_messages` を読んで台数を書く。
    キューが `worker_idle_after` の間空なら `worker_min_instances` (ゼロ) に戻す。
- **書き換えるのはサービス (pool) 側のインスタンス数だけで、リビジョンテンプレートには触らない。**

## 理由

### なぜサーバーが下限を上げるのか

バックログはメトリクスであり、**メトリクスは数分遅れる**。
それを待つと、数字が追いつくまでプルリクエストが放置される。
だからサーバーは publish した時点で 1 台上げる。
この呼び出しはリクエストパスの外にあり、cooldown でまとめられる。

### `num_undelivered_messages` を使う理由

このメトリクスは**配信済みで未 ack のメッセージも数える**。
つまりレビューの最中の worker はまだメッセージを握っているので、
**その下からスケールで消されることがない**。

### バックログが読めないときに 1 台残す理由

**読めないことと空であることは同じではない。**
間違えたときのコストは、
一方が「idle なインスタンス 1 台」、もう一方が「誰も処理していないキュー」。

### リビジョンテンプレートに書かない理由

テンプレートを変更すると新しいリビジョンがロールし、**実行中のレビューが中断される**。
Terraform は初期値だけを設定してこのフィールドを ignore するので、
apply のたびにスケーラと打ち消し合うこともない。

### worker pool にしたことでスケール設計が簡単になった

worker pool には**オートスケールが無い**。
だからこの自前設計はそのまま残る。ただし単純になった。
`scaling.manualInstanceCount` は**下限ではなく台数そのもの**なので、
スケーラが計算した数がそのまま動く数になる。ゼロも受け付ける。

### 検証は記憶ではなく、インストール済みのものに対して行った

Go クライアント (`google.golang.org/api` v0.287.1) に
`ProjectsLocationsWorkerPools` の Get / Patch と `ManualInstanceCount` があること、
ピン留めしたプロバイダ (`hashicorp/google` 6.50.0) に
`google_cloud_run_v2_worker_pool` があり、そのコンテナスキーマに
ポート・プローブ・concurrency・`cpu_idle` が**無い**こと、
`gcloud run worker-pools` が GA で `--instances` が非負整数を取ること。

## 影響

- **`worker_max_instances` はリソースに設定しない。** この数字を動かすのは kibitz だけなので、
  上限は数字が決まる場所 (`KIBITZ_SCALE_MAX_INSTANCES`) に置く。
- `KIBITZ_SCALE_WORKER_SERVICE` は `KIBITZ_SCALE_WORKER_POOL` に改名。
  **フォールバックは置かない** — 改名する apply が、名指しするリソースごと置き換えるので。
- **worker pool はサービスからリネームできない。**
  この apply は `kibitz-worker` サービスを削除して pool を作る。
  キューにある仕事は失われない (未 ack は再配信される) が、
  **実行中のレビューは中断される**ので、キューが空のときに apply すること。
- サーバーとスケーラは worker に対する `roles/run.developer` だけを持つ。
  インスタンス数を変えられて、それ以外は何もできない。

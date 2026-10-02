# scaler は Cloud Run ジョブではなく、リクエスト課金の Cloud Run サービスとして動かす

- **状態**: 採用 (2026-10-02)
- **関連**: ADR-0014 (scaler の実行形態の部分を置き換える) / `cmd/kibitz-scaler` / `deploy/terraform/gcp/autoscale.tf` / `docs/deployment.md`

## 背景

ADR-0014 で、`kibitz-scaler` は **Cloud Run ジョブ**として Cloud Scheduler から毎分起動していた。

1 回の調整は「Monitoring を 1 回読み、worker pool に最大 1 回書く」だけで、
処理自体は 1 秒前後で終わる。ところがジョブの実行は、毎回コンテナを起動し、
**起動から終了まで**の 1 vCPU / 512Mi が課金される。
1 日 1,440 回実行しているので、**課金されている時間の大半は起動のオーバーヘッド**だった。
レビューが 1 件も無い日でも同じ額がかかる。

## 決定

**scaler を Cloud Run サービス (`cpu_idle = true`、つまりリクエスト課金) として動かし、
Cloud Scheduler から `POST /reconcile` で呼ぶ。**

- `kibitz-scaler -serve` は `POST /reconcile` 1 回につき `Scaler.Reconcile` を 1 回実行する。
  失敗は 500 で返す。
- `min_instance_count = 0`、`max_instance_count = 1`。
- ingress は internal のみ。Cloud Scheduler は OIDC トークン (scaler の SA) で呼び、
  `roles/run.invoker` は scaler の SA にだけ付ける。
- 1 回実行モードと `-loop` は残す (Cloud Run 以外で動かす場合のため)。

## 理由

### Cloud Functions ではなく Cloud Run サービスにした理由

Cloud Functions (第 2 世代、現 Cloud Run functions) の実体は Cloud Run サービスで、
課金も同じ。コストが下がるのは「リクエスト課金になる」からであって Functions だからではない。
Functions にすると Functions Framework とソースデプロイという**別のビルド経路**が増える。
今の distroless イメージをそのままサービスとしてデプロイすれば同じ効果が得られる。

### 状態を持たないので、HTTP にするだけで済む

`Reconcile` は毎回 `2 * IdleAfter` の窓でメトリクスを読み直し、
「台数がいつ変わったか」は worker pool の `updateTime` から読む。
プロセスの中に何も覚えていないので、ジョブの 1 回実行をリクエスト 1 回に置き換えても挙動は変わらない。

### 1 台・直列にした理由

Cloud Scheduler のリトライは、リトライ元の試行とまだ重なっていることがある。
2 つの調整が同時に台数を書くと、互いが変えている最中の数を見て判断することになる。
上限を 1 台にし、ハンドラはロックで 1 件ずつ処理する。毎分 1 回の呼び出しには 1 台で足りる。

### 失敗を 500 で返す理由

Cloud Scheduler は**ステータスコードでしか**失敗を知らない。
アラート (`scaler_failures`) も scaler サービスの 4xx / 5xx の件数で数えている。
本文にエラーを書いて 200 を返すと、失敗が見えなくなる。

### 4xx もアラートに含める理由

Cloud Scheduler が scaler を呼べない (invoker 権限が無い、ingress で弾かれる) とき、
scaler は一度も動かず、**scaler 自身のログには何も残らない**。
これはジョブ時代には `completed_execution_count{result="failed"}` に現れなかった種類の失敗でもある。

## 影響

- **apply でジョブが削除され、サービスが作られる。** 型が違うので `moved` では移せない。
  切り替えの間に 1〜2 回調整が抜けるだけで、キューにも実行中のレビューにも影響は無い
  (台数は最後の値のまま残る)。
- release workflow は `gcloud run jobs update` ではなく `gcloud run services update` を呼ぶ。
  デプロイ用 SA の権限もジョブからサービスに付け替わる。
- 調べ方が変わる: `gcloud run jobs executions list` ではなく、
  `resource.type=cloud_run_revision AND resource.labels.service_name=kibitz-scaler` のログを見る。
- イメージの ENTRYPOINT は変えていない。`-serve` は Terraform が `args` で渡す。

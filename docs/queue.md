# キュー設計 (Cloud Pub/Sub / Amazon SQS)

**GCP をメインとするため Cloud Pub/Sub を先に実装する。**
SQS 実装はインターフェースを満たす第二実装として後から追加する
([roadmap.md](roadmap.md) Phase X)。ただし SQS の 256 KB 制限を前提にした
Claim Check の設計は最初から入れておく (生 payload の保全にも使うため)。

## 1. 抽象化

```go
// internal/queue
type Message struct {
    ID         string
    Event      *event.ReviewEvent
    Attributes map[string]string // platform, kind, repo, pr, delivery_id, traceparent
    Deliveries int               // 配送回数 (分かる場合)
    ack        func() error
    nack       func() error
}

type Publisher interface {
    Publish(ctx context.Context, ev *event.ReviewEvent) (string, error)
    Close() error
}

type Subscriber interface {
    Receive(ctx context.Context, fn func(context.Context, *Message) error) error
    Close() error
}
```

バックエンドは `KIBITZ_QUEUE_BACKEND=pubsub|sqs|memory` で選択する。
`memory` はローカル開発とテスト用。

## 2. Cloud Pub/Sub

| 項目 | 設定 |
| --- | --- |
| トピック | `kibitz-events` |
| サブスクリプション | `kibitz-worker` (pull) |
| メッセージ順序 | 有効。ordering key = `{platform}/{repo_full_name}/{pr_number}` |
| ack 期限 | 60 秒 + クライアントライブラリの自動延長 (`MaxExtension` を 30 分に設定) |
| 配信保証 | at-least-once。必要なら exactly-once delivery を有効化 |
| 再試行 | 指数バックオフ (最小 10 秒 / 最大 600 秒) |
| DLQ | `kibitz-events-dead` へ、最大配信 5 回で送る |
| メッセージ上限 | 10 MB (正規化イベントは数 KB なので余裕) |
| フロー制御 | `MaxOutstandingMessages` = ワーカーの同時実行数と一致させる |

順序指定を使うと同一 PR のイベントは直列化される。これは「push 直後に連続して
イベントが来る」ケースで古い SHA のレビューを走らせないために有効。

## 3. Amazon SQS

| 項目 | 設定 |
| --- | --- |
| キュー | `kibitz-events.fifo` |
| MessageGroupId | `{platform}/{repo_full_name}/{pr_number}` (PR 単位で直列化) |
| MessageDeduplicationId | 正規化イベントの `source.delivery_id` (5 分間の重複排除) |
| 可視性タイムアウト | 初期 60 秒。処理中は `ChangeMessageVisibility` を 30 秒ごとに延長 |
| 受信 | ロングポーリング (`WaitTimeSeconds=20`)、`MaxNumberOfMessages` は同時実行数に合わせる |
| DLQ | `kibitz-events-dlq.fifo`、`maxReceiveCount=5` |
| メッセージ上限 | **256 KB** → 大きい payload は Claim Check が必須 |
| 高スループット | 必要なら FIFO の高スループットモード (グループ単位並列) を有効化 |

### FIFO を選ぶ理由とトレードオフ

- 利点: PR 単位の直列化が無料で手に入る。重複排除 ID も使える。
- 欠点: 1 つのメッセージが長時間処理されると同じグループの後続がブロックされる。
  これは「同じ PR の古いイベントを待たせる」意図と一致するので基本は問題ない。
  ただし処理が visibility timeout を超えて失われるとグループ全体が滞るため、
  延長ハートビートと最大実行時間 (デフォルト 15 分) を必ず設ける。
- 代替案: 標準キュー + 状態ストアの PR ロックで直列化する。
  クラウド差異を減らしたい場合はこちらでもよい (ロックは Pub/Sub 側にも必要な保険)。
  → **決定: FIFO + ロックの併用。** ロックはどちらのバックエンドでも有効にする。

## 4. Claim Check (大きな payload の退避)

```
サーバー:
  raw payload が N KB (既定 64 KB) を超える、または SQS バックエンドの場合:
    blobstore.Put("{platform}/{yyyy}/{mm}/{dd}/{delivery_id}.json.gz") → uri
    ReviewEvent.payload_ref = {uri, size, sha256}
ワーカー:
  raw payload が必要になった時だけ取得 (通常は正規化イベントだけで足りる)
  成功時にオブジェクトを削除、または保存先のライフサイクルで 7 日後に自動削除
```

生 payload を残す価値は、(a) 正規化のバグ調査、(b) 新しいイベント種別への後方互換対応、
(c) リプレイによる再現テスト。ただし PII を含むため保持期間は短くし、暗号化する。

## 5. 冪等性と再試行

多層で守る。

1. **Forge 側の再送**: GitHub は自動再送なし、GitLab / ADO はあり → 配送 ID で排除。
2. **キューの at-least-once**: 同じメッセージが 2 回届く → 配送 ID で排除。
3. **ワーカーのクラッシュ**: ack 前に落ちる → 再配送される。途中まで投稿済みの可能性があるため、
   投稿は「同一マーカーのコメントを upsert」「同じ (file, line, rule) のコメントは重複投稿しない」形にする。
   配送の記録は「予約 (claim)」と「完了 (done)」を区別しており、**予約だけが残っている配送は
   完了扱いにしない**。実行中かどうかを決めるのはロックのほうで、ロックが空いていれば
   再配送したワーカーがそのまま引き継ぐ。Cloud Run のリビジョン入れ替えは
   まさにこの形でジョブを中断するため、ここを取り違えるとレビューが黙って消える。
4. **同一 PR への連続 push**: 古い SHA のジョブは、状態ストアの `latest_sha` と比較して
   自分が最新でなければ即 ack して破棄する。

```go
// ジョブ本体の冒頭
first, err := store.MarkProcessed(ctx, "delivery:"+ev.Source.Platform+":"+ev.Source.DeliveryID, 7*24*time.Hour)
if err != nil { return err }          // nack → 再配送
if !first { return nil }              // 処理済み → ack

lease, err := store.AcquireLock(ctx, lockKey(ev), 15*time.Minute)
if errors.Is(err, store.ErrLocked) { return queue.ErrRetryAfter(30 * time.Second) }
defer lease.Release(ctx)
```

## 6. エラー分類

| 分類 | 例 | 処理 |
| --- | --- | --- |
| 恒久的 (`worker.Permanent`) | 削除済み PR、権限なし、対応していないプラットフォーム | ack。PR に失敗を通知して再試行しない |
| 一時的 | Forge API 5xx / レートリミット、ネットワーク断、状態ストア到達不能 | nack。バックオフ後に再配送 |
| 競合 (`worker.RetryAfter`) | 同じ PR を別のワーカーが処理中 | nack (遅延付き)。ログレベルは info |
| 再試行上限 | 同じジョブが `KIBITZ_MAX_DELIVERIES` 回失敗 | ack。PR に失敗を通知する |
| AI 起因 | 出力スキーマ不一致 | 1 回だけ再実行 (検証エラーをプロンプトに添える)。それでも失敗なら上記の再試行扱い |
| デコード不能 | 未知の schema_version、壊れたメッセージ | nack。**サブスクリプションの DLQ ポリシーに任せる** |

**DLQ に実際に入るのは「デコードできないメッセージ」と「ワーカーが応答せず ack 期限を
超え続けたメッセージ」だけ**である点に注意。ジョブが上限まで失敗した場合は DLQ に送らず、
ack したうえで PR にコメントする。理由は、DLQ に溜まった無言のメッセージより
「レビューに失敗しました」と書かれた PR コメントのほうが早く気づかれるため。
DLQ の監視は依然として必要 (Terraform でアラートを設定する、Phase 9)。

「失敗したことを PR に伝える」のは重要で、沈黙して DLQ に落ちるとユーザーは待ち続けてしまう。
DLQ には CloudWatch / Cloud Monitoring のアラートを設定する。

## 7. スループットとコスト

- Webhook のバースト (一括 push、大量 PR の rebase) はキューが吸収する。
- ワーカーはキューの滞留数 (`num_undelivered_messages` / `ApproximateNumberOfMessagesVisible`) で
  水平オートスケールする。上限はモデル API のレートリミットとコスト予算で決める。
- 1 ジョブあたりの上限 (実行時間、トークン、変更ファイル数) を必ず設定し、
  暴走する PR (巨大な自動生成差分など) でコストが跳ねないようにする。

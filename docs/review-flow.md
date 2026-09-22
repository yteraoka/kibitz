# レビュー処理の流れ

Webhook が届いてからレビューコメントが付くまでに、何がどの順番で起きるか。
「なぜレビューされなかったのか」を追うときの地図でもある。

コンポーネントの役割分担は [architecture.md](architecture.md)、
キューの詳細は [queue.md](queue.md)、
エージェントの実行方法は [worker.md](worker.md) を参照。

> 図は Mermaid で書いてある。GitHub 上ではそのまま図として表示される。

## 1. 全体

Webhook の応答は**数秒以内に返さないと Forge 側がタイムアウトする**のに対して、
レビューは数分かかる。そのため境界はキューにあり、サーバーは
「検証・正規化・判定・publish」だけをして 202 を返す。

```mermaid
sequenceDiagram
    autonumber
    actor Dev as 開発者
    participant Forge as GitHub / GitLab
    participant Server as kibitz-server
    participant Queue as Pub/Sub
    participant Scaler as kibitz-scaler
    participant Worker as kibitz-worker
    participant Agent as OpenCode + モデル

    Dev->>Forge: PR を作る / コメントする
    Forge->>Server: POST /webhook/github
    Server->>Server: 署名検証 → 正規化 → policy 判定
    alt 対象外
        Server-->>Forge: 204 No Content
    else 対象
        Server->>Queue: publish、ordering key は PR 単位
        Server-)Scaler: Wake、インスタンス数 0 からの起動を促す
        Server-->>Forge: 202 Accepted
    end

    Queue->>Worker: 配送、at-least-once
    Worker->>Forge: PR / diff / 既存コメント / .kibitz.yaml を取得
    Worker->>Worker: shallow clone
    Worker->>Agent: プロンプトを渡して実行
    Agent-->>Worker: .kibitz/out/review.json
    Worker->>Worker: スキーマ検証、行番号検証、重複排除、件数制限
    Worker->>Forge: サマリコメント + インライン指摘
    Worker->>Queue: ack
    Forge-->>Dev: 通知
```

`Wake` を別に送っているのは、pull 購読にはオートスケールの根拠になる
inbound リクエストが無いため。バックログのメトリクスは数分遅れるので、
それを待つとレビューの開始が遅れる ([deployment.md](deployment.md#ワーカーのオートスケール))。

## 2. サーバー側 — 検証から publish まで

順番に意味がある。**検証は生のバイト列に対して、パースより前に**行う。

```mermaid
sequenceDiagram
    autonumber
    participant Forge
    participant R as webhook.Receiver
    participant H as webhook/github.Handler
    participant P as policy.Engine
    participant Q as queue.Publisher

    Forge->>R: POST body + 署名ヘッダ
    R->>R: body を読む、既定 25 MB で打ち切り
    R->>H: Verify、生バイト列の HMAC
    opt 署名が不一致
        H-->>R: error
        R-->>Forge: 401
    end
    R->>H: Normalize
    opt 扱わないイベント
        H-->>R: nil
        R-->>Forge: 204、理由は unsupported_event
    end
    H-->>R: ReviewEvent
    R->>P: Evaluate
    Note over P: 自分自身の発言か / 許可リポジトリか /<br/>メンションかコマンドか / キーワードを含むか
    opt Publish false
        P-->>R: Reason
        R-->>Forge: 204、理由をログとメトリクスに残す
    end
    R->>Q: Publish、失敗したら 3 回まで指数バックオフ
    alt publish 失敗
        R-->>Forge: 503、Forge 側の再送とログに残す
    else
        R-->>Forge: 202
    end
```

判定理由 (`policy.Reason`) はログにもメトリクスのラベルにも出る。
「なぜレビューされなかったのか」に答えられるようにするため
([event-schema.md](event-schema.md))。

publish 失敗に 503 を返すのは、**GitHub が自動で再送しないから**。
せめて hook の delivery log に失敗として残し、手動の Redeliver ができるようにする。

## 3. ワーカー側 — Guard

キューは at-least-once なので、同じメッセージが 2 回来る前提で組んである。
`worker.Guard` がその面倒を引き受け、実際のレビューは `ReviewJob` が見る。

```mermaid
sequenceDiagram
    autonumber
    participant Q as Pub/Sub
    participant W as worker.Worker
    participant G as worker.Guard
    participant S as Firestore
    participant J as ReviewJob

    Q->>W: Message
    W->>W: ジョブ用の context、既定 15 分でタイムアウト
    W->>G: Handle
    G->>G: 発言者が kibitz 自身なら捨てる、コメントループの二重防止
    G->>S: MarkProcessed、配送 ID で claim
    opt 既に完了済み
        S-->>G: claimed false かつ done
        G-->>W: nil、ack して終わり
    end
    G->>S: AcquireLock、PR 単位
    opt 他のワーカーが処理中
        S-->>G: ErrLocked
        G-->>W: RetryAfter 30s、claim は返す
    end
    G->>G: リース延長を開始、失効したら job の context を cancel
    G->>J: Handle
    J-->>G: 結果
    G->>S: リース解放
    alt 成功
        G->>S: remember、完了として記録
        G-->>W: nil
    else 失敗かつ再試行する
        G->>S: claim を返す
        G-->>W: error、メッセージは再配送される
    else 失敗かつ諦める
        Note over G: 恒久的なエラー、または配送 5 回目
        G->>J: NotifyFailure、PR に失敗を書く
        G->>S: remember
        G-->>W: nil、ack して DLQ にも送らない
    end
```

諦めるときに ack するのは、**同じ失敗を 5 回繰り返しても結果は変わらない**から。
代わりに PR に理由を書く。黙って消えるのが一番困る。

リースが失効したら job の context を cancel するのは、
2 つのワーカーが同じ PR を同時にレビューするのを防ぐため。

## 4. レビュー本体

`ReviewJob.review()`。**安い判定から順に並べてある**ので、
やらないと決まる場合ほど早く終わる。

```mermaid
sequenceDiagram
    autonumber
    participant J as ReviewJob
    participant F as forge.Client
    participant S as Firestore
    participant WS as workspace
    participant A as reviewer.Engine

    J->>F: PullRequest、イベントの内容は古いので取り直す
    opt closed かつ未マージ
        Note over J: ここで終了
    end
    J->>F: ReadFile AGENTS.md / .kibitz/guidelines.md / .kibitz.yaml
    Note over J: すべて**デフォルトブランチ側**から。<br/>指示の位置に入るものは PR 側から読まない
    Note over J: review.enabled / triggers / skip_draft で<br/>走らせるかを決める
    J->>S: ignore されていないか
    J->>S: この SHA は既にレビュー済みか
    Note over J: コマンドのときは両方とも素通し、<br/>明示的な依頼だから
    J->>F: Diff
    J->>J: paths_ignore で除外、diff 自体から落とす
    opt 前回レビュー済みの SHA がある
        J->>F: Compare、前回からの差分だけに絞る
    end
    J->>F: Comments、既存の指摘、失敗しても続行
    J->>F: CloneAuth、短命な資格情報
    J->>WS: init + fetch + checkout、shallow
    opt 差分が大きい、既定 10000 行超
        J->>A: triage、ファイル名とサイズだけ見て読む対象を選ぶ
        A-->>J: 選抜結果
    end
    J->>A: Run、ModeReview
    A-->>J: Result
    opt 出力がスキーマに合わない
        J->>A: 検証エラーを添えてもう一度だけ
    end
    J->>J: Sanitize、行番号 / severity / 重複 / 件数上限
    J->>S: この SHA をレビュー済みとして記録
    J->>F: 投稿
    J->>WS: 破棄
```

要点:

- **イベントの内容を信用しない。** Webhook が飛んだ時点のスナップショットは
  ジョブが動く頃には古い。Azure DevOps では署名が無いのでそもそも信用できない
- **`.kibitz.yaml` はデフォルトブランチから読む。** PR 側から読むと、
  PR を開いた人がその PR のレビューのされ方を決められてしまう
  ([security.md](security.md))
- **`paths_ignore` は diff そのものに効かせる。** 指摘の検証も同じ diff で行うので、
  エージェントが除外ファイルを読んで指摘してきても投稿されない
- **増分レビューは差分を絞るだけ。** 指摘の行番号は常に PR 全体の diff に対して
  検証するので、範囲外にコメントが付くことはない
- **triage は名前とサイズしか見ない。** 中身まで読んだら、節約するはずのものを使ってしまう

### スキップの判断順序

上から順に見て、最初に当たったところで終わる。理由はログに残る。

| 判定 | 場所 | コマンドのとき |
| --- | --- | --- |
| 自分自身の発言 | server の policy → worker の Guard (二重) | 同じく捨てる |
| リポジトリが許可リストに無い | server の policy | 同じく捨てる |
| メンションもコマンドも無い | server の policy | — |
| タイトル / 本文にキーワードが無い | server の policy | 素通し |
| PR が closed かつ未マージ | `review()` | 同じくスキップ |
| `review.enabled` が false / `triggers` に無い | `review()` | `triggers` は素通し |
| draft かつ `skip_draft` | `review()` | 素通し |
| `/kibitz ignore` されている | `review()` | `review` が解除する |
| この SHA はレビュー済み | `review()` | 素通し |
| 差分が空、または全ファイルが `paths_ignore` | `review()` | 同じくスキップ |
| 1 時間の投稿数上限、既定 10 | `post()` | 同じく止まる |

最後の投稿数上限だけは、**コマンドでも解除されない**。
コメントループに対する最後の防波堤なので、ここで止まるときは ERROR でログに出す。

## 5. エージェントの実行

`reviewer/opencode`。CLI をヘッドレスで叩き、**結果はファイルで受け取る**。
stdout のテキストをパースするより堅い。

```mermaid
sequenceDiagram
    autonumber
    participant J as ReviewJob
    participant R as opencode.Runner
    participant CLI as opencode
    participant M as Vertex AI

    J->>R: Request、diff / PR / 既存コメント / guidelines / focus
    R->>R: ジョブ用の opencode.json を書く、権限は読み取りのみ
    R->>R: プロンプトを workspace の .kibitz/prompt.md に書く
    R->>CLI: opencode run --format json --agent ... --auto "指示" --file .kibitz/prompt.md
    loop ステップごと
        CLI->>M: 推論
        M-->>CLI: ツール呼び出し / テキスト
        CLI->>CLI: read / grep などでリポジトリを読む
        CLI-->>R: stdout に JSON イベント、1 行 1 件
    end
    CLI->>CLI: .kibitz/out/review.json を書く
    CLI-->>R: 終了
    R->>R: step_finish の part.tokens を合算、トークン数
    R->>R: review.json を読んで ParseOutput
    alt スキーマ違反
        R-->>J: OutputError、呼び出し側が 1 回だけ再試行
    else
        R-->>J: Result
    end
```

- `--file` は配列オプションなので**必ず最後に置く**。
  後続の引数まで添付ファイル名として食われる
- トークン数は `step_finish` イベントの `part.tokens` にしか無く、
  しかもステップごとの値なので合算が必要
  ([worker.md](worker.md#stdout-の-json-イベント))
- セッションが消えていたら、セッション無しでもう一度実行する。
  ワーカーのコンテナは使い捨てなので、消えているのは異常ではない

## 6. 投稿

```mermaid
sequenceDiagram
    autonumber
    participant J as ReviewJob
    participant S as Firestore
    participant F as forge.Client

    J->>S: Incr、この 1 時間の投稿数
    opt 上限超過
        Note over J: ERROR ログを出して投稿しない
    end
    J->>F: UpsertSummary、marker で 1 件を差し替える
    opt 指摘がある
        J->>F: CreateReview、インライン指摘をまとめて 1 レビューに
        opt 位置を拒否された
            Note over J: Forge は 1 件の不正な位置で<br/>レビュー全体を拒否する
            J->>F: UpsertSummary、指摘を本文に書いて投稿し直す
        end
    end
```

サマリを upsert にしてあるので、同じ PR を何度レビューしてもサマリは 1 件のまま。
`<!-- kibitz:summary -->` のような marker で自分のコメントを見つける。

## 7. 質問への回答

コメントがメンションを含み、コマンドでなければ質問として扱う。

```mermaid
sequenceDiagram
    autonumber
    actor Dev as 開発者
    participant F as forge.Client
    participant J as ReviewJob
    participant A as reviewer.Engine

    Dev->>F: 指摘に「なぜ?」と返信
    F->>J: comment.created
    J->>F: PullRequest / Diff / Comments
    J->>J: そのスレッドを切り出す、質問自身は除く
    Note over J: kibitz 自身の発言も含める。<br/>「なぜ?」は、その上の指摘が無いと意味を成さない
    J->>J: workspace を用意
    J->>A: Run、ModeAnswer、保存済みセッション ID があれば渡す
    A-->>J: 回答テキスト、stdout から
    J->>F: ReplyToThread、同じスレッドに返す
```

セッションの継続は**最適化**として実装してある。ワーカーのコンテナは使い捨てで、
レビューと追質問の間に消えていることが多いため、文脈はセッションではなく
毎回プロンプトに入れる ([worker.md](worker.md))。

## 8. 失敗したとき

```mermaid
sequenceDiagram
    autonumber
    participant J as ReviewJob
    participant G as worker.Guard
    participant Q as Pub/Sub
    participant F as forge.Client

    J-->>G: error
    alt 恒久的なエラー
        Note over G: 設定ミス、404、スキーマ違反の再試行後など
        G->>F: NotifyFailure、PR に理由を書く
        G->>Q: ack
    else 配送回数が上限、既定 5 回
        G->>F: NotifyFailure
        G->>Q: ack
    else それ以外
        G->>Q: nack、再配送させる
        Note over Q: 再配送の上限を超えたメッセージは DLQ へ
    end
```

再試行するかどうかは `internal/worker/errors.go` の分類による。
「何度やっても同じ」ものを再試行しても、レートリミットを消費するだけで結果は変わらない。

エラー分類と実際の再試行回数、DLQ に入る条件は [queue.md](queue.md#6-エラー分類)。

## 関連

| ドキュメント | 内容 |
| --- | --- |
| [architecture.md](architecture.md) | コンポーネントの役割、インターフェース定義 |
| [event-schema.md](event-schema.md) | 正規化イベント、publish の判定条件 |
| [queue.md](queue.md) | 順序制御、冪等性、リトライ、DLQ |
| [worker.md](worker.md) | プロンプト設計、OpenCode の実行、構造化出力 |
| [configuration.md](configuration.md) | 環境変数、`.kibitz.yaml` |
| [security.md](security.md) | 署名検証、プロンプトインジェクション、fork PR |

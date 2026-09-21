# アーキテクチャ

## 1. 設計方針

1. **受信と実行を分離する**
   Webhook の応答は数秒以内に返す必要がある (GitHub は 10 秒でタイムアウト)。
   一方 AI レビューは数十秒〜数分かかる。両者をキューで分離し、サーバーは I/O のみ、
   ワーカーは長時間処理に専念する。
2. **プラットフォーム差異は境界で吸収する**
   Webhook パーサ (入口) と Forge クライアント (出口) だけがプラットフォーム固有。
   中間のイベント・キュー・レビュー生成は共通コードで動かす。
3. **クラウド差異も境界で吸収する**
   `queue.Publisher` / `queue.Subscriber` / `blobstore.Store` / `store.StateStore` の
   4 つのインターフェースの裏に Pub/Sub 版と SQS/S3/DynamoDB 版を置く。
   テスト用にインメモリ実装も用意する。
4. **AI 実行は差し替え可能にする**
   `reviewer.Engine` インターフェースの実装として OpenCode を使う。
   OpenCode の呼び出し方 (`opencode run` / `opencode serve` への HTTP) も実装差し替えで選べるようにする。
5. **失敗は再試行、二重実行は冪等性で防ぐ**
   キューは at-least-once 前提。配送 ID とジョブキーで重複実行を抑止する。

## 2. コンポーネント

### 2.1 kibitz-server

Webhook を受け取り、検証して正規化イベントを publish するだけのステートレス HTTP サーバー。

| エンドポイント | 用途 |
| --- | --- |
| `POST /webhook/github` | GitHub / GitHub Enterprise Server |
| `POST /webhook/gitlab` | GitLab.com / self-managed |
| `POST /webhook/azuredevops` | Azure DevOps Services / Server |
| `GET /healthz` | Liveness |
| `GET /readyz` | Readiness (キュー接続確認を含む) |
| `GET /metrics` | Prometheus メトリクス (別ポート/別リスナーで公開) |

処理フロー:

```
1. Body を http.MaxBytesReader で上限付き読み込み (生バイト列を保持)
2. 署名 / トークン検証   ← パース前に必ず実行 (docs/security.md)
3. プラットフォーム固有 payload → event.ReviewEvent へ正規化
4. トリガ判定 (対象外なら 204 を返して publish しない)
   - 自分自身 (bot) の発言 → 無視 (無限ループ防止)
   - draft PR / 除外パスのみの変更 / 対象外イベント種別 → 無視
   - コメントはコマンド (例: "@kibitz review") かどうか判定
5. 必要なら生 payload を blobstore に退避し、参照だけをイベントに載せる (Claim Check)
6. queue.Publisher.Publish() — 短いタイムアウト + 数回のリトライ
7. 202 Accepted (publish 失敗時のみ 5xx を返し、Forge 側の再送に委ねる)
```

標準ライブラリの `net/http` + Go 1.22 の `http.ServeMux` を使い、Web フレームワークは入れない。
ミドルウェアは request ID 付与、`slog` による構造化ログ、パニックリカバリ、タイムアウトの 4 つ。

### 2.2 kibitz-worker

キューを購読し、1 メッセージ = 1 ジョブとして処理する常駐プロセス。

```
1. メッセージ受信 → event.ReviewEvent にデコード (schema_version を確認)
2. 冪等性チェック: delivery_id が処理済みなら即 ack して終了
3. ジョブロック取得: 同一 PR の同時実行を防ぐ (lease 付き)
   - 同一 PR に新しい head SHA のジョブが来ていたら古いジョブはキャンセル
4. Forge クライアント生成 (インストールトークン / PAT を短命で取得)
5. リポジトリ設定 .kibitz.yaml を取得しマージ (docs/configuration.md)
6. ワークスペース準備: shallow clone + PR ref を fetch、diff を取得
7. プロンプト構築 → reviewer.Engine.Run() で OpenCode 実行 (MCP 有効)
8. 構造化出力 (JSON) を読み取り、検証・件数制限・重複排除
9. Forge API へ投稿 (レビュー本体 / インラインコメント / スレッド返信)
10. 状態を記録して ack。失敗時は nack → 再配送、上限超過で DLQ
```

長時間処理のため、処理中は ack 期限 (Pub/Sub) / 可視性タイムアウト (SQS) を延長し続ける。
OpenCode の同時実行数はセマフォで制限する (メモリとトークン消費が大きいため)。

### 2.3 Forge クライアント

プラットフォームごとの読み書きを 1 つのインターフェースに閉じ込める。

```go
// internal/forge
type Client interface {
    // 読み取り
    PullRequest(ctx context.Context, ref PRRef) (*PullRequest, error)
    Diff(ctx context.Context, ref PRRef) (*Diff, error)           // 変更ファイル + patch
    FileAtRef(ctx context.Context, ref PRRef, path, rev string) ([]byte, error)
    Comments(ctx context.Context, ref PRRef) ([]Comment, error)

    // 書き込み
    CreateReview(ctx context.Context, ref PRRef, r Review) error  // まとめて投稿
    ReplyToThread(ctx context.Context, ref PRRef, threadID, body string) error
    UpsertSummary(ctx context.Context, ref PRRef, marker, body string) error // 同一 marker のコメントを更新

    // 認証・クローン
    CloneAuth(ctx context.Context, ref PRRef) (CloneCredential, error)
}
```

`UpsertSummary` は「PR ごとにサマリコメントは 1 つだけ」を実現するためのもので、
本文に HTML コメントのマーカー (`<!-- kibitz:summary -->`) を埋めて既存コメントを特定・更新する。

プラットフォームごとの実体:

| 操作 | GitHub | GitLab | Azure DevOps |
| --- | --- | --- | --- |
| レビュー投稿 | `POST /repos/{o}/{r}/pulls/{n}/reviews` (`comments[]` にまとめる) | `POST /projects/:id/merge_requests/:iid/discussions` を件数分 | `POST .../pullRequests/{id}/threads` を件数分 |
| インライン位置 | `path` + `line` + `side` | `position` (`base_sha`/`start_sha`/`head_sha`/`new_path`/`new_line`) | `threadContext` (`filePath` + `rightFileStart/End`) |
| スレッド返信 | `POST .../pulls/comments/{id}/replies` | `POST .../discussions/{discussion_id}/notes` | `POST .../threads/{threadId}/comments` |
| 差分取得 | `GET .../pulls/{n}/files` または `Accept: application/vnd.github.v3.diff` | `GET .../merge_requests/:iid/diffs` | `GET .../pullRequests/{id}/iterations/{it}/changes` |
| PR ref | `refs/pull/{n}/head` | `refs/merge-requests/{iid}/head` | `refs/pull/{id}/merge` |
| 認証 | GitHub App (JWT → installation token, 1h) | PAT / Group Access Token / OAuth | PAT / Microsoft Entra ID (Service Principal) |

レートリミットは各クライアント内で処理する (`Retry-After` / `X-RateLimit-Remaining` を見た待機、
指数バックオフ + ジッタ、`golang.org/x/time/rate` による送信レート制御)。

### 2.4 状態ストア

```go
// internal/store
type StateStore interface {
    // 冪等性: 初回のみ true。TTL 付き
    MarkProcessed(ctx context.Context, key string, ttl time.Duration) (first bool, err error)
    // 排他: PR 単位のリース
    AcquireLock(ctx context.Context, key string, ttl time.Duration) (Lease, error)
    // 任意の小さな状態 (OpenCode セッション ID、直近レビュー済み SHA、トークン使用量)
    Get(ctx context.Context, key string) ([]byte, error)
    Put(ctx context.Context, key string, v []byte, ttl time.Duration) error
}
```

キー設計:

| キー | 内容 | TTL |
| --- | --- | --- |
| `delivery:{platform}:{delivery_id}` | 配送単位の重複排除 | 7d |
| `job:{platform}:{repo}:{pr}:{kind}:{head_sha}` | 同じ内容の再レビュー抑止 | 30d |
| `lock:{platform}:{repo}:{pr}` | PR 単位の実行ロック (lease) | 15m |
| `session:{platform}:{repo}:{pr}` | OpenCode セッション ID (追加質問の文脈継続用) | 30d |
| `budget:{platform}:{repo}:{yyyymm}` | トークン / コスト使用量 | 60d |

実装は GCP なら Firestore、AWS なら DynamoDB (条件付き書き込みで lock を実現)、
ローカル / 単一ノードならインメモリ、必要なら Redis 実装を追加する。

## 3. ディレクトリ構成

```
.
├── cmd/
│   ├── kibitz-server/main.go
│   └── kibitz-worker/main.go
├── internal/
│   ├── config/            # 環境変数・設定ファイルの読み込みと検証
│   ├── httpx/             # ミドルウェア、ヘルスチェック、graceful shutdown
│   ├── webhook/           # 検証 + 正規化
│   │   ├── webhook.go     #   type Handler interface { Verify, Normalize }
│   │   ├── github/
│   │   ├── gitlab/
│   │   └── azuredevops/
│   ├── event/             # 正規化イベントスキーマ (バージョン付き)
│   ├── queue/
│   │   ├── queue.go       #   Publisher / Subscriber / Message
│   │   ├── pubsub/
│   │   ├── sqs/
│   │   └── memory/
│   ├── blobstore/         # Claim Check 用 (gcs / s3 / memory)
│   ├── store/             # StateStore (firestore / dynamodb / memory)
│   ├── forge/             # Client (github / gitlab / azuredevops)
│   ├── workspace/         # clone, fetch, cleanup, サイズ制限
│   ├── reviewer/
│   │   ├── engine.go      #   Engine interface
│   │   ├── opencode/      #   run 方式 / serve 方式
│   │   ├── prompt/        #   テンプレートと diff 整形
│   │   └── result.go      #   構造化出力のスキーマと検証
│   ├── policy/            # トリガ判定、フィルタ、予算・件数制限
│   └── telemetry/         # slog, OpenTelemetry, メトリクス
├── deploy/
│   ├── docker/            # server / worker の Dockerfile
│   ├── terraform/         # gcp / aws モジュール
│   └── helm/              # Kubernetes 用 (任意)
├── testdata/              # 実 Webhook payload のフィクスチャ
└── docs/
```

Go モジュールパス: `github.com/yteraoka/kibitz`。Go 1.24 系を使用。

## 4. 主要インターフェース (抜粋)

```go
// internal/webhook
type Handler interface {
    Platform() event.Platform
    // Verify は生ボディとヘッダから真正性を検証する。パース前に呼ぶこと。
    Verify(r *http.Request, body []byte) error
    // Normalize は対象外イベントの場合 (nil, nil) を返す。
    Normalize(r *http.Request, body []byte) (*event.ReviewEvent, error)
}

// internal/queue
type Publisher interface {
    Publish(ctx context.Context, ev *event.ReviewEvent) (messageID string, err error)
    Close() error
}

type Subscriber interface {
    // Receive は fn が nil を返せば ack、error を返せば nack する。
    // fn の実行中は ack 期限 / 可視性タイムアウトを自動延長する。
    Receive(ctx context.Context, fn func(context.Context, *Message) error) error
    Close() error
}

// internal/reviewer
type Engine interface {
    Run(ctx context.Context, req Request) (*Result, error)
}

type Request struct {
    Workspace   string        // clone 済みディレクトリ
    Event       *event.ReviewEvent
    Diff        *forge.Diff
    RepoConfig  *config.RepoConfig
    SessionID   string        // 空なら新規、既存なら文脈を継続
    Mode        Mode          // ModeReview | ModeAnswer
    Deadline    time.Duration
}

type Result struct {
    Summary   string
    Comments  []InlineComment  // file, line, severity, body, (任意) suggestion
    Reply     string           // ModeAnswer のときの返信本文
    SessionID string
    Usage     Usage            // tokens, cost, duration
}
```

## 5. デプロイ形態

| コンポーネント | GCP | AWS |
| --- | --- | --- |
| server | Cloud Run (HTTP) | ECS Fargate + ALB、または Lambda + API Gateway |
| queue | Cloud Pub/Sub | SQS FIFO (+ DLQ) |
| worker | GKE / Cloud Run worker pool (pull 購読) | ECS Fargate (常駐) |
| blob | Cloud Storage | S3 |
| state | Firestore | DynamoDB |
| secret | Secret Manager | Secrets Manager / SSM Parameter Store |

ワーカーは Pub/Sub の push 購読ではなく **pull 購読**にする。処理が数分に及ぶため、
ack 期限の延長を自前で制御できる pull のほうが扱いやすい。

ワーカーのコンテナイメージには `opencode` バイナリ、`git`、`ripgrep`、
MCP サーバーを `npx` / `bunx` で起動するための Node.js または Bun を含める。

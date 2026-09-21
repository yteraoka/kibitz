# Webhook とイベント正規化

## 1. プラットフォーム対応表

| 項目 | GitHub | GitLab | Azure DevOps |
| --- | --- | --- | --- |
| 用語 | Pull Request | Merge Request | Pull Request |
| 検証方式 | HMAC-SHA256 (`X-Hub-Signature-256: sha256=...`) | 共有トークン一致 (`X-Gitlab-Token`) | Basic 認証 + 任意のカスタムヘッダ (HMAC なし) |
| イベント種別 | `X-GitHub-Event` ヘッダ | `X-Gitlab-Event` ヘッダ + body の `object_kind` | body の `eventType` |
| 配送 ID | `X-GitHub-Delivery` | `X-Gitlab-Event-UUID` | body の `id` |
| PR 作成 | `pull_request` / `action=opened` | `merge_request` / `action=open` | `git.pullrequest.created` |
| PR 更新 (push) | `pull_request` / `action=synchronize` | `merge_request` / `action=update` (`oldrev` あり) | `git.pullrequest.updated` |
| PR 本文コメント | `issue_comment` / `action=created` | `note` / `noteable_type=MergeRequest` | `ms.vss-code.git-pullrequest-comment-event` |
| コード行コメント | `pull_request_review_comment` | `note` (`position` あり) | 同上 (`threadContext` あり) |
| レビュー依頼 | `pull_request` / `action=review_requested` | (なし。`reviewers` の差分で判定) | `git.pullrequest.updated` の reviewers 変化 |
| リトライ | 自動再送なし (手動 redeliver) | 失敗時にバックオフ再送・連続失敗で無効化 | 失敗継続で subscription が無効化される |
| Payload 上限 | 25 MB | 制限は緩いが大きい | 比較的小さい |

重要な非対称性:

- **Azure DevOps には HMAC 署名がない。** Service Hooks の Basic 認証 + 固定ヘッダ + 送信元 IP 制限 (可能なら) で代替する。詳細は [security.md](security.md)。
- **Azure DevOps の PR コメント通知には diff 位置が薄い。** 補完のため worker 側で API から thread を取り直す。
- **GitLab の `note` は MR 以外 (Issue, Commit, Snippet) にも飛ぶ。** `object_attributes.noteable_type` で絞る。
- **GitHub は Webhook を自動再送しない。** publish に失敗して 5xx を返しても救済されないため、サーバー内で数回リトライしてから諦める。

## 2. 正規化イベント (`event.ReviewEvent`)

キューに載せる唯一のスキーマ。プラットフォーム固有の語彙はここで消える。

```json
{
  "schema_version": 1,
  "id": "01JBX2...",
  "occurred_at": "2026-09-21T02:11:04Z",
  "source": {
    "platform": "github",
    "instance_url": "https://github.com",
    "delivery_id": "7f3c...",
    "event_name": "pull_request.synchronize"
  },
  "kind": "pr.updated",
  "repository": {
    "id": "123456",
    "owner": "yteraoka",
    "name": "kibitz",
    "full_name": "yteraoka/kibitz",
    "clone_url": "https://github.com/yteraoka/kibitz.git",
    "default_branch": "main",
    "project": "",
    "visibility": "private"
  },
  "pull_request": {
    "id": "998877",
    "number": 42,
    "title": "Add SQS subscriber",
    "description": "...",
    "state": "open",
    "draft": false,
    "url": "https://github.com/yteraoka/kibitz/pull/42",
    "author": { "id": "1", "login": "yteraoka", "is_bot": false },
    "source": { "branch": "feat/sqs", "sha": "abc123", "repo_full_name": "yteraoka/kibitz" },
    "target": { "branch": "main", "sha": "def456", "repo_full_name": "yteraoka/kibitz" },
    "is_fork": false,
    "changed_files": 12
  },
  "comment": {
    "id": "555",
    "thread_id": "th_1",
    "in_reply_to": "",
    "body": "@kibitz なぜこの実装だと競合するのですか?",
    "author": { "id": "1", "login": "yteraoka", "is_bot": false },
    "path": "internal/queue/sqs/subscriber.go",
    "line": 88,
    "url": "https://github.com/.../#discussion_r555"
  },
  "command": { "name": "review", "args": ["--focus", "security"] },
  "actor": { "id": "1", "login": "yteraoka", "is_bot": false },
  "payload_ref": { "uri": "gs://kibitz-payloads/github/7f3c....json", "size": 184320, "sha256": "..." },
  "trace": { "traceparent": "00-...-01" }
}
```

`kind` の値 (これだけをワーカーは見る):

| kind | 意味 | ワーカーの動作 |
| --- | --- | --- |
| `pr.opened` | PR 作成 | 全体レビュー |
| `pr.updated` | 新しいコミットが push された | 前回レビュー済み SHA からの増分レビュー |
| `pr.ready_for_review` | draft 解除 | 全体レビュー |
| `pr.review_requested` | ボットにレビュー依頼 | 全体レビュー |
| `comment.created` | PR 上のコメント | メンション時のみ回答 (スレッド継続) |
| `command` | 明示コマンド (`@kibitz review` 等) | コマンドに応じた処理 |
| `pr.closed` / `pr.merged` | クローズ | セッションと作業領域の後片付け |
| `issue.comment` | Issue 上のメンション | 質問への回答。Phase 8 以降は実装指示も受け付ける |
| `issue.assigned` | ボットに Issue がアサインされた | Phase 8: 実装モードの起動条件 (既定は無効) |

`issue.*` は Phase 8 (実装モード) で使う。スキーマ上は `pull_request` を省略し、
`issue` オブジェクト (id / number / title / body / labels / assignees) を持つ。
GitHub は `issues` / `issue_comment`、GitLab は `issue` / `note`、
Azure DevOps は Work Item の `workitem.updated` / `workitem.commented` が対応する
(Azure DevOps は Git と Work Item が別サービスなので、リポジトリとの紐付けを
Work Item のリンクから解決する必要がある)。

### 設計上の決定

- **生 payload はイベントに埋め込まない。** 必要なときだけ `payload_ref` でオブジェクトストレージを参照する (Claim Check、[queue.md](queue.md))。これにより SQS の 256 KB 制限を回避でき、正規化イベント自体は数 KB に収まる。
- **`schema_version` を必ず持つ。** ワーカーは未知のメジャーバージョンを受けたら nack せず DLQ に送る (無限リトライを防ぐ)。
- **時刻は RFC3339 / UTC 固定**、ID は文字列 (GitLab は数値 ID、ADO は GUID のため)。
- **`actor.is_bot`** は自分自身のコメントを無視するために使う。GitHub App なら `type=Bot` とアプリのユーザー ID、GitLab / ADO は設定したボットアカウント ID と照合する。

## 3. トリガ判定 (`internal/policy`)

サーバーとワーカーの両方で同じ関数を使い、サーバーでは「publish するか」、
ワーカーでは「実際に実行するか」を判定する (リポジトリ設定はワーカーでしか読めないため二段階になる)。

```
publish する条件 (サーバー):
  - 既知の kind である
  - actor が自分自身 (bot) ではない
  - 全体設定の allow/deny リスト (org / repo) に合致する
  - comment.created の場合はメンションかコマンドを含む

実行する条件 (ワーカー、.kibitz.yaml 読み込み後):
  - review.enabled / answer.enabled
  - draft PR は skip (設定で変更可)
  - 変更ファイルがすべて paths_ignore に該当するなら skip
  - 同一 (PR, head_sha, kind) が処理済みなら skip
  - リポジトリの月次トークン予算を超えていないか
```

## 4. コマンド構文

PR コメント中の 1 行として解釈する。プラットフォーム共通。

```
@kibitz review                     # 全体を再レビュー
@kibitz review --focus security    # 観点を指定
@kibitz review --full              # 増分ではなく全体
@kibitz explain internal/queue/sqs/subscriber.go:88
@kibitz answer <質問>              # 明示的に質問 (メンションだけでも同義)
@kibitz ignore                     # この PR では以降レビューしない
@kibitz help

# Phase 8 (Issue 上で使用、既定は無効)
@kibitz implement                  # この Issue の内容を実装してブランチと PR を作る
@kibitz plan                       # 実装方針だけを提示する (コードは書かない)
```

メンション名は設定で変更可能にする (GitLab / ADO ではボットアカウント名が異なるため)。

## 5. テスト方針

`testdata/webhooks/{platform}/{event}.json` に実際の payload を保存し、
`Verify` と `Normalize` のゴールデンテストを行う。正規化結果も
`testdata/events/{platform}/{event}.golden.json` と比較する。
payload 中の組織名・ユーザー名・トークンはフィクスチャ作成時に匿名化する。

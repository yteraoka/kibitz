# 設計判断の記録 (ADR)

kibitz が**なぜそう作られているか**の記録。
1 つの決定につき 1 ファイルで、過去の判断は書き換えずに残す。
書き方と番号の規約は [ADR-0001](0001-record-architecture-decisions.md) にある。

現在の仕様は `docs/` の他のドキュメントが持つ。
ここにあるのは**決定と、その理由と、捨てた案**であって、仕様書ではない。

## 一覧

| # | 決定 | 状態 |
| --- | --- | --- |
| [0001](0001-record-architecture-decisions.md) | 設計上の決定を ADR として残す | 採用 |
| [0002](0002-gcp-first-single-tenant.md) | GCP を主軸にし、単一組織を前提に作る | 採用 |
| [0003](0003-opencode-as-agent-engine.md) | エージェントエンジンに OpenCode を採用する | 採用 |
| [0004](0004-platform-differences-live-in-two-places.md) | プラットフォーム差は正規化イベントと forge の 2 か所に閉じる | 採用 |
| [0005](0005-pubsub-with-per-pr-ordering-key.md) | キューは Cloud Pub/Sub、順序キーはプルリクエスト単位にする | 採用 |
| [0006](0006-claim-and-completion-are-different-facts.md) | 「始めた」と「終わった」を別の事実として記録する | 採用 |
| [0007](0007-github-app-only.md) | 認証は GitHub App のみにし、PAT の経路を捨てる | 採用 |
| [0008](0008-models-through-vertex-ai-without-api-keys.md) | モデルは Vertex AI 経由で使い、API キーを持たない | 採用 |
| [0009](0009-japanese-output-by-default.md) | レビューの出力は日本語を既定にする | 採用 |
| [0010](0010-data-and-instruction-positions.md) | **データの位置と指示の位置を分ける** | 採用 |
| [0011](0011-repository-settings-from-the-default-branch.md) | リポジトリ設定はデフォルトブランチから読み、一方向にだけ効かせる | 採用 |
| [0012](0012-narrow-the-conditions-that-wake-the-bot.md) | ボットが動く条件を狭くする | 採用 |
| [0013](0013-narrow-what-is-reviewed-and-report-the-cost.md) | レビュー範囲を絞り、かかった費用をレビュー本文に書く | 採用 |
| [0014](0014-worker-pool-and-self-managed-scaling.md) | worker は Cloud Run worker pool で動かし、台数は自分で決める | 採用 |
| [0015](0015-mcp-declared-in-three-layers-off-by-default.md) | MCP は三層で宣言し、既定では何も有効にしない | 採用 |
| [0016](0016-kibitz-mcp-holds-no-credential.md) | kibitz-mcp に資格情報を持たせない | 採用 |
| [0017](0017-index-decision-records-serve-bodies-as-tools.md) | 設計文書は索引をプロンプトに載せ、本文はツールで読ませる | 採用 |
| [0018](0018-measure-whether-the-index-is-used.md) | 索引が使われたかを計測し、ツール名はエンジンの呼び方のまま記録する | 採用 |

**[ADR-0010](0010-data-and-instruction-positions.md) が中核**で、
0011・0015・0016・0017 はいずれもその原則を各所に適用した結果になっている。

## kibitz 自身との関係

`docs/adr/**/*.md` は `worker.DefaultReferenceDocs` の先頭パターンなので、
**このフォルダの中身は、以降のレビューで索引としてプロンプトに載る**
(パスとタイトルのみ。本文は `search_docs` / `get_doc` で読まれる)。
つまり kibitz は、自分の設計判断に照らして自分のプルリクエストをレビューする。
詳細は [ADR-0017](0017-index-decision-records-serve-bodies-as-tools.md)。

## 新しく足すとき

1. 次の番号で `NNNN-英語の短い要約.md` を作る
2. 先頭の `# ` 見出しには**決定の内容**を書く (索引の 1 行になる)
3. この表に 1 行足す
4. 決定を覆す場合は、**元の ADR を書き換えず**に状態を `置き換え` にして新しい ADR を指す

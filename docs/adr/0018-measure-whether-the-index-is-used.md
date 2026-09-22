# 索引が使われたかを計測し、ツール名はエンジンの呼び方のまま記録する

- **状態**: 採用 (2026-09-22)
- **関連**: [ADR-0017](0017-index-decision-records-serve-bodies-as-tools.md) / `internal/reviewer/opencode/events.go`

## 背景

[ADR-0017](0017-index-decision-records-serve-bodies-as-tools.md) で、
設計文書の索引を**毎回のプロンプト**に載せ、
`search_docs` / `get_doc` を**毎回のリクエストのツール定義**として送るようにした。

どちらも払い続けるコストである。
そして払った分が何か買えているかは、**トークン数からは分からない**。
索引を 18 行載せたレビューと、載せずに走ったレビューの区別が付かない。

`internal/reviewer/opencode/events.go` にはツール呼び出しの数を数えるコードが
すでにあったが、**どこからも読まれていなかった**。数えて捨てていた。

## 決定

### 1. `tool_use` イベントを読み、3 つを記録する

- ツールごとの呼び出し回数と、そのうち失敗した数
- `get_doc` で**実際に読まれた文書のパス** (重複は 1 つに畳む)
- `search_docs` の回数

ログ (`docs_read` / `docs_searched` / `tool_calls` / `reference_docs`) と
メトリクス (`kibitz_reference_docs_consulted_total{action}`、
`kibitz_agent_tool_calls_total{tool,outcome}`) の両方に出す。

### 2. ツール名は**エンジンが報告したまま**記録する

正規化しない。`reviewer.ToolMatches` が**末尾一致**で kibitz のツールを判定する
(区切り文字が前に必要なので `forget_doc` は `get_doc` ではない)。

### 3. レビュー本文には書かない

## 理由

### なぜ「索引の件数」と「読まれた数」を同じログ行に並べるか

知りたいのは比である。「18 件出して 0 件読まれた」が 1 行で読めないと、
2 つのクエリを繋ぐ人しか気づかない。そして誰も繋がない。

### なぜツール名を正規化しないか

opencode は MCP サーバーのツールに**サーバー名と `_` を前置する**ので、
kibitz の `get_doc` は `kibitz_get_doc` として届く。
これはエンジンの都合であって、kibitz が知っているべき規則ではない。

ADR-0017 はプロンプトでツール名を**修飾なしで**書くと決めた。
「名前は言うが、アドレスは言わない」。**読む側も同じ立場を取る**のが筋で、
完全修飾名を焼き込めば、エンジンが名前空間の付け方を変えた瞬間に
「誰も ADR を読んでいない」という嘘のグラフが出る。

末尾一致は外すこともありうるが、**外す方向を選んである**。
取りこぼしは「この機能は使われていない」と読まれ、
それは**人が実際に行動する**読み方だからである。
逆向きの誤りはグラフが少し太るだけで済む。

### なぜレビュー本文に書かないか

これは運用者が機能を続けるか決めるための数字で、
PR を読む人が知りたいことではない。
レビュー本文はすでにトークン数と概算費用を載せている
([ADR-0013](0013-narrow-what-is-reviewed-and-report-the-cost.md))。
あれは**読む人の判断に効く**から載せてある。これは効かない。

### 検証について

ツール名の前置は推測ではない。OpenAI 互換のエンドポイントを自前で立てて
実機の opencode 1.18.31 を走らせ、**モデルに提示されたツール一覧**を記録した。

```
["bash","edit","glob","grep","kibitz_get_doc","kibitz_get_pr_diff",
 "kibitz_get_pr_metadata","kibitz_list_pr_comments","kibitz_list_pr_files",
 "kibitz_search_docs","read","skill","task","todowrite","webfetch","write"]
```

そのときの stdout をそのまま
`internal/reviewer/opencode/testdata/opencode-1.18.31-tool-use.jsonl` に置いてある。
**イベントの形はこの設計で唯一 kibitz が決められない部分**なので、
手で書いた想定ではなく実出力に対して回す。

## 影響

- `reviewer.Result` に `Tools` が増えた。エンジンが何も報告しなければゼロ値で、
  レビュー自体は変わらない。
- 同じ呼び出しが `running` で先に出ることがあるので、
  **終わった状態だけを数える**。でないと二重になる。
- 失敗した `get_doc` は**呼び出しとしては数えるが、読まれた文書には数えない**。
  索引外のパスを拒否した回数は、索引が働いている証拠であって、
  文書が参照された証拠ではない。
- `search_docs` に渡された**検索語は記録していない**。
  索引の語彙を調整する材料にはなるが、モデルが書いた自由文をログに流すことになる。
  必要になったら、そのときに決める。

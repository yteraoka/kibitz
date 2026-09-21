# エージェントエンジンの選定: OpenCode か pi か

`reviewer.Engine` の実装として何を使うかの比較と決定。

## 結論

**OpenCode を主軸にする。** 決め手は MCP サーバー対応が要件であること。
pi は設計思想として MCP を持たない (下記) ため、要件を満たすには TypeScript で
エクステンションを自作・保守する必要があり、Go 中心のこのプロジェクトに
別言語のコア部品を抱え込むことになる。

ただし `reviewer.Engine` インターフェースは pi でも実装できる粒度に保つ。
将来「外部連携は MCP ではなく CLI ツールで十分」と判断が変われば乗り換えられる。

## 比較

| 観点 | OpenCode | pi |
| --- | --- | --- |
| MCP | **ビルトイン。** `mcp` 設定で local (コマンド起動) / remote (URL) を宣言、`environment` で秘密情報を注入、`enabled` で個別に ON/OFF | **意図的に非対応。** 「CLI ツール + README (skills) を使え、必要ならエクステンションで自作せよ」という方針 |
| 権限制御 | `permission` で `allow`/`ask`/`deny` を宣言。`bash` はコマンドパターン単位、`edit` はパス単位、作業ディレクトリ外は `external_directory` で制御 | 権限機構なし (既定で全許可)。粒度はツール単位の allowlist (`--tools` / `--exclude-tools` / `--no-builtin-tools`)。隔離はコンテナや micro-VM で行う前提 |
| ヘッドレス実行 | `opencode run --format json`、`opencode serve` (HTTP API + JS SDK)、`run --attach` でサーバー再利用 | `pi -p` (print)、`pi --mode json` (JSONL イベント)、`pi --mode rpc` (stdin/stdout の JSONL RPC)、TS SDK |
| Go からの駆動 | 子プロセス + JSON イベント、または HTTP API | 子プロセス + JSONL。**RPC モードは双方向で、外部プロセス統合を明示的に想定した設計** |
| セッション継続 | `--session <id>` / `--continue` / `--fork`、HTTP なら同一セッションに POST | `--session <path\|id>` / `-c` / `--fork` / `--session-dir` / `--no-session` |
| サブエージェント | ビルトイン (`--agent`、エージェントごとの権限) | 非対応 (自作するか別インスタンスを起動) |
| 配布形態 | 単一バイナリ | npm パッケージ (Node.js ランタイムが必要) |
| 拡張方法 | 設定 + エージェント定義 + MCP + プラグイン | TypeScript エクステンション (ツールの置き換え、権限ゲート、UI まで何でも) |
| プロバイダ | 多数 (Anthropic 直、Bedrock、Vertex AI など) | 多数 (同上) |
| プロジェクトの性格 | 機能を内蔵する方向。利用実績が多い | コアを極小に保ち拡張で賄う方向。新しく、新規コントリビュータの issue/PR は既定で自動クローズする運用 |

## kibitz の要件に照らした評価

### 1. MCP (決定的)

要件は「worker が外部サービスや MCP サーバーを使えること」。Jira / Sentry / 社内検索のような
**他者が提供する MCP サーバー**をそのまま繋げられるかどうかが差になる。
OpenCode は設定だけで済む。pi では MCP クライアントをエクステンションとして実装し、
プロセス管理・再接続・ツール定義の変換まで自前で持つことになる。

なお、kibitz 自身のツール (`kibitz-mcp`: PR 差分取得、ファイル取得、既存コメント一覧など) は
pi 方式の「CLI ツール + skills」でも実現できる。MCP が本当に要るのは第三者製サーバーを使うときだけ、
という整理は覚えておく価値がある。

### 2. 権限と安全性

サーバー上で、人間の承認なしに、第三者が書ける入力 (PR 差分・コメント) を扱う。

- **レビュー用途 (読み取りのみ)**: pi の `--tools read,grep,find,ls` は
  `bash`/`write`/`edit` を完全に外せるので、実は非常に強い保証になる。
  OpenCode の `permission` は表現力が高い分、設定ミスの余地も大きい。ここは互角か、やや pi 有利。
- **将来の実装モード (編集を許す)**: 「`edit` はこのパスだけ」「`bash` は `go test`/`git` だけ」
  といった粒度が要る。OpenCode はこれを宣言的に書ける。pi ではエクステンションを書くことになる。
- **ヘッドレス特有の落とし穴**: OpenCode は `"ask"` が残っていると承認待ちでハングする
  (対策: 全権限を `allow`/`deny` で決め切る + `--auto`)。pi は承認機構自体がないので固まらない代わりに、
  ツールを渡したら無条件に実行される。

どちらにせよコンテナ隔離と egress 制限は必須で、それが第一の防御線である点は変わらない。

### 3. Go ワーカーからの統合

pi の RPC モード (LF 区切り JSONL、双方向) は、非 Node 言語からの統合を明示的に想定しており、
Go から扱うには素直。OpenCode は「毎回プロセス起動 (`run`)」か「HTTP サーバーを立てて叩く (`serve`)」の
二択で、前者は MCP のコールドスタート、後者はサイドカー管理という手間がある。
ここは pi 有利だが、[worker.md](worker.md) の通り `run` / `attach` の二実装を用意すれば実用上の差は小さい。

### 4. 運用

OpenCode は単一バイナリなので distroless 寄りのイメージに置きやすい。
pi は Node.js が必要。ただし MCP サーバーを `npx`/`bunx` で動かすなら結局 Node は要るので、
OpenCode を選んでも Node 依存は消えない。

### 5. プロジェクトリスク

pi は「コアを小さく保ち、欲しい機能は自分で書く」という明確な思想で、
新規コントリビュータからの issue/PR を既定で自動クローズする運用を取っている。
自分たちで深く作り込む覚悟があるなら軽量で読みやすい土台になるが、
上流に機能追加を期待する使い方には向かない。
OpenCode は機能を内蔵する方向で、MCP・権限・エージェントのように
kibitz が必要とするものが既に揃っている。

## pi に切り替えるべきケース

次のいずれかに該当するなら、pi のほうが適する。

- 外部サービス連携を第三者製 MCP ではなく、自前の CLI ツール + skills で賄うと決めた場合
  (要件が「MCP 対応」ではなく「外部サービス連携」だったのなら、この道はあり得る)
- エージェントループそのものに手を入れたい (独自の compaction、独自のツール実行経路、
  micro-VM へのツールルーティングなど) 場合
- 依存を最小化し、エンジンの挙動をすべて自分たちの管理下に置きたい場合

## 抽象化の契約

どちらに乗り換えても `reviewer.Engine` の外側は変えなくて済むよう、
エンジンに要求する能力を次の 6 つに限定する。

| 要求 | OpenCode | pi |
| --- | --- | --- |
| 作業ディレクトリの指定 | `--dir` | プロセスの cwd |
| ツール / 権限の制限 | `permission` 設定 + `--auto` | `--tools` / `--exclude-tools` / `--no-builtin-tools` |
| システムプロンプトの差し替え | エージェント定義 (`--agent`) | プロンプトテンプレート / `--system` 相当 |
| セッションの継続 | `--session <id>` | `--session <path\|id>` |
| 構造化出力 | ファイル出力 (`.kibitz/out/review.json`) | 同左 |
| 実行イベントの取得 | `--format json` | `--mode json` / `--mode rpc` |

外部サービス連携だけは抽象化しきれない (MCP かカスタムツールか)。
そのため `kibitz-mcp` は **MCP サーバーとしても、単体 CLI としても動く**ように実装する。
こうしておけば pi に切り替えても自前ツールはそのまま使い回せる。

## 参照

- OpenCode: [CLI](https://opencode.ai/docs/cli/) / [MCP servers](https://opencode.ai/docs/mcp-servers/) / [Permissions](https://opencode.ai/docs/permissions/) / [Server](https://opencode.ai/docs/server/)
- pi: [earendil-works/pi](https://github.com/earendil-works/pi) / [coding-agent README](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md) / [containerization](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/containerization.md)

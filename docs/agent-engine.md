# エージェントエンジンの選定: OpenCode か pi か

`reviewer.Engine` の実装として何を使うかの比較と決定。

## 結論

**OpenCode を主軸にする。** 決定の経緯は [ADR-0003](adr/0003-opencode-as-agent-engine.md)、
pi 1.0 を受けた再評価は [ADR-0024](adr/0024-keep-opencode-after-pi-gained-mcp.md) にある。

当初の決め手は「pi は MCP を持たない」ことだったが、pi は 1.0 (2026-10-01) で
MCP を組み込みで持つようになり、この差は無くなった。
それでも OpenCode を続けるのは、次の 3 つの理由による。

- 乗り換えの費用 (`internal/reviewer/opencode` と、ADR-0021 などで実機確認した挙動のやり直し)
- pi の MCP 実装が出たばかりであること
- pi の上流の方向性への懸念

pi は `reviewer.Engine` の裏に試作エンジンとして足し、
[下の未検証の点](#pi-に切り替える前に確かめること)を実機で確かめてから改めて判断する。

## 比較

pi は 1.0、OpenCode は 1.18.31 (worker のイメージに入っている版) 時点。

| 観点 | OpenCode | pi |
| --- | --- | --- |
| MCP | **ビルトイン。** `mcp` 設定で local (コマンド起動) / remote (URL) を宣言、`environment` で秘密情報を注入、`enabled` で個別に ON/OFF | **ビルトイン (1.0 から)。** `mcp.json` の `mcpServers` で stdio / streamable HTTP を宣言 (SSE は非対応)、`env`/`headers` の `${VAR}` で秘密情報を注入、`enabled` で個別に ON/OFF、`toolExposure` でツール単位に `hidden` |
| MCP ツールの見せ方 | すべてのツールをモデルに直接宣言 | 既定は **codemode** (モデルが書く JavaScript から呼ぶ)。`direct` / `deferred` (`tool_search` で後から宣言) / `hidden` を選べる |
| 権限制御 | `permission` で `allow`/`ask`/`deny` を宣言。`bash` はコマンドパターン単位、`edit` はパス単位、作業ディレクトリ外は `external_directory` で制御 | 権限機構なし (既定で全許可)。粒度はツール単位の allowlist (`--tools` / `--exclude-tools` / `--no-builtin-tools`)。それより細かい制御はエクステンションの `tool_call` フック。隔離はコンテナや micro-VM で行う前提 |
| リポジトリ側の設定 | kibitz がジョブごとに生成し、リポジトリからは読まない (ADR-0010) | `.pi/` 以下 (設定・MCP・エクステンション) は project trust を与えたときだけ読む。ヘッドレスでは `--no-approve` で拒否。`AGENTS.md` / `CLAUDE.md` は trust に関係なく読まれる |
| ヘッドレス実行 | `opencode run --format json`、`opencode serve` (HTTP API + JS SDK)、`run --attach` でサーバー再利用 | `pi -p` (print)、`pi --mode json` (JSONL イベント)、`pi --mode rpc` (stdin/stdout の JSONL RPC)、TS SDK |
| Go からの駆動 | 子プロセス + JSON イベント、または HTTP API | 子プロセス + JSONL。**RPC モードは双方向で、外部プロセス統合を明示的に想定した設計** |
| セッション継続 | `--session <id>` / `--continue` / `--fork`、HTTP なら同一セッションに POST | `--session <path\|id>` / `--session-id` / `-c` / `--fork` / `--session-dir` / `--no-session` |
| サブエージェント | ビルトイン (`--agent`、エージェントごとの権限) | 非対応 (自作するか別インスタンスを起動) |
| 配布形態 | 単一バイナリ | npm パッケージ (Node.js 22.19 以上) またはインストーラ |
| 拡張方法 | 設定 + エージェント定義 + MCP + プラグイン | TypeScript エクステンション (ツールの置き換え、権限ゲート、UI まで何でも) + MCP |
| プロバイダ | 多数 (Anthropic 直、Bedrock、Vertex AI など) | 多数 (同上)。Vertex AI は API キーまたは ADC |
| 自身の外部通信 | モデル一覧の更新 (`models.opencode.ai`) など | モデルカタログの更新、インストール時のテレメトリとプロバイダ帰属ヘッダー (**既定で有効**、`PI_TELEMETRY=0` / `PI_OFFLINE=1` で止める) |
| プロジェクトの性格 | 機能を内蔵する方向。利用実績が多い | コアを極小に保ち拡張で賄う方向。2026-04 に Earendil が買収し、自社ゲートウェイ (Radius) への導線がある。新規コントリビュータの issue/PR は既定で自動クローズ |

## kibitz の要件に照らした評価

### 1. MCP (互角)

要件は「worker が外部サービスや MCP サーバーを使えること」。Jira / Sentry / 社内検索のような
**他者が提供する MCP サーバー**をそのまま繋げられるかどうか。

pi 1.0 以降は**どちらも設定だけで済む**。ADR-0015 の三層の宣言は、
config をジョブごとに書き出してプレースホルダで資格情報を注入するという形なので、
どちらのエンジンでも表現できる (OpenCode の `{env:NAME}` は pi では `${NAME}`)。

違いは MCP ツールの見せ方にある。pi は既定で codemode 経由にし、
ツール定義をプロンプトに載せずに済ませる。トークンは節約できるが、
ツール呼び出しが codemode の中で入れ子になる。kibitz で使うなら、
評価が固まるまでは `exposure: "direct"` にして OpenCode と同じ見え方に揃えるのが無難。

### 2. 権限と安全性

サーバー上で、人間の承認なしに、第三者が書ける入力 (PR 差分・コメント) を扱う。

- **レビュー用途 (読み取りのみ)**: pi の `--tools read,grep,find,ls` は
  `bash`/`write`/`edit` を外せる。OpenCode の `permission` は表現力が高い分、設定ミスの余地も大きい。
  ただし pi 1.0 では、codemode 公開の MCP サーバーが繋がると **codemode が自動で有効になり**、
  codemode からの呼び出しは**有効なツールの組み合わせに依存しない**とドキュメントにある。
  MCP と併用したときにもこの保証が保たれるかは未検証で、確かめるまで pi の利点とは言えない。
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

pi を使うなら、pi 自身の外部通信を止める必要がある (`PI_TELEMETRY=0`、`PI_OFFLINE=1`)。
また Node は `NODE_USE_ENV_PROXY=1` が無いとプロキシの変数を無視する (ADR-0021)。
pi 本体の通信が ADR-0021 のプロキシを通るかは実機で確かめる必要がある。

### 5. 乗り換えの費用

`internal/reviewer/opencode` (テスト込みで約 3,700 行) は、次のものを持つ。

- 失敗の診断 (`diagnose.go`)
- モデルカタログ (`catalog.go`)
- イベント解析 (`events.go`)
- プロセス管理

ADR-0018 の計測と ADR-0021 のプロキシも、OpenCode の実機の挙動を前提に確かめてある。
乗り換えではこれらをやり直すことになる。

### 6. プロジェクトリスク

pi は「コアを小さく保ち、欲しい機能は自分で書く」という明確な思想で、
新規コントリビュータからの issue/PR を既定で自動クローズする運用を取っている。
MCP 対応はこの思想を曲げた変更で、買収後の方針転換と報じられている。
自前の Radius ゲートウェイへの導線もあり、今後どちらへ進むかは読みにくい。
自分たちで深く作り込む覚悟があるなら軽量で読みやすい土台になるが、
上流に機能追加を期待する使い方には向かない。
OpenCode は機能を内蔵する方向で、MCP・権限・エージェントのように
kibitz が必要とするものが既に揃っている。

## pi に切り替える前に確かめること

[ADR-0024](adr/0024-keep-opencode-after-pi-gained-mcp.md) の未検証の点。試作エンジンで確かめる。

1. `--tools read,grep,find,ls` と MCP を併用したとき、codemode や `tool_search` から
   許可していないツールに届かないか。`autoEnableCodemode: false` と `exposure: "direct"` で閉じられるか
2. codemode 経由の入れ子の呼び出し (`parentToolCallId`) を、ADR-0018 の計測と
   イベント解析がどう数えるべきか
3. ADR-0021 のプロキシに、pi 本体 (Node) が `NODE_USE_ENV_PROXY=1` 無しで従うか。
   pi 自身の通信が `PI_OFFLINE=1` で止まるか
4. Vertex AI で、ADC による Gemini に加えて Claude と Model Garden の第三者モデル
   (`vertex-maas/...`、ADR-0008) が使えるか
5. `~/.pi/agent` (設定・認証・trust・セッション) をジョブごとの使い捨てディレクトリに隔離できるか

## pi のほうが適するケース

上の点が確かめられたうえで、次のいずれかに該当するなら pi のほうが適する。

- エージェントループそのものに手を入れたい (独自の compaction、独自のツール実行経路、
  micro-VM へのツールルーティングなど) 場合
- 読み取り専用の保証を、権限設定ではなく「ツールを渡さない」形で持ちたい場合
- Go からの統合を、毎回のプロセス起動やサイドカーではなく常駐の RPC で行いたい場合

「第三者製 MCP サーバーを使うかどうか」は、pi 1.0 以降は判断の分かれ目にならない。

## 抽象化の契約

どちらに乗り換えても `reviewer.Engine` の外側は変えなくて済むよう、
エンジンに要求する能力を次の 6 つに限定する。

| 要求 | OpenCode | pi |
| --- | --- | --- |
| 作業ディレクトリの指定 | `--dir` | プロセスの cwd |
| ツール / 権限の制限 | `permission` 設定 + `--auto` | `--tools` / `--exclude-tools` / `--no-builtin-tools` (+ `autoEnableCodemode: false`) |
| システムプロンプトの差し替え | エージェント定義 (`--agent`) | `--system-prompt` / `--append-system-prompt` |
| セッションの継続 | `--session <id>` | `--session <path\|id>` / `--session-id` |
| 構造化出力 | ファイル出力 (`.kibitz/out/review.json`) | 同左 |
| 実行イベントの取得 | `--format json` | `--mode json` / `--mode rpc` |

MCP はどちらも設定ファイルで宣言できる。
それでも `kibitz-mcp` は **MCP サーバーとしても、単体 CLI としても動く**ように実装してある。
こうしておけば、MCP をどう見せるかがエンジンごとに違っても、自前ツールはそのまま使い回せる。

## 参照

- OpenCode: [CLI](https://opencode.ai/docs/cli/) / [MCP servers](https://opencode.ai/docs/mcp-servers/) / [Permissions](https://opencode.ai/docs/permissions/) / [Server](https://opencode.ai/docs/server/)
- pi: [earendil-works/pi](https://github.com/earendil-works/pi) / [v1.0.0 リリースノート](https://github.com/earendil-works/pi/releases/tag/v1.0.0) / [coding-agent README](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/README.md)
- pi のドキュメント: [MCP](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/mcp.md) / [codemode](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/codemode.md) / [CLI](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/cli.md) / [security](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/security.md) / [containerization](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/containerization.md)

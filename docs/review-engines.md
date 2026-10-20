# エンジンごとのレビュー処理

レビューのとき、3 つのエージェントエンジン (OpenCode / pi / ADK) がそれぞれ中で何をしているか。
エンジンを選んだ理由と比較は [agent-engine.md](agent-engine.md)、
Webhook から投稿までの全体の流れは [review-flow.md](review-flow.md) にある。
この文書が扱うのは、worker が `reviewer.Engine.Run` を呼んでから戻るまでの間。

> 図は Mermaid で書いてある。GitHub 上ではそのまま図として表示される。

## 1. エンジンの位置

worker はエンジンの違いを知らない。PR の取得、チェックアウト、トリアージ、出力の検証、投稿は
すべて worker が行い、エンジンに頼むのは「プロンプトを渡して、出力ファイルを書かせる」ことだけ。

```mermaid
flowchart LR
    subgraph W["kibitz-worker: ReviewJob.review"]
        direction TB
        A["PR / diff / 設定を取得"] --> B["shallow clone"]
        B --> C["トリアージ<br/>大きすぎる変更のみ"]
        C --> D["reviewer.Request を組み立てる"]
        D --> E["runWithRetry"]
        E --> F["Sanitize<br/>行番号・重複・件数"]
        F --> G["サマリ + インライン指摘を投稿"]
    end

    E -- "Engine.Run(req)" --> X{"どのエンジンか"}
    X --> O["opencode.Runner<br/>子プロセス"]
    X --> P["pi.Runner<br/>子プロセス"]
    X --> K["adk.Runner<br/>worker 内"]
    O & P & K -- "reviewer.Result" --> E
```

### どのエンジンで動くか

```mermaid
flowchart TD
    S["レビュー開始"] --> Q{"/kibitz review --engine X<br/>で指定されたか"}
    Q -- "いいえ" --> D["デプロイの既定<br/>KIBITZ_AGENT_ENGINE"]
    Q -- "はい" --> L{"X は許可された<br/>エンジンか"}
    L -- "いいえ" --> N["既定のエンジンで実行<br/>サマリに理由を書く"]
    L -- "はい" --> M{"X はこのモデルを<br/>実行できるか<br/>ModelChecker.CanRun"}
    M -- "いいえ" --> N
    M -- "はい" --> U["X で実行<br/>サマリにエンジン名を書く"]
```

- 既定は `KIBITZ_AGENT_ENGINE` (`opencode` / `pi` / `adk`)。
- コメントで切り替えられるのは、デプロイが許可したエンジンだけ。
  切り替えたエンジンで動いたレビューのセッションは保存しない。続く質問 (answer) は既定のエンジンで動き、
  別のエンジンのセッションは開けないため。
- `CanRun` を持つのは ADK だけ (Gemini 以外を拒否)。
- トリアージも同じエンジンで動く。ただしコマンドで ADK を指定し、トリアージ用のモデルが Gemini でない場合は、
  トリアージだけ既定のエンジンで動く (`ReviewJob.triageEngine`)。

## 2. 3 エンジンに共通する手順

3 つの `Run` は、骨格が同じになるように書いてある。違うのは「エージェントをどう動かすか」と
「権限をどう作るか」の部分だけ。

```mermaid
flowchart TD
    A["ジョブディレクトリを作る<br/>os.MkdirTemp、終了時に削除"] --> B{"モードが triage か"}
    B -- "いいえ" --> C["jobcontext を書き出す<br/>diff・PR メタデータ・既存コメント・参照文書"]
    B -- "はい" --> E
    C --> D["MCP サーバーを選ぶ<br/>serversFor"]
    D --> E["エンジン固有の準備<br/>設定ファイル / 権限 / ツール"]
    E --> F["egress セッションを開始<br/>設定されている場合"]
    F --> G["プロンプトを組み立てる<br/>reviewer.BuildPrompt"]
    G --> H["エージェントを実行"]
    H --> I["実行イベントを集計<br/>トークン数・ツール呼び出し"]
    I --> J{"モード"}
    J -- "review" --> K[".kibitz/out/review.json を読む<br/>ParseOutput"]
    J -- "triage" --> T[".kibitz/out/triage.json を読む<br/>ParseTriage"]
    J -- "answer / plan" --> R["最後の応答テキストを Reply に"]
    K & T & R --> Z["reviewer.Result を返す"]
```

### MCP サーバーの選び方 (`serversFor`)

3 エンジンとも同じ規則で選ぶ。

1. トリアージには何も渡さない (ファイル名の一覧だけで判断させるため)。
2. リポジトリの `.kibitz.yaml` が名前を挙げたサーバーのうち、デプロイのカタログにあるものだけ。
3. fork からの PR では、`allow_fork: true` のサーバーだけ。
4. jobcontext があれば、kibitz 自身の `kibitz-mcp` (`kibitz` という名前) を必ず足す。
   `get_pr_diff` / `list_pr_files` / `list_pr_comments` / `get_pr_metadata` / `search_docs` / `get_doc` を持ち、
   トリアージで外したファイルの diff もここから読める。

### 出力の契約と、失敗したときの再試行

review の結果は**ファイル** (`.kibitz/out/review.json`) で受け取る。標準出力のテキストは読まない。
JSON として読めない・スキーマに合わない場合、エンジンは `reviewer.OutputError` を返す。
worker はそのときだけ 1 回やり直す。

```mermaid
sequenceDiagram
    autonumber
    participant J as ReviewJob
    participant E as Engine
    J->>E: Run req
    alt 成功
        E-->>J: Result
    else OutputError: 出力がスキーマに合わない
        E-->>J: OutputError
        J->>J: req.Feedback にエラー文を入れ、SessionID を空にする
        J->>E: Run req、新しいセッションで 1 回だけ
        E-->>J: Result または error
    else それ以外のエラー
        E-->>J: error
        Note over J: ジョブを失敗させ、キューの再配送に任せる
    end
```

戻った `Result` は、どのエンジンのものでも `reviewer.Sanitize` が同じように検証する
(diff の外の行・重大度の閾値・重複・件数の上限)。

## 3. OpenCode (`KIBITZ_AGENT_ENGINE=opencode`、既定)

実装は `internal/reviewer/opencode`。**`opencode run` を 1 ジョブにつき 1 プロセス起動する。**
権限は OpenCode 自身の `permission` 設定で作り、その設定はジョブごとに kibitz が生成する。

### プロセスの構成

```mermaid
flowchart LR
    subgraph Worker["kibitz-worker プロセス"]
        R["opencode.Runner"]
    end
    subgraph Job["ジョブディレクトリ"]
        CFG["opencode.json<br/>model / permission / mcp"]
        CTX["context.json"]
    end
    subgraph WS["ワークスペース = PR のチェックアウト"]
        PR[".kibitz/prompt.md"]
        OUT[".kibitz/out/review.json"]
    end

    R -- "書く" --> CFG & CTX & PR
    R -- "exec: opencode run --standalone<br/>--format json --agent kibitz-review --auto" --> OC["opencode プロセス<br/>cwd = ワークスペース"]
    OC -- "OPENCODE_CONFIG" --> CFG
    OC -- "--file" --> PR
    OC -- "read / grep / glob" --> WS
    OC -- "edit: .kibitz/out/* のみ" --> OUT
    OC -- "stdio / remote" --> MCP["kibitz-mcp<br/>+ リポジトリが有効にした MCP"]
    MCP --> CTX
    OC -- "HTTPS_PROXY" --> EG["egress プロキシ"]
    MCP -- "HTTPS_PROXY" --> EG
    EG --> LLM["モデル API<br/>Vertex AI など"]
    OC -- "JSON イベント: stdout" --> R
```

### 実行の流れ

```mermaid
sequenceDiagram
    autonumber
    participant R as opencode.Runner
    participant FS as ジョブdir / ワークスペース
    participant OC as opencode run
    participant M as モデル
    participant T as MCP サーバー

    R->>FS: context.json を書く
    R->>FS: opencode.json を書く、permission と mcp
    R->>R: 子プロセスの環境変数を許可リストから組み立てる
    R->>FS: .kibitz/prompt.md を書く
    R->>OC: exec、OPENCODE_DISABLE_PROJECT_CONFIG=1
    loop エージェントループ
        OC->>M: プロンプト + ツール定義
        M-->>OC: ツール呼び出し
        alt read / grep / glob
            OC->>FS: ワークスペースを読む
        else MCP ツール
            OC->>T: get_pr_diff など
        else edit
            OC->>FS: .kibitz/out/review.json を書く
        end
        OC-->>R: step_start / tool_use / step_finish を stdout に
    end
    OC-->>R: 終了
    R->>R: イベントを集計 parseEvents
    opt step_finish が欠けた step がある
        R->>OC: opencode session export
        OC-->>R: その step のトークン数
    end
    R->>FS: review.json を読む → ParseOutput
```

### 権限 (`opencode.json` の `permission`)

ジョブごとに生成し、リポジトリ側の設定は読まない (ADR-0010)。

| ツール | review | answer / plan | triage |
| --- | --- | --- | --- |
| `*` (下に無いもの全部) | deny | deny | deny |
| `read` / `grep` / `glob` | allow | allow | allow |
| `edit` | `.kibitz/out/*` だけ allow | deny | `.kibitz/out/*` だけ allow |
| `bash` / `webfetch` / `external_directory` | deny | deny | deny |
| MCP サーバーのツール | サーバー名で allow | サーバー名で allow | (サーバー自体を渡さない) |

- **シェルはどのモードにも無い。** `bash` のルールはコマンドライン文字列への一致なので、
  1 つのプログラムを許すとそのオプションを全部許すことになるため。
- `--auto` は必須。`ask` が 1 つでも残ると、ヘッドレスでは承認待ちのまま止まる。
- MCP は OpenCode 2 の形式で書き、サーバーごとに `codemode: false` を付ける。
  付けないと 2.0 の既定の Code Mode になり、`"*": "deny"` で `execute` が拒否されて、ツールが 1 つも使えなくなる。

### チェックアウトから何も読ませない

PR のチェックアウトにある `AGENTS.md`、`.opencode/`、`.claude/`、`.agents/` を読ませないよう、
`OPENCODE_DISABLE_PROJECT_CONFIG=1` を付けて起動する。読まれると、ブランチ側がレビュアーへの指示を書き換えられてしまう。
リポジトリの規約は worker がデフォルトブランチから読み、プロンプトの `Guidelines` として渡す。

### 途中で止まったときの扱い

OpenCode には、ほかの 2 つに無いやり直しの経路が 2 つある。

```mermaid
flowchart TD
    A["exec"] --> B{"失敗したか"}
    B -- "いいえ" --> Z["イベントを集計して出力を読む"]
    B -- "session not found" --> C["SessionID を外して再実行<br/>コンテナが入れ替わればセッションは消える"]
    C --> B2{"失敗したか"}
    B2 -- "いいえ" --> Z
    B2 -- "はい" --> D
    B -- "その他" --> D{"モデルのターンで打ち切られたか<br/>EndedOnModelTurn"}
    D -- "いいえ" --> F["RunError を返す<br/>diagnose の要約付き"]
    D -- "はい、残り回数あり" --> E["同じセッションを再開<br/>「続けて最後までやり遂げて」<br/>最大 2 回"]
    E --> B2
```

- **モデルのターンでの打ち切り**: Gemini が `OTHER` や `UNEXPECTED_TOOL_CALL` などの終了理由を返すと、
  OpenCode はモデル自身の発言で終わる会話をもう一度送ってしまい、Vertex AI に拒否される。
  これを `diagnose.go` で見分け、セッションを再開する。打ち切られた回のトークンも合算する。
- **トークン数の取りこぼし**: OpenCode 2 の `run --format json` は最後の `step_finish` を出さないことがある。
  そのため `opencode session export` で該当する step だけを読み直す (`recoverUsage`)。
- **失敗の診断**: stdout のイベントと stderr のログから、失敗の原因を 1 行にまとめる (`diagnose.go`)。
  その 1 行が失敗通知のコメントに出る。

## 4. pi (`KIBITZ_AGENT_ENGINE=pi`、試作)

実装は `internal/reviewer/pi`。OpenCode と同じく **`pi` を 1 ジョブにつき 1 プロセス起動する。**
違いは権限の作り方にある。pi には権限機構が無いので、次の 2 段で作る。

1. **そもそも渡さない**: settings.json の `defaultTools` で、モードに必要なツールだけを有効にする。
2. **呼び出しごとに検査する**: ガード拡張 `deploy/pi/kibitz-guard.ts` が、`tool_call` のたびにツール名・パス・書き込み先を確かめる。

### プロセスの構成

```mermaid
flowchart LR
    subgraph Worker["kibitz-worker プロセス"]
        R["pi.Runner"]
    end
    subgraph Job["ジョブディレクトリ"]
        AG["agent/<br/>settings.json / mcp.json / models.json"]
        SYS["system.md<br/>エージェント定義の本文"]
        CTX["context.json"]
    end
    SD["セッションディレクトリ<br/>ジョブの外、コンテナ内"]
    subgraph WS["ワークスペース"]
        PR[".kibitz/prompt.md"]
        OUT[".kibitz/out/review.json"]
    end

    R -- "書く" --> AG & SYS & CTX & PR
    R -- "exec: pi --mode json --no-approve<br/>--extension kibitz-guard.ts" --> PI["pi プロセス<br/>cwd = ワークスペース"]
    PI -- "PI_CODING_AGENT_DIR" --> AG
    PI -- "--append-system-prompt" --> SYS
    PI -- "--session-dir" --> SD
    PI --> G["kibitz-guard<br/>tool_call フック"]
    G -- "許可されたもの" --> TOOLS["read / grep / find / ls / write"]
    TOOLS --> WS
    G -- "mcp__kibitz__ など" --> MCP["MCP サーバー"]
    MCP --> CTX
    PI -- "HTTPS_PROXY" --> EG["egress プロキシ"]
    EG --> LLM["モデル API"]
    PI -- "JSONL イベント: stdout" --> R
```

### 実行の流れ

```mermaid
sequenceDiagram
    autonumber
    participant R as pi.Runner
    participant FS as ジョブdir / ワークスペース
    participant PI as pi
    participant G as kibitz-guard
    participant M as モデル

    R->>R: ガード拡張が設定されているか確認、無ければ失敗
    R->>FS: context.json を書く
    R->>R: profileFor でモードのツールと書き込み可能パスを決める
    R->>FS: agent/settings.json、mcp.json、models.json を書く
    R->>R: ガードの規則を JSON にして KIBITZ_PI_GUARD に入れる
    R->>FS: .kibitz/prompt.md と system.md を書く
    R->>R: セッションを探す、無ければ新しい ID
    R->>PI: exec、PI_OFFLINE=1 PI_TELEMETRY=0
    loop エージェントループ
        PI->>M: プロンプト + ツール定義
        M-->>PI: ツール呼び出し
        PI->>G: tool_call
        alt 規則に合う
            G-->>PI: 通す
            PI->>FS: 実行
        else 規則に合わない
            G-->>PI: block + 理由
            PI->>M: 拒否された旨を返す
        end
        PI-->>R: JSONL イベントを stdout に
    end
    PI-->>R: 終了コード 0
    R->>R: 最後の assistant の stopReason を見る
    alt error / aborted
        R-->>R: エラーを返す
    else それ以外
        R->>FS: review.json を読む → ParseOutput
    end
```

### モードごとのツール (`profileFor`)

| モード | 有効にするツール | 書き込める場所 |
| --- | --- | --- |
| review / triage | `read` `grep` `find` `ls` `write` | `^\.kibitz/out/[^/]+$` |
| answer / plan | `read` `grep` `find` `ls` | なし |
| implement | 上の 4 つ + `edit` `write` | リポジトリが許したパス (`repoconfig.PathFilter` の正規表現) |

MCP のツールは `mcp__<サーバー名>__` という接頭辞で許可する。
ツールを絞るのに `--tools` を使わないのは、`--tools` だと MCP のツールまで落ちてしまうから。

### ガード拡張が確かめること

```mermaid
flowchart TD
    A["tool_call"] --> B{"KIBITZ_PI_GUARD を<br/>読めたか"}
    B -- "いいえ" --> X["block: すべて拒否<br/>fail closed"]
    B -- "はい" --> C{"ツール名が許可リスト<br/>または MCP 接頭辞に一致"}
    C -- "いいえ" --> X2["block: このツールは使えない"]
    C -- "はい" --> D{"パス引数を持つツールか"}
    D -- "いいえ" --> OK["通す"]
    D -- "はい" --> E["pi と同じ解決をする<br/>@ の除去、~ の展開、相対パス"]
    E --> F["シンボリックリンクを解決<br/>存在する部分まで realpath"]
    F --> G{"チェックアウトの中か"}
    G -- "いいえ" --> X3["block: リポジトリの外"]
    G -- "はい" --> H{"glob 引数が .. / 絶対パス / ~ を<br/>含まないか"}
    H -- "含む" --> X4["block"]
    H -- "含まない" --> I{"write / edit か"}
    I -- "いいえ" --> OK
    I -- "はい" --> J{"writable の正規表現に一致"}
    J -- "いいえ" --> X5["block: 書き込めない"]
    J -- "はい" --> OK
```

### チェックアウトから何も読ませない

`--no-approve` (project trust を与えない)、`--no-context-files` (`AGENTS.md` / `CLAUDE.md`)、
`--no-skills`、`--no-prompt-templates` を付ける。`~/.pi/agent` に当たるものはジョブディレクトリに作り、
`PI_CODING_AGENT_DIR` でそこを指す。そのため、運用者の設定も前のジョブの設定も効かない。

### セッション

pi は ID によるセッション検索を起動ディレクトリ単位で行う。毎回新しいチェックアウトで起動するので、ID では見つからない。
そこでセッションファイルをジョブの外の `--session-dir` に置き、続きのときは `*_<id>.jsonl` を探して
`--session <file>` で開く。新しいときは `kibitz-<乱数>` を `--session-id` で渡す。

### OpenCode との違い

- プロバイダのエラーでも pi は終了コード 0 で終わる。そのため最後の assistant メッセージの `stopReason` で失敗を判定する。
- 応答が打ち切られたときの再開と、`diagnose.go` に当たる詳しい診断はまだ無い。

## 5. ADK for Go (`KIBITZ_AGENT_ENGINE=adk`、試作)

実装は `internal/reviewer/adk`。**子プロセスを起動しない。** エージェントループ、ツール、モデルのクライアントがすべて
worker のプロセスの中で動く。そのため安全性の根拠も変わる。権限設定で「禁止」するのではなく、
「kibitz が書いたツールしか存在しない」ことと `os.Root` の閉じ込めが根拠になる。

### プロセスの構成

```mermaid
flowchart LR
    subgraph Worker["kibitz-worker プロセス"]
        R["adk.Runner"]
        subgraph Agent["llmagent + runner"]
            LOOP["エージェントループ"]
            CB["BeforeModelCallback<br/>モデル呼び出し回数の上限"]
        end
        subgraph Tools["kibitz のツール: os.Root で閉じ込め"]
            T1["read_file / list_dir<br/>glob_files / grep_files"]
            T2["write_file<br/>review / triage のみ"]
        end
        SS["InMemory セッション<br/>最大 200 件"]
        GEM["Gemini クライアント<br/>Vertex AI、ADC"]
        MT["mcptoolset<br/>uniqueToolset で名前の衝突を除く"]
    end
    WS["ワークスペース"]
    MCP["MCP サーバー: 子プロセス<br/>kibitz-mcp など"]
    EG["egress プロキシ"]
    V["Vertex AI"]

    R --> LOOP
    LOOP --> CB
    LOOP --> T1 & T2
    T1 & T2 --> WS
    LOOP --> MT
    MT -- "stdio / streamable HTTP" --> MCP
    MCP -- "HTTPS_PROXY" --> EG
    LOOP --> SS
    LOOP --> GEM
    GEM -- "直接、プロキシを通さない" --> V
```

### 実行の流れ

```mermaid
sequenceDiagram
    autonumber
    participant R as adk.Runner
    participant A as llmagent / runner
    participant T as kibitz のツール
    participant MCP as MCP サーバー
    participant G as Gemini

    R->>R: implement なら拒否、Gemini 以外のモデルも拒否
    R->>R: Gemini クライアントを作る
    R->>R: context.json を書く
    R->>T: os.OpenRoot でワークスペースを開く
    R->>MCP: MCP サーバーを起動・接続、egress セッション経由
    R->>R: エージェント定義の本文を読む → InstructionProvider
    R->>T: 前の試行の review.json を消す
    R->>A: llmagent.New + runner.New
    R->>A: Run、プロンプトはメッセージ本文として直接渡す
    loop エージェントループ
        A->>A: BeforeModelCallback で回数を数える、上限を超えたら失敗
        A->>G: GenerateContent
        G-->>A: FunctionCall
        alt kibitz のツール
            A->>T: read_file / grep_files / write_file など
            T-->>A: 結果、外に出るパスは os.Root が拒否
        else MCP のツール
            A->>MCP: get_pr_diff など
            MCP-->>A: 結果
        end
        A-->>R: session.Event、UsageMetadata と FunctionResponse を集計
    end
    R->>R: セッションを記録、古いものから捨てる
    R->>T: os.Root 経由で review.json を読む → ParseOutput
```

### モードごとのツール

| モード | 使えるツール | 書き込める場所 |
| --- | --- | --- |
| review / triage | `read_file` `list_dir` `glob_files` `grep_files` `write_file` | 出力ファイル 1 つだけ (`.kibitz/out/review.json` または `triage.json`) |
| answer / plan | `read_file` `list_dir` `glob_files` `grep_files` | なし (`write_file` を渡さない) |
| implement | (起動時に拒否) | — |

- ツール名が `read` ではなく `read_file` なのは、ADK が MCP のツール名に接頭辞を付けないため。
  付けないので、MCP サーバーの `read` とぶつかる。ぶつかったツールは、後から来た方を外して警告を出す。
- ツールの結果には上限がある (1 回の読み取りは 2,000 行・256 KiB、grep は 200 件など)。
  結果は以降の毎ターンで課金されるため。

### ほかの 2 つと違う点

- **プロンプト**: `.kibitz/prompt.md` を書かず、ユーザーメッセージの本文として直接渡す。
  システムプロンプトは OpenCode のエージェント定義 (`deploy/opencode/agents/*.md`) の本文で、3 エンジンで共通。
  `Instruction` ではなく `InstructionProvider` で渡すのは、`Instruction` がテンプレートとして解釈され、
  定義に含まれる JSON の `{}` が壊れるから。
- **egress**: プロキシを通るのは MCP サーバーの通信だけ。モデルへの通信は、worker 自身が設定どおりの宛先に、
  エージェントに見せない資格情報で行うものなので、プロキシを通さない。
- **チェックアウトの設定**: ADK は `AGENTS.md` も設定ファイルも読まない。止めるフラグが要らない。
- **暴走の歯止め**: `KIBITZ_ADK_MAX_MODEL_CALLS` (既定 100) を超えるとその回は失敗になる。
- **失敗の判定**: ループのエラー、またはイベントの `ErrorCode`。answer / plan で応答が空なら、終了理由と一緒に失敗にする。

## 6. 3 つの比較

| | OpenCode | pi | ADK |
| --- | --- | --- | --- |
| 実行形態 | 子プロセス `opencode run` | 子プロセス `pi --mode json` | worker 内 (ライブラリ) |
| 権限の作り方 | `permission` 設定 (allow / deny) | `defaultTools` + ガード拡張 | kibitz が書いたツールだけを渡す |
| ワークスペース外の拒否 | `external_directory: deny` | ガードがパスを解決して判定 | `os.Root` が開くときに拒否 |
| シェル | なし | なし | なし |
| チェックアウトの設定を読まない仕組み | `OPENCODE_DISABLE_PROJECT_CONFIG=1` | `--no-approve` ほか 3 つのフラグ | もともと読まない |
| プロンプトの渡し方 | `.kibitz/prompt.md` を `--file` で添付 | `.kibitz/prompt.md` を `@` で添付 | メッセージ本文 |
| システムプロンプト | `--agent kibitz-review` | `--append-system-prompt system.md` | `InstructionProvider` |
| 実行イベント | stdout の JSON | stdout の JSONL | `session.Event` |
| セッション | OpenCode の保存領域 / `--session` | `--session-dir` のファイル | メモリ (200 件) |
| モデル通信の egress | プロキシ経由 | プロキシ経由 | 直接 |
| 失敗からの回復 | セッション消失の再実行、打ち切りの再開 (2 回)、トークンの読み直し | なし | なし |
| モデル | 制限なし | 制限なし | Gemini のみ |
| 対応モード | すべて | すべて | implement 以外 |

3 つとも、出力の契約 (`.kibitz/out/review.json`)、MCP サーバーの選び方、子プロセスに渡す環境変数の絞り方
(許可リスト + MCP 定義が参照する変数だけ) は同じ。どのエンジンでも、エージェントの後で worker が同じ `Sanitize` を通す。

## 参照

- [agent-engine.md](agent-engine.md): エンジンの比較と選定、抽象化の契約
- [worker.md](worker.md): OpenCode のヘッドレス実行、MCP 統合、プロンプト設計
- [triage.md](triage.md): トリアージと 3 つの diff
- [security.md](security.md): プロンプトインジェクションと fork PR の扱い
- ADR: [0003](adr/0003-opencode-as-agent-engine.md) / [0024](adr/0024-keep-opencode-after-pi-gained-mcp.md) / [0025](adr/0025-trial-adk-go-as-an-in-process-engine.md)

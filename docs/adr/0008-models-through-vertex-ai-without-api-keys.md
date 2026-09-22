# モデルは Vertex AI 経由で使い、API キーを持たない

- **状態**: 採用 (2026-09-21、既定モデルを 2026-09-21 に変更)
- **関連**: `9256f2a` / `fb98b4e` / `c456bff` / `docs/deployment.md`

## 背景

レビューを書くモデルをどこから呼ぶか。
Anthropic や OpenAI に直接 API キーで繋ぐのが最短だが、
この worker は**サーバー上で、人間の承認なしに、第三者が書ける入力を処理する**。
そこに長期有効な API キーを置く意味を考える必要があった。

## 決定

**Vertex AI 経由で使い、Workload Identity で認証する。worker は API キーを持たない。**

- 既定のプロバイダは `google-vertex`、既定のモデルは **Gemini**。
- リージョンの既定は `global`。
- Vertex の Model Garden で提供される第三者モデル (GLM など) は、
  `vertex-maas/<publisher>/<model>` という名前で指定すると、
  OpenAI 互換プロバイダを Vertex のエンドポイントに向けて宣言する。
  資格情報はジョブごとに発行する。
- API キーが要るプロバイダも使える。ただし**設定に入るのは環境変数名だけ**で、
  値は Secret Manager から環境に入る。

## 理由

### キーを持たないことの価値

Workload Identity なら、worker の資格情報は**そのサービスアカウントそのもの**で、
どこかに書き出された文字列ではない。漏れる場所が減る。

Model Garden 経由の第三者モデルでも、この性質は保てる。
トークンは 1 時間で切れ、config はジョブごとに書き直され、
ジョブは 15 分で打ち切られるので、**短命な資格情報が問題にならない**。
解決すべき課題ではなく、すでにある構造から落ちてくる。

### 既定を Claude から Gemini に変えた理由

Vertex AI は **Anthropic のモデルをアクセス申請の後にしか提供しない**。
申請していない環境では既定値が動かないので、既定として不適切だった。
Gemini on Vertex は申請不要で、worker 自身のサービスアカウントで認証できる。

### 記憶ではなくカタログを見るべきだった

古い既定値は**二重に間違っていた**。実際のカタログに対して
`opencode models` を走らせて、ようやく確定した。

- プロバイダは `google-vertex` で、**Gemini と Claude の両方を出す**。
  `google-vertex-anthropic` は別プロバイダ。既定値は後者を指していた。
- Vertex の Claude の ID には**バージョン接尾辞が付く** (`claude-opus-5@default`)。
  モデル側も間違っていた。

同じ誤りをもう一度やった。「GLM は Vertex AI に無い」と報告したが、
これは**opencode のカタログに無い**ことを Vertex に無いと言い換えたもので、別の主張だった。
Vertex は Model Garden のマネージドサービスとして GLM を提供している。

## 影響

- `docs/deployment.md` に**検証済みのプロバイダ / モデル ID の表**と、
  worker イメージの中からカタログを引くコマンドがある。
  次にモデルを変えるときは、推測にしない。
- Model Garden のエンドポイント URL は、この環境から検証できなかったので
  **上書き可能** (`KIBITZ_VERTEX_MAAS_BASE_URL`)。
  デプロイ前に URL とモデル ID を確認する curl が `docs/deployment.md` にある。
  間違えても設定変更で済み、リリースにはならない。
- API キー方式のプロバイダでは、**worker は転送した環境変数の名前をログに出す。値は出さない。**
- リージョンを data residency のために固定すると、使えるモデルが制約される。
- `KIBITZ_TRIAGE_MODEL` で補助的な処理に安いモデルを充てられる (ADR-0013)。

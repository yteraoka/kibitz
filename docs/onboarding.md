# 導入手順 (リポジトリを kibitz に載せる)

kibitz 本体のデプロイは [deployment.md](deployment.md)。この文書は**動いている
kibitz に、新しいリポジトリを 1 つ追加する**ための手順で、3 プラットフォームを
それぞれ扱う。

## 0. 先に決める 3 つ

どのプラットフォームでも共通で、**サーバー側の設定**。リポジトリ側からは変えられない。

| 設定 | 何を決めるか |
| --- | --- |
| `KIBITZ_ALLOWED_REPOS` | 受け付けるリポジトリ。**ここに無いリポジトリからの配送は捨てられる。** Webhook を先に設定しても動かない |
| `KIBITZ_TRIGGER_KEYWORDS` | 設定すると、PR 系イベントはタイトルか本文にキーワードかメンションを含むときだけレビューする。**キューを空に保つ手段**でもある (ワーカーが 0 台に落ちる) |
| `KIBITZ_REPO_BUDGETS` | 1 か月の上限額。未設定なら上限なし ([configuration.md](configuration.md#リポジトリ別の予算)) |

`KIBITZ_ALLOWED_REPOS` は `owner/name` に対するグロブで、カンマ区切り。

```
KIBITZ_ALLOWED_REPOS=acme/*,contrib/kibitz-sandbox
```

> **リポジトリの追加は、Webhook の設定だけでは終わらない。**
> 許可リストが `*` でない限り、サーバー側にも追加が要る。
> 許可されていないリポジトリは理由 `repo_not_allowed` として
> 捨てられ、PR 側には何も出ない。

## 1. GitHub

### 1-1. App をインストールする

App 自体の作成は [deployment.md §1](deployment.md#1-github-app-を作る)。すでに
App がある場合、新しいリポジトリの追加は**インストール先の選択を変えるだけ**。

```
https://github.com/settings/installations        # 個人
https://github.com/organizations/YOUR_ORG/settings/installations   # 組織
```

対象の App → **Configure** → Repository access で追加する。

> **選び忘れると `git fetch` が失敗する。** App がインストールされていても、
> そのリポジトリが選ばれていなければ clone 用のトークンは通らない。
> 症状は「Webhook は届くのにレビューが始まらない」。

### 1-2. 購読しているイベント

App 側の設定なので、リポジトリごとの作業はない。念のため。

| イベント | kibitz が使う action |
| --- | --- |
| Pull request | `opened` / `reopened` / `synchronize` / `ready_for_review` / `review_requested` / `closed` |
| Issue comment | PR 上のコメント (GitHub では PR も issue) |
| Pull request review comment | 差分行へのコメント |

配送先は `POST /webhook/github`。検証は `X-Hub-Signature-256` の HMAC。

## 2. GitLab

GitLab には GitHub App に相当するものが無い。**プロジェクト (またはグループ) ごとに
Webhook を作る。**

### 2-1. Webhook を作る

プロジェクト → **Settings → Webhooks → Add new webhook**。

| 項目 | 値 |
| --- | --- |
| URL | `https://<kibitz>/webhook/gitlab` |
| Secret token | 下記 |
| Trigger | **Merge request events** と **Comments** の 2 つだけ |
| SSL verification | 有効のまま |

Trigger は 2 つで足りる。kibitz が見るのは `Merge Request Hook` と
`Note Hook` だけで、他は受け取っても捨てる。

> **グループ Webhook は Premium 以上。** 無料版ではプロジェクトごとに作る。
> 多数あるなら [GitLab の API](https://docs.gitlab.com/api/projects/#add-project-hook)
> でまとめて作るほうが早い。

### 2-2. どちらのトークンを使うか

GitLab は**バージョンによって検証方法が違う**。kibitz は両方を受け付ける。

| | 設定する環境変数 | 何を検証できるか |
| --- | --- | --- |
| **署名トークン** (GitLab 19.0+、`whsec_...`) | `KIBITZ_GITLAB_SIGNING_TOKENS` | **本文まで**。こちらを使う |
| 共有トークン (従来) | `KIBITZ_GITLAB_WEBHOOK_TOKENS` | 送信者だけ。本文は検証されない |

両方設定した場合、**署名が付いていれば署名で検証する**。移行期はこれで両対応できる。

署名は 5 分より古い配送を拒否する (タイムスタンプが署名対象に含まれるため、
上限が無いと捕まえた配送を永久に再生できる)。

### 2-3. API トークン

kibitz が投稿するために `KIBITZ_GITLAB_TOKEN` が要る。`api` スコープの
personal / group / project access token。

> **GitLab には GitHub App のインストールトークンに相当するものが無く、
> 長命な資格情報になる。** group access token を使い、ローテーション手順を
> 決めておくこと ([runbook.md](runbook.md#シークレットのローテーション))。

## 3. Azure DevOps

**Azure DevOps は Service Hook に署名しない。** 資格情報は Basic 認証だけで、
これが唯一の防御になる ([ADR-0007](adr/0007-github-app-only.md) と同じ理由で
扱いを厚くしてある)。

### 3-1. Service Hook を 4 本作る

Project settings → **Service hooks** → **+** → **Web Hooks**。

kibitz が見る `eventType` は 4 つで、**1 本につき 1 イベント**しか選べないので
4 本作る。

| Trigger (UI の名前) | eventType |
| --- | --- |
| Pull request created | `git.pullrequest.created` |
| Pull request updated | `git.pullrequest.updated` |
| Pull request merge attempted | `git.pullrequest.merged` |
| Pull request commented on | `ms.vss-code.git-pullrequest-comment-event` |

どの本も Action は **Web Hooks**、設定は同じ。

| 項目 | 値 |
| --- | --- |
| URL | `https://<kibitz>/webhook/azure-devops` |
| Basic authentication username | `KIBITZ_AZDO_BASIC_USER` と同じ値 |
| Basic authentication password | `KIBITZ_AZDO_BASIC_PASSWORDS` のいずれか |
| HTTP headers | 任意。`KIBITZ_AZDO_HEADER_NAME: 値` の形 |
| Resource details to send | **All** |

`Resource details to send` は **All** にする。Minimal では PR の情報が足りない。

### 3-2. 固定ヘッダを足す理由

Basic 認証のパスワードだけが漏れると、**誰でも偽の PR イベントを送れる**。
`KIBITZ_AZDO_HEADER_NAME` / `_VALUES` を設定すると、**両方**一致しない限り
受け付けない。

```
KIBITZ_AZDO_HEADER_NAME=X-Kibitz-Hook
KIBITZ_AZDO_HEADER_VALUES=<長いランダム値>
```

Service Hook 側の HTTP headers に `X-Kibitz-Hook: <同じ値>` を入れる。

> 署名が無い以上、**受け取った内容は主張であって事実ではない。**
> kibitz はイベントの中身を信用せず、API で PR を取り直してから判断する
> ([ADR-0004](adr/0004-platform-differences-live-in-two-places.md))。

### 3-3. API トークン

| 変数 | 値 |
| --- | --- |
| `KIBITZ_AZDO_ORG_URL` | `https://dev.azure.com/{org}` (Server なら `https://{server}/{collection}`) |
| `KIBITZ_AZDO_TOKEN` | PAT または Entra ID のアクセストークン |
| `KIBITZ_AZDO_TOKEN_IS_BEARER` | Entra ID の場合のみ `true` |

PAT に必要なスコープは **Code (Read)** と **Pull Request Threads (Read & write)**。

> **PAT と Entra ID トークンは送る場所が違う。** PAT は空ユーザーの Basic 認証の
> パスワード、Entra ID は Bearer。入れ替えるとどちらも拒否されるが、
> **返ってくるのは 401 ではなく「203 + サインインページの HTML」**なので、
> 症状から原因にたどり着きにくい。kibitz はこれを検出して
> 「トークンが受け付けられていない」と報告する。

## 4. リポジトリ側の設定 (任意)

`.kibitz.yaml` をリポジトリの**デフォルトブランチ**に置くと、そのリポジトリだけ
挙動を変えられる。無くても動く。

```yaml
version: 1
review:
  paths_ignore:
    - "vendor/**"
    - "**/*_generated.go"
  focus:
    - "並行処理"
    - "SQL インジェクション"
  min_severity: high
  max_comments: 10
guidelines: docs/review-guidelines.md
```

読むのは**常にデフォルトブランチ側**で、PR 側ではない。PR 側から読むと、
**PR を開いた人がその PR のレビューのされ方を決められてしまう**
([ADR-0011](adr/0011-repository-settings-from-the-default-branch.md)、
[security.md](security.md))。

同じ理由で、**予算 (`KIBITZ_REPO_BUDGETS`) と許可リストはここには置けない。**
リポジトリ側から上げられる上限は上限ではない。

## 5. 動いているか確かめる

1. 対象リポジトリで PR を開く (キーワードを設定しているなら、それを含める)
2. サーバーのログに `published` が出る
3. ワーカーが起動する (0 台からの起動は 30 秒ほどかかる)
4. PR にサマリコメントが付く

```bash
# 受け取ったが publish しなかったものを、理由つきで
gcloud run services logs read kibitz-server --region "$REGION" --limit 50 | \
  grep -E "skipped|rejected"
```

コメントで `/kibitz help` と書くと、応答するかどうかだけを確かめられる
(モデルを呼ばないので無料)。

## 6. つまずく場所

| 症状 | 原因 |
| --- | --- |
| Webhook は 200 だがレビューされない | `KIBITZ_ALLOWED_REPOS` に入っていない。ログの `reason` が `repo_not_allowed` |
| 同上 (GitHub) | App がそのリポジトリにインストールされていない。clone が落ちる |
| 同上 (キーワード設定時) | タイトルにも本文にもキーワードが無い (`reason` が `no_keyword`)。コメントでの依頼は常に通る |
| 401 が返る | 検証に失敗している。[deployment.md §7 の切り分け](deployment.md#401-の切り分け) |
| Azure DevOps で `invalid character '<'` 系 | トークンが受け付けられていない (203 + サインインページ)。スコープか、Bearer かどうかの設定 |
| draft PR がレビューされない | 既定の挙動 (`KIBITZ_SKIP_DRAFT=true`)。`/kibitz review` で明示実行できる |
| 予算の通知が出て止まっている | 今月分を使い切った。`KIBITZ_REPO_BUDGETS` を見直すか、翌月まで待つ |

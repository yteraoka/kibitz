package worker

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yteraoka/kibitz/internal/forge"
	"github.com/yteraoka/kibitz/internal/repoconfig"
	"github.com/yteraoka/kibitz/internal/reviewer"
	"github.com/yteraoka/kibitz/internal/sandbox"
)

// What implement mode says, and where.
//
// There are five outcomes and each gets its own comment, because they are
// different news. "Nothing was changed" and "it was changed and the tests
// failed" are not variations on a theme: the first is a judgement the agent
// made and the second is a fact about the code, and somebody reading the issue
// has to be able to tell which one happened without opening the log.
//
// All of them carry the agent's own account of what it did. That account is the
// only thing it produced in three of the five cases, and throwing it away
// because the change was refused would throw away the reason it was refused.

// maxStepOutput bounds how much of a failing command's output is quoted. The
// runner already caps it (sandbox.MaxOutputBytes); this is the second cap,
// because a pull request comment has its own limit and five failing steps would
// otherwise reach it.
const maxStepOutput = 6 << 10

// noChangesComment reports that the agent decided nothing needed changing.
//
// It is not treated as a failure. An agent that read the issue, read the code
// and concluded that the change should not be made has done something useful,
// and the reason it gives is the reason a person wanted.
func (j *ReviewJob) noChangesComment(result *reviewer.Result, headSHA string, settings resolved, p pass) string {
	var b strings.Builder

	b.WriteString("## 実装: 変更なし\n\n")
	b.WriteString("**コードは変更していません。** 以下が、そう判断した理由です。\n\n")
	writeAgentAccount(&b, result)
	j.writeImplementFooter(&b, headSHA, result, settings, p)
	return b.String()
}

// outsidePathsComment reports that the change was thrown away because it
// reached outside what the repository allows.
//
// The paths are named. Half of the reason this happens is a repository whose
// paths_allow is narrower than the change it asked for, and the person who can
// widen it cannot do so without being told which paths were missing.
func (j *ReviewJob) outsidePathsComment(outside, changed []string, result *reviewer.Result, settings resolved, p pass) string {
	var b strings.Builder

	b.WriteString("## 実装: 破棄しました (編集が許可されていないパス)\n\n")
	b.WriteString("**プルリクエストは作っていません。** 次のパスは編集が許可されていません。\n\n")
	for _, path := range outside {
		fmt.Fprintf(&b, "- `%s`\n", path)
	}

	b.WriteString("\n変更は**すべて**破棄しました。許可された分だけを残すことはしません: ")
	b.WriteString("片方だけ適用された編集は、どちらでもないものになります。\n\n")
	b.WriteString("意図した変更であれば、`" + repoconfig.Path + "` の `implement.paths_allow` に追加してください。\n")
	b.WriteString("CI 設定・依存定義ファイル・kibitz 自身の設定は、`paths_allow` に書いても編集できません。\n\n")

	writeChangedFiles(&b, changed)
	b.WriteString("\n### エージェントの報告\n\n")
	writeAgentAccount(&b, result)
	j.writeImplementFooter(&b, "", result, settings, p)
	return b.String()
}

// verifyFailedComment reports that the change was written and did not build.
//
// This is the comment the whole arrangement exists to be able to write. The
// output goes in it verbatim, because "the tests failed" is not information and
// the last twenty lines of the failure is.
func (j *ReviewJob) verifyFailedComment(verified *sandbox.Result, changed []string, result *reviewer.Result, settings resolved, p pass) string {
	var b strings.Builder

	b.WriteString("## 実装: 検証に失敗しました\n\n")
	b.WriteString("**プルリクエストは作っていません。** 変更は書けましたが、")
	b.WriteString("リポジトリ自身のビルド・テストが通りませんでした。\n\n")

	if verified.Failure != "" {
		b.WriteString("検証そのものを実行できませんでした。\n\n")
		b.WriteString("```\n")
		b.WriteString(truncate(verified.Failure, 2000))
		b.WriteString("\n```\n\n")
	}
	writeVerifySteps(&b, verified)
	writeChangedFiles(&b, changed)

	b.WriteString("\n### エージェントの報告\n\n")
	b.WriteString("_この報告は、検証の前に書かれたものです。_\n\n")
	writeAgentAccount(&b, result)
	j.writeImplementFooter(&b, "", result, settings, p)
	return b.String()
}

// openedComment is what the issue says once there is something to look at.
// It is short on purpose: everything worth reading is in the pull request.
func (j *ReviewJob) openedComment(created *forge.PullRequestInfo, changed []string, verified *sandbox.Result) string {
	var b strings.Builder

	b.WriteString("## 実装: draft プルリクエストを作りました\n\n")
	fmt.Fprintf(&b, "%s\n\n", created.URL)
	if !created.Draft {
		// Said rather than left to be noticed. Somebody who expected a draft
		// and got a pull request that can be merged should hear it from kibitz.
		b.WriteString("_このリポジトリでは draft のプルリクエストを作れないため、通常のプルリクエストになっています。_\n\n")
	}

	fmt.Fprintf(&b, "変更 %d ファイル。", len(changed))
	if n := len(verified.Steps); n > 0 {
		fmt.Fprintf(&b, "検証 %d ステップすべて通っています。", n)
	}
	b.WriteString("\n\n**AI が書いたコードです。人のレビューを前提にしています。**\n")
	return b.String()
}

// pullRequestBody is the description of the change, written for whoever is
// about to review it.
//
// The first thing in it is what it is: a change written by a model from an
// issue, with nothing but the repository's own tests behind it. A reviewer who
// reads it as an ordinary pull request will review it as one.
func (j *ReviewJob) pullRequestBody(result *reviewer.Result, ref forge.IssueRef, verified *sandbox.Result, changed []string, settings resolved, p pass) string {
	var b strings.Builder

	b.WriteString("> **このプルリクエストは kibitz (AI) が Issue から書いたものです。**\n")
	b.WriteString("> 人はまだ読んでいません。リポジトリ自身のビルドとテストが通っていること以外、\n")
	b.WriteString("> 何も確認されていません。\n\n")

	fmt.Fprintf(&b, "Closes #%d\n\n", ref.Number)

	b.WriteString("## エージェントの報告\n\n")
	writeAgentAccount(&b, result)

	b.WriteString("\n## 検証\n\n")
	if len(verified.Steps) == 0 {
		b.WriteString("_実行したコマンドはありません。_\n")
	} else {
		b.WriteString("| コマンド | 結果 | 所要 |\n| --- | --- | --- |\n")
		for _, step := range verified.Steps {
			outcome := "通った"
			if !step.Passed() {
				outcome = fmt.Sprintf("**失敗 (exit %d)**", step.ExitCode)
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n",
				strings.Join(step.Command, " "), outcome, step.Duration.Round(time.Millisecond))
		}
	}
	// No link. This body is posted in somebody else's repository, where a
	// relative path into kibitz's own documentation resolves to a file that is
	// not there.
	b.WriteString("\nこれらは kibitz の資格情報を持たない別の環境で実行されています。\n\n")

	writeChangedFiles(&b, changed)
	b.WriteString("\n---\n")
	j.writeUsage(&b, result.Usage, p)
	writeSettingsNotes(&b, settings)
	return b.String()
}

// writeAgentAccount puts the agent's own words in, fenced.
//
// It is fenced for the same reason everything else third-party is fenced, and
// for one more: this text goes into a pull request description, where kibitz
// itself will read it again when somebody asks a question on that pull request.
func writeAgentAccount(b *strings.Builder, result *reviewer.Result) {
	account := ""
	if result != nil {
		account = strings.TrimSpace(result.Reply)
	}
	if account == "" {
		b.WriteString("_エージェントは何も書きませんでした。_\n")
		return
	}
	b.WriteString(account)
	b.WriteString("\n")
}

// writeVerifySteps lists what ran and quotes the failure.
func writeVerifySteps(b *strings.Builder, verified *sandbox.Result) {
	if len(verified.Steps) == 0 {
		return
	}

	b.WriteString("### 実行したコマンド\n\n")
	for _, step := range verified.Steps {
		mark := "✓"
		switch {
		case step.TimedOut:
			mark = "⏱ タイムアウト"
		case !step.Passed():
			mark = fmt.Sprintf("✗ exit %d", step.ExitCode)
		}
		fmt.Fprintf(b, "- `%s` — %s (%s)\n",
			strings.Join(step.Command, " "), mark, step.Duration.Round(time.Millisecond))
	}

	failed, ok := verified.FirstFailure()
	if !ok {
		return
	}
	b.WriteString("\n### 出力\n\n")
	fmt.Fprintf(b, "`%s`:\n\n", strings.Join(failed.Command, " "))
	b.WriteString("```\n")
	b.WriteString(truncate(failed.Output, maxStepOutput))
	b.WriteString("\n```\n")
	if failed.Truncated {
		b.WriteString("\n_出力は末尾のみです。_\n")
	}
	b.WriteString("\n")
}

// writeChangedFiles lists what was touched. It is a list of paths and not a
// diff: the diff is in the pull request, or it is nowhere because there is no
// pull request.
func writeChangedFiles(b *strings.Builder, changed []string) {
	if len(changed) == 0 {
		return
	}

	fmt.Fprintf(b, "### 変更したファイル (%d)\n\n", len(changed))
	const maxListed = 50
	for i, path := range changed {
		if i == maxListed {
			fmt.Fprintf(b, "- _ほか %d 件_\n", len(changed)-maxListed)
			break
		}
		fmt.Fprintf(b, "- `%s`\n", path)
	}
}

// writeImplementFooter says what the run cost and what the settings did, which
// is the same footer a review gets.
func (j *ReviewJob) writeImplementFooter(b *strings.Builder, headSHA string, result *reviewer.Result, settings resolved, p pass) {
	b.WriteString("\n---\n")
	if headSHA != "" {
		fmt.Fprintf(b, "対象: `%s` (デフォルトブランチ)", shortSHA(headSHA))
		if result != nil && result.Usage.Duration > 0 {
			fmt.Fprintf(b, " / 所要 %s", result.Usage.Duration.Round(time.Second))
		}
		b.WriteString("\n")
	}
	if result != nil {
		j.writeUsage(b, result.Usage, p)
	}
	writeSettingsNotes(b, settings)
}

// truncate cuts a string to at most limit bytes without splitting a character.
//
// Bytes rather than characters because what it is protecting is a size limit,
// and rune-safe because the text is Japanese: cutting mid-character produces a
// replacement glyph in the middle of a sentence somebody has to read.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "…"
}

package sandbox_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yteraoka/kibitz/internal/sandbox"
)

// TestAFailingCommandIsReportedAsFailing is the whole reason this exists. A
// verification that reported success for a failing build would let implement
// mode open pull requests that do not compile, which is worse than opening
// none.
func TestAFailingCommandIsReportedAsFailing(t *testing.T) {
	dir := t.TempDir()

	result := sandbox.Verify(context.Background(), dir, &sandbox.Request{
		Commands: [][]string{
			{"true"},
			{"false"},
		},
	})

	if result.Passed() {
		t.Fatalf("a run containing a failing command passed: %+v", result)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("%d steps ran, want 2: %+v", len(result.Steps), result.Steps)
	}
	if !result.Steps[0].Passed() {
		t.Errorf("the first step failed: %+v", result.Steps[0])
	}

	failed, ok := result.FirstFailure()
	if !ok {
		t.Fatal("FirstFailure found nothing in a failed run")
	}
	if failed.ExitCode == 0 {
		t.Errorf("the failing step reports exit code 0: %+v", failed)
	}
}

func TestEveryCommandPassingIsAPass(t *testing.T) {
	result := sandbox.Verify(context.Background(), t.TempDir(), &sandbox.Request{
		Commands: [][]string{{"true"}, {"true"}},
	})
	if !result.Passed() {
		t.Fatalf("a run of passing commands did not pass: %+v", result)
	}
}

// TestNothingRunAfterAFailure: the tests after a build that did not compile
// produce noise, not information.
func TestNothingRunAfterAFailure(t *testing.T) {
	result := sandbox.Verify(context.Background(), t.TempDir(), &sandbox.Request{
		Commands: [][]string{{"false"}, {"true"}},
	})
	if len(result.Steps) != 1 {
		t.Errorf("%d steps ran after a failure, want 1: %+v", len(result.Steps), result.Steps)
	}
}

// TestAnEmptyRunHasNotPassed: a verification that verified nothing must not
// read as a green light.
func TestAnEmptyRunHasNotPassed(t *testing.T) {
	if (&sandbox.Result{}).Passed() {
		t.Error("a result with no steps passed")
	}
	if (&sandbox.Result{Failure: "the archive would not extract"}).Passed() {
		t.Error("a result that failed outright passed")
	}
	var nilResult *sandbox.Result
	if nilResult.Passed() {
		t.Error("a nil result passed")
	}
}

func TestACommandThatDoesNotExistFails(t *testing.T) {
	result := sandbox.Verify(context.Background(), t.TempDir(), &sandbox.Request{
		Commands: [][]string{{"kibitz-no-such-command"}},
	})
	if result.Passed() {
		t.Fatalf("a command the image does not have passed: %+v", result)
	}
	if !strings.Contains(result.Steps[0].Output, "kibitz-no-such-command") {
		t.Errorf("the output does not say what could not be run: %+v", result.Steps[0])
	}
}

func TestTheTimeoutIsReportedAsSuch(t *testing.T) {
	result := sandbox.Verify(context.Background(), t.TempDir(), &sandbox.Request{
		Commands: [][]string{{"sleep", "10"}},
		Timeout:  200 * time.Millisecond,
	})
	if result.Passed() {
		t.Fatal("a run that timed out passed")
	}
	if !result.Steps[0].TimedOut {
		t.Errorf("the step is not marked as timed out: %+v", result.Steps[0])
	}
}

// TestTheCommandRunsInTheTree covers the thing a build depends on: it has to
// run where the code is.
func TestTheCommandRunsInTheTree(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := sandbox.Verify(context.Background(), dir, &sandbox.Request{
		Commands: [][]string{{"cat", "marker.txt"}},
	})
	if !result.Passed() {
		t.Fatalf("the command did not run in the tree: %+v", result)
	}
	if result.Steps[0].Output != "x" {
		t.Errorf("output is %q, want x", result.Steps[0].Output)
	}
}

// TestTheEnvironmentIsBuiltNotInherited: a build that depends on something
// kibitz happened to have in its environment would pass here and fail in the
// repository's own CI.
func TestTheEnvironmentIsBuiltNotInherited(t *testing.T) {
	t.Setenv("KIBITZ_GITHUB_PRIVATE_KEY", "a-secret-that-must-not-be-visible")

	result := sandbox.Verify(context.Background(), t.TempDir(), &sandbox.Request{
		Commands: [][]string{{"env"}},
	})
	if !result.Passed() {
		t.Fatalf("env did not run: %+v", result)
	}
	if strings.Contains(result.Steps[0].Output, "a-secret-that-must-not-be-visible") {
		t.Errorf("a credential from the surrounding process reached the build:\n%s", result.Steps[0].Output)
	}
	if !strings.Contains(result.Steps[0].Output, "PATH=") {
		t.Errorf("PATH did not reach the build, so nothing would run:\n%s", result.Steps[0].Output)
	}
}

func TestOutputIsTruncatedFromTheFront(t *testing.T) {
	// The tail is what says why something failed.
	result := sandbox.Verify(context.Background(), t.TempDir(), &sandbox.Request{
		Commands: [][]string{{"sh", "-c", "for i in $(seq 1 4000); do echo line-$i; done; echo THE-LAST-LINE"}},
	})
	if !result.Passed() {
		t.Fatalf("the command failed: %+v", result)
	}

	step := result.Steps[0]
	if !step.Truncated {
		t.Fatalf("output of %d bytes was not marked truncated", len(step.Output))
	}
	if len(step.Output) > sandbox.MaxOutputBytes {
		t.Errorf("kept %d bytes, more than the %d limit", len(step.Output), sandbox.MaxOutputBytes)
	}
	if !strings.Contains(step.Output, "THE-LAST-LINE") {
		t.Error("the end of the output was dropped; the tail is the part that says why")
	}
	if strings.Contains(step.Output, "line-1\n") {
		t.Error("the start of the output was kept instead of the end")
	}
}

func TestParseCommandsRefusesWhatNeedsAShell(t *testing.T) {
	// There is no shell, so accepting these would run "go" with three odd
	// arguments and call it a pass.
	for _, command := range []string{
		"go build ./... && go test ./...",
		"make build; make test",
		"go test ./... | tee out",
		"go test ./... > out",
		"echo $HOME",
		"echo `date`",
	} {
		t.Run(command, func(t *testing.T) {
			if _, err := sandbox.ParseCommands([]string{command}); err == nil {
				t.Errorf("ParseCommands(%q) was accepted", command)
			}
		})
	}
}

func TestParseCommandsSplitsIntoArgv(t *testing.T) {
	got, err := sandbox.ParseCommands([]string{"go build ./...", "  go test ./internal/...  ", ""})
	if err != nil {
		t.Fatalf("ParseCommands: %v", err)
	}
	want := [][]string{{"go", "build", "./..."}, {"go", "test", "./internal/..."}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if strings.Join(got[i], " ") != strings.Join(want[i], " ") {
			t.Errorf("command %d is %v, want %v", i, got[i], want[i])
		}
	}
}

func TestParseCommandsRefusesAnEmptyList(t *testing.T) {
	if _, err := sandbox.ParseCommands(nil); err == nil {
		t.Error("an empty command list was accepted; nothing would be verified")
	}
	if _, err := sandbox.ParseCommands([]string{"", "  "}); err == nil {
		t.Error("a list of blanks was accepted")
	}
}

// --- the archive -------------------------------------------------------

func TestPackAndUnpackRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, src, "main.go", "package main\n")
	write(t, src, "internal/a/b.go", "package a\n")
	write(t, src, ".git/config", "[core]\n") // must not travel

	var buf bytes.Buffer
	if err := sandbox.Pack(src, &buf); err != nil {
		t.Fatalf("Pack: %v", err)
	}

	dst := t.TempDir()
	if err := sandbox.Unpack(&buf, dst); err != nil {
		t.Fatalf("Unpack: %v", err)
	}

	if got := read(t, dst, "main.go"); got != "package main\n" {
		t.Errorf("main.go is %q", got)
	}
	if got := read(t, dst, "internal/a/b.go"); got != "package a\n" {
		t.Errorf("internal/a/b.go is %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "config")); err == nil {
		t.Error(".git travelled; the runner builds a tree and has no use for history")
	}
}

// TestUnpackRefusesAnEntryThatClimbsOut is the classic abuse of an archive
// from another process, and the archive here comes from one.
func TestUnpackRefusesAnEntryThatClimbsOut(t *testing.T) {
	for _, name := range []string{"../escaped.txt", "a/../../escaped.txt", "/etc/escaped.txt"} {
		t.Run(name, func(t *testing.T) {
			archive := hostileArchive(t, name)
			dst := t.TempDir()

			err := sandbox.Unpack(bytes.NewReader(archive), dst)
			// Either refused outright, or contained: what must not happen is
			// a file appearing outside dst.
			outside := filepath.Join(filepath.Dir(dst), "escaped.txt")
			if _, statErr := os.Stat(outside); statErr == nil {
				t.Fatalf("%q was written outside the extraction directory (Unpack err = %v)", outside, err)
			}
			if _, statErr := os.Stat("/etc/escaped.txt"); statErr == nil {
				t.Fatal("an absolute path was written")
			}
		})
	}
}

func TestUnpackRefusesSomethingThatIsNotGzip(t *testing.T) {
	if err := sandbox.Unpack(strings.NewReader("not an archive"), t.TempDir()); err == nil {
		t.Error("a body that is not gzip was accepted")
	}
}

// TestASymlinkDoesNotTravel: a link is either meaningless in the extracted
// tree or a way out of it.
func TestASymlinkDoesNotTravel(t *testing.T) {
	src := t.TempDir()
	write(t, src, "real.txt", "x")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "link")); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	var buf bytes.Buffer
	if err := sandbox.Pack(src, &buf); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	dst := t.TempDir()
	if err := sandbox.Unpack(&buf, dst); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Error("a symlink was recreated in the extracted tree")
	}
}

func TestTheWireFormatRoundTrips(t *testing.T) {
	req := sandbox.Request{Commands: [][]string{{"go", "test", "./..."}}, Timeout: time.Minute}
	data, err := req.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := sandbox.UnmarshalRequest(data)
	if err != nil {
		t.Fatalf("UnmarshalRequest: %v", err)
	}
	if len(back.Commands) != 1 || back.Commands[0][0] != "go" || back.Timeout != time.Minute {
		t.Errorf("round trip changed the request: %+v", back)
	}

	if _, err := sandbox.UnmarshalRequest([]byte(`{"commands":[]}`)); err == nil {
		t.Error("a request naming no commands was accepted")
	}
	if _, err := sandbox.UnmarshalRequest([]byte(`not json`)); err == nil {
		t.Error("a body that is not JSON was accepted")
	}
}

// --- helpers -----------------------------------------------------------

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}

// hostileArchive builds a tar whose single entry is named name. Pack would
// never produce one, which is the point: Unpack reads what another process
// wrote.
func hostileArchive(t *testing.T, name string) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	body := []byte("escaped\n")
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o644,
		Size:     int64(len(body)),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A tree that did not arrive whole must not extract as though it had. The
// review that raised this described the failure as io.EOF being swallowed;
// what a cut-short archive actually produces is io.ErrUnexpectedEOF, so the
// case was already an error. The test is here to keep it one, and to record
// which error it is.
func TestUnpackRefusesATruncatedArchive(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "internal/queue/queue.go", Mode: 0o644, Size: 1000, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	// Ten bytes where the header promised a thousand, and then the stream
	// stops: a transfer that was cut off mid-file.
	if _, err := tw.Write([]byte("package qu")); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	zw := gzip.NewWriter(&archive)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	err := sandbox.Unpack(&archive, dir)
	if err == nil {
		t.Fatal("Unpack accepted an archive that was cut short")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want one wrapping io.ErrUnexpectedEOF", err)
	}
}

// Empty files are ordinary, and reporting every error from the copy must not
// turn one into a failure.
func TestUnpackKeepsEmptyFiles(t *testing.T) {
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	for _, header := range []*tar.Header{
		{Name: "empty", Mode: 0o644, Size: 0, Typeflag: tar.TypeReg},
		{Name: "one", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg},
	} {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	zw := gzip.NewWriter(&archive)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := sandbox.Unpack(&archive, dir); err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	for name, want := range map[string]int64{"empty": 0, "one": 1} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info.Size() != want {
			t.Errorf("%s is %d bytes, want %d", name, info.Size(), want)
		}
	}
}

package adk

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// The workspace tools. Their names are kibitz's own rather than the short
// ones other engines use ("read", "grep"), because ADK does not namespace a
// tool server's tools: an MCP server that offered a "read" would otherwise
// collide with this one.
const (
	ReadFileTool  = "read_file"
	ListDirTool   = "list_dir"
	GlobTool      = "glob_files"
	GrepTool      = "grep_files"
	WriteFileTool = "write_file"
)

// Limits on what a tool returns. The model pays for every byte of a result
// on every later turn, so a tool that hands back a whole lock file is a tool
// that makes the review expensive and no better.
const (
	defaultReadLines = 2000
	maxReadBytes     = 256 << 10
	maxLineBytes     = 2000
	maxListEntries   = 1000
	maxGlobResults   = 500
	maxGrepMatches   = 200
	maxGrepFileBytes = 2 << 20
	maxWriteBytes    = 1 << 20
	binarySniffBytes = 8 << 10
)

// workspace serves files from one checkout and nothing outside it.
//
// The confinement is os.Root's, not a check of the path's spelling: a path
// that climbs out with "..", an absolute path and a symbolic link that points
// out of the checkout all fail inside the standard library, at the moment the
// file is opened. That matters more here than it did for the other engines.
// The tools run inside the worker process, which holds the GitHub App's key,
// so there is no separate process whose environment was trimmed to fall back
// on.
type workspace struct {
	root *os.Root
	// writable reports whether a path, relative to the checkout, may be
	// written. Nil means nothing may.
	writable func(string) bool
}

func openWorkspace(dir string, writable func(string) bool) (*workspace, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &workspace{root: root, writable: writable}, nil
}

func (w *workspace) Close() error { return w.root.Close() }

// clean turns what the model named into a path os.Root accepts: relative,
// slash-separated, "." for the top. A leading "/" is read as the top of the
// checkout, since that is what a model means by it far more often than the
// root of the filesystem; os.Root would refuse the absolute form anyway.
func clean(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return "."
	}
	return path.Clean(p)
}

// skipDir reports whether a walk should not descend into a directory.
// .git holds the object store, which is large, binary and not what a review
// is about.
func skipDir(name string) bool { return name == ".git" }

type readArgs struct {
	Path string `json:"path" jsonschema:"file path relative to the repository root"`
	// Offset and Limit page through a long file.
	Offset int `json:"offset,omitempty" jsonschema:"1-based line to start from (default 1)"`
	Limit  int `json:"limit,omitempty" jsonschema:"number of lines to return (default 2000)"`
}

type readResult struct {
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated,omitempty"`
}

func (w *workspace) read(_ agent.Context, args readArgs) (readResult, error) {
	name := clean(args.Path)
	f, err := w.root.Open(name)
	if err != nil {
		return readResult{}, toolError(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return readResult{}, toolError(err)
	}
	if info.IsDir() {
		return readResult{}, fmt.Errorf("%s is a directory; use %s", args.Path, ListDirTool)
	}

	br := bufio.NewReader(f)
	if head, _ := br.Peek(binarySniffBytes); bytes.IndexByte(head, 0) >= 0 {
		return readResult{}, fmt.Errorf("%s looks like a binary file", args.Path)
	}

	offset := max(args.Offset, 1)
	limit := args.Limit
	if limit <= 0 {
		limit = defaultReadLines
	}

	var b strings.Builder
	res := readResult{}
	scanner := bufio.NewScanner(br)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		res.TotalLines++
		n := res.TotalLines
		if n < offset || n >= offset+limit {
			if n >= offset+limit {
				res.Truncated = true
			}
			continue
		}
		if b.Len() > maxReadBytes {
			res.Truncated = true
			continue
		}
		line := scanner.Text()
		if len(line) > maxLineBytes {
			line = line[:maxLineBytes] + "…"
		}
		fmt.Fprintf(&b, "%6d\t%s\n", n, line)
	}
	if err := scanner.Err(); err != nil {
		return readResult{}, toolError(err)
	}
	res.Content = b.String()
	return res, nil
}

type listArgs struct {
	Path string `json:"path,omitempty" jsonschema:"directory relative to the repository root (default: the root)"`
}

type listResult struct {
	Entries   []string `json:"entries"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (w *workspace) list(_ agent.Context, args listArgs) (listResult, error) {
	entries, err := fs.ReadDir(w.root.FS(), clean(args.Path))
	if err != nil {
		return listResult{}, toolError(err)
	}
	res := listResult{Entries: []string{}}
	for _, e := range entries {
		if len(res.Entries) == maxListEntries {
			res.Truncated = true
			break
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		res.Entries = append(res.Entries, name)
	}
	return res, nil
}

type globArgs struct {
	Pattern string `json:"pattern" jsonschema:"glob over paths relative to the repository root; ** matches any number of directories, e.g. internal/**/*_test.go"`
}

type globResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (w *workspace) glob(_ agent.Context, args globArgs) (globResult, error) {
	re, err := globRegexp(clean(args.Pattern))
	if err != nil {
		return globResult{}, err
	}
	res := globResult{Paths: []string{}}
	err = fs.WalkDir(w.root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != "." && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !re.MatchString(p) {
			return nil
		}
		if len(res.Paths) == maxGlobResults {
			res.Truncated = true
			return fs.SkipAll
		}
		res.Paths = append(res.Paths, p)
		return nil
	})
	if err != nil {
		return globResult{}, toolError(err)
	}
	return res, nil
}

// globRegexp compiles a glob into a regular expression over a whole
// slash-separated path. "*" and "?" stay within one segment, "**" crosses
// segments, and "**/" also matches no directory at all, so "**/*.go" finds a
// file at the top as well.
func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("unterminated [ in %q", pattern)
			}
			class := pattern[i+1 : i+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end
		case '{':
			end := strings.IndexByte(pattern[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unterminated { in %q", pattern)
			}
			alts := strings.Split(pattern[i+1:i+end], ",")
			for j, alt := range alts {
				alts[j] = regexp.QuoteMeta(alt)
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", pattern, err)
	}
	return re, nil
}

type grepArgs struct {
	Pattern string `json:"pattern" jsonschema:"regular expression (RE2 syntax)"`
	Path    string `json:"path,omitempty" jsonschema:"file or directory to search, relative to the repository root (default: the root)"`
	Include string `json:"include,omitempty" jsonschema:"glob the file paths must match, e.g. **/*.go"`
}

type grepResult struct {
	Matches   []string `json:"matches"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (w *workspace) grep(ctx agent.Context, args grepArgs) (grepResult, error) {
	return w.grepIn(ctx, args)
}

// grepIn is grep for any context: a search of a large checkout is the one
// tool that runs long enough to be worth stopping when the job is.
func (w *workspace) grepIn(ctx context.Context, args grepArgs) (grepResult, error) {
	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return grepResult{}, fmt.Errorf("invalid pattern: %w", err)
	}
	var include *regexp.Regexp
	if args.Include != "" {
		if include, err = globRegexp(clean(args.Include)); err != nil {
			return grepResult{}, err
		}
	}

	res := grepResult{Matches: []string{}}
	fsys := w.root.FS()
	err = fs.WalkDir(fsys, clean(args.Path), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// The starting point itself is the caller's mistake and worth
			// reporting; anything below it is skipped.
			if p == clean(args.Path) {
				return err
			}
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || (include != nil && !include.MatchString(p)) {
			return nil
		}
		done, err := grepFile(fsys, p, re, &res)
		if err != nil {
			return nil
		}
		if done {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return grepResult{}, toolError(err)
	}
	return res, nil
}

// grepFile appends the matches in one file and reports whether the result is
// full.
func grepFile(fsys fs.FS, p string, re *regexp.Regexp, res *grepResult) (bool, error) {
	f, err := fsys.Open(p)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil || info.Size() > maxGrepFileBytes {
		return false, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false, err
	}
	if bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0 {
		return false, nil
	}
	for n, line := range strings.Split(string(data), "\n") {
		if !re.MatchString(line) {
			continue
		}
		if len(res.Matches) == maxGrepMatches {
			res.Truncated = true
			return true, nil
		}
		if len(line) > maxLineBytes {
			line = line[:maxLineBytes] + "…"
		}
		res.Matches = append(res.Matches, fmt.Sprintf("%s:%d: %s", p, n+1, line))
	}
	return false, nil
}

type writeArgs struct {
	Path    string `json:"path" jsonschema:"file path relative to the repository root"`
	Content string `json:"content" jsonschema:"the whole content of the file"`
}

type writeResult struct {
	Written int `json:"bytes_written"`
}

func (w *workspace) write(_ agent.Context, args writeArgs) (writeResult, error) {
	name := clean(args.Path)
	if w.writable == nil || !w.writable(name) {
		return writeResult{}, fmt.Errorf("writing %s is not allowed", args.Path)
	}
	if len(args.Content) > maxWriteBytes {
		return writeResult{}, fmt.Errorf("content is larger than %d bytes", maxWriteBytes)
	}
	if dir := path.Dir(name); dir != "." {
		if err := w.root.MkdirAll(dir, 0o700); err != nil {
			return writeResult{}, toolError(err)
		}
	}
	if err := w.root.WriteFile(name, []byte(args.Content), 0o600); err != nil {
		return writeResult{}, toolError(err)
	}
	return writeResult{Written: len(args.Content)}, nil
}

// toolError rewrites a filesystem error for the model. The message os.Root
// gives for an escape names the whole host path of the checkout, which is
// none of the model's business; what it needs to know is that it may not go
// there.
func toolError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%s does not exist", pe.Path)
		case strings.Contains(pe.Err.Error(), "escapes from parent"),
			strings.Contains(pe.Err.Error(), "path escapes"):
			return fmt.Errorf("%s is outside the repository", pe.Path)
		}
		return fmt.Errorf("%s: %w", pe.Path, pe.Err)
	}
	return err
}

// tools builds the workspace tools for a mode. The write tool is given only
// to a mode that writes, rather than given everywhere and refused: a tool the
// model does not have is one it cannot be talked into calling.
func (w *workspace) tools() ([]tool.Tool, error) {
	var out []tool.Tool
	add := func(t tool.Tool, err error) error {
		if err != nil {
			return err
		}
		out = append(out, t)
		return nil
	}
	if err := errors.Join(
		add(functiontool.New(functiontool.Config{
			Name:        ReadFileTool,
			Description: "Read a text file in the repository. Lines are returned numbered. Use offset and limit to page through a long file.",
		}, w.read)),
		add(functiontool.New(functiontool.Config{
			Name:        ListDirTool,
			Description: "List a directory in the repository. Directories end with /.",
		}, w.list)),
		add(functiontool.New(functiontool.Config{
			Name:        GlobTool,
			Description: "Find files in the repository whose path matches a glob.",
		}, w.glob)),
		add(functiontool.New(functiontool.Config{
			Name:        GrepTool,
			Description: "Search file contents in the repository with a regular expression. Returns path:line: text.",
		}, w.grep)),
	); err != nil {
		return nil, err
	}
	if w.writable != nil {
		if err := add(functiontool.New(functiontool.Config{
			Name:        WriteFileTool,
			Description: "Write a file. Only the output file named in the request may be written.",
		}, w.write)); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

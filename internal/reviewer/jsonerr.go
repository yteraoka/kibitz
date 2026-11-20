package reviewer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// describeJSONError turns a decoding error into one the agent can act on.
//
// encoding/json reports a byte offset and nothing else, and "invalid character
// '"' after object key:value pair" alone does not tell the agent which of its
// strings held the unescaped quote. The retry only helps if the feedback says
// where to look, so this adds the line, the column and the line itself with a
// marker under the offending character.
func describeJSONError(data []byte, err error) string {
	var offset int64 = -1
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr):
		// Offset counts the bytes read, the offending one included.
		offset = syntaxErr.Offset - 1
	case errors.As(err, &typeErr):
		offset = typeErr.Offset - 1
	}

	msg := err.Error()
	if offset < 0 || int(offset) >= len(data) {
		return msg
	}

	line := bytes.Count(data[:offset], []byte("\n")) + 1
	start := bytes.LastIndexByte(data[:offset], '\n') + 1
	end := bytes.IndexByte(data[offset:], '\n')
	if end < 0 {
		end = len(data)
	} else {
		end += int(offset)
	}
	column := utf8.RuneCount(data[start:offset]) + 1

	// Keep the excerpt short: a minified document is one very long line.
	const context = 80
	from, to := start, end
	prefix, suffix := "", ""
	if int(offset)-from > context {
		from = int(offset) - context
		for from < int(offset) && !utf8.RuneStart(data[from]) {
			from++
		}
		prefix = "…"
	}
	if to-int(offset) > context {
		to = int(offset) + context
		for to > int(offset) && to < len(data) && !utf8.RuneStart(data[to]) {
			to--
		}
		suffix = "…"
	}
	excerpt := prefix + string(data[from:to]) + suffix
	marker := strings.Repeat(" ", utf8.RuneCountInString(prefix)+utf8.RuneCount(data[from:offset])) + "^"

	hint := ""
	if syntaxErr != nil {
		hint = "\nhint: a double quote, backslash or newline inside a JSON string must be escaped as \\\", \\\\ or \\n; elements and members are separated by commas"
	}
	return fmt.Sprintf("%s (line %d, column %d)\n%s\n%s%s", msg, line, column, excerpt, marker, hint)
}

package tasks

import (
	"errors"
	"strings"
)

// Split splits a command line into words, honouring single and double quotes
// and backslash escapes, so `implement -test "go test ./..."` stays three words.
func Split(s string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped, inWord = true, true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("unterminated quote or escape")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// Join is the inverse of Split, for display.
func Join(words []string) string {
	out := make([]string, len(words))
	for i, w := range words {
		if w == "" || strings.ContainsAny(w, " \t\n\"'\\") {
			w = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w) + `"`
		}
		out[i] = w
	}
	return strings.Join(out, " ")
}

// Parse turns a command line such as `review owner/repo 12` into a task kind and its args.
func Parse(line string) (kind string, args []string, err error) {
	words, err := Split(line)
	if err != nil {
		return "", nil, err
	}
	if len(words) > 0 && words[0] == "devloop" {
		words = words[1:]
	}
	if len(words) == 0 {
		return "", nil, errors.New("empty command")
	}
	if !validKind(words[0]) {
		return "", nil, errors.New("unknown command " + words[0] + "; use one of: " + strings.Join(Kinds, ", "))
	}
	return words[0], words[1:], nil
}

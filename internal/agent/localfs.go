package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Read-only tools over a local checkout, confined to its root directory.

const (
	maxFileBytes   = 200_000
	maxGrepMatches = 200
	maxListEntries = 500
)

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true}

type localFS struct{ root string }

// resolve maps a repo-relative path to an absolute one, refusing anything outside root.
func (l localFS) resolve(rel string) (string, error) {
	p := filepath.Join(l.root, filepath.Clean("/"+rel))
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if real != l.root && !strings.HasPrefix(real, l.root+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the repository", rel)
	}
	return real, nil
}

type readInput struct {
	Path string `json:"path" jsonschema:"required,description=Path relative to the repository root"`
}
type grepInput struct {
	Pattern string `json:"pattern" jsonschema:"required,description=Go regular expression"`
	Path    string `json:"path,omitempty" jsonschema:"description=Directory or file to search, relative to the root (default: whole repo)"`
}
type globInput struct {
	Pattern string `json:"pattern" jsonschema:"required,description=Glob matched against the repo-relative path, e.g. **/*.go or src/*.js"`
}

func localTools(root string, verbose bool) ([]anthropic.BetaTool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if abs, err = filepath.EvalSymlinks(abs); err != nil {
		return nil, err
	}
	l := localFS{root: abs}

	return []anthropic.BetaTool{
		newTool(verbose, "read_local_file", "Read a file from the local checkout (may not be on the PR's head commit).",
			func(_ context.Context, in readInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				p, err := l.resolve(in.Path)
				if err != nil {
					return text(""), err
				}
				b, err := os.ReadFile(p)
				if err != nil {
					return text(""), err
				}
				if len(b) > maxFileBytes {
					return text(string(b[:maxFileBytes]) + "\n[truncated]"), nil
				}
				return text(string(b)), nil
			}),
		newTool(verbose, "grep_local", "Search file contents in the local checkout with a regular expression. Returns path:line: text.",
			func(_ context.Context, in grepInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				re, err := regexp.Compile(in.Pattern)
				if err != nil {
					return text(""), err
				}
				start, err := l.resolve(in.Path)
				if err != nil {
					return text(""), err
				}
				var out []string
				errStop := errors.New("stop")
				err = filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
					if err != nil {
						return nil
					}
					if d.IsDir() {
						if skipDirs[d.Name()] {
							return filepath.SkipDir
						}
						return nil
					}
					f, err := os.Open(p)
					if err != nil {
						return nil
					}
					defer f.Close()
					rel, _ := filepath.Rel(l.root, p)
					sc := bufio.NewScanner(f)
					for n := 1; sc.Scan(); n++ {
						if re.MatchString(sc.Text()) {
							out = append(out, fmt.Sprintf("%s:%d: %.300s", rel, n, sc.Text()))
							if len(out) >= maxGrepMatches {
								return errStop
							}
						}
					}
					return nil
				})
				if err != nil && !errors.Is(err, errStop) {
					return text(""), err
				}
				if len(out) == 0 {
					return text("no matches"), nil
				}
				return text(strings.Join(out, "\n")), nil
			}),
		newTool(verbose, "list_local_files", "List files in the local checkout whose repo-relative path matches a glob (** matches any number of directories).",
			func(_ context.Context, in globInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				re, err := globToRegexp(in.Pattern)
				if err != nil {
					return text(""), err
				}
				var out []string
				_ = filepath.WalkDir(l.root, func(p string, d fs.DirEntry, err error) error {
					if err != nil || len(out) >= maxListEntries {
						return filepath.SkipDir
					}
					if d.IsDir() {
						if skipDirs[d.Name()] {
							return filepath.SkipDir
						}
						return nil
					}
					rel, _ := filepath.Rel(l.root, p)
					if re.MatchString(filepath.ToSlash(rel)) {
						out = append(out, rel)
					}
					return nil
				})
				if len(out) == 0 {
					return text("no files"), nil
				}
				return text(strings.Join(out, "\n")), nil
			}),
	}, nil
}

// globToRegexp converts a glob with *, ** and ? into an anchored regular expression.
func globToRegexp(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				if i+1 < len(glob) && glob[i+1] == '/' {
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
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

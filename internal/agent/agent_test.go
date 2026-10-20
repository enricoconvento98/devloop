package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/enricoconvento98/devloop/internal/ghclient"
)

type fakeGitHub struct{ ghclient.API }

func (fakeGitHub) GetPR(_ context.Context, n int) (*ghclient.PRDetail, error) {
	return &ghclient.PRDetail{Number: n, Title: "Add discount helper", HeadSHA: "abc123",
		Files: []ghclient.PRFile{{Filename: "src/discount.js", Status: "added", Patch: "+return price - price * percent;"}}}, nil
}

// scriptedAPI fakes the Messages API: it replays responses in order and records requests.
func scriptedAPI(t *testing.T, responses ...string) (*httptest.Server, *[]map[string]any) {
	var mu sync.Mutex
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(body, &req)
		requests = append(requests, req)
		if len(requests) > len(responses) {
			t.Errorf("unexpected request #%d", len(requests))
			http.Error(w, "no more responses", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, responses[len(requests)-1])
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func msg(stop string, content string) string {
	return `{"id":"msg","type":"message","role":"assistant","model":"m","stop_reason":"` + stop +
		`","usage":{"input_tokens":10,"output_tokens":5},"content":[` + content + `]}`
}

func TestRunReviewLoop(t *testing.T) {
	srv, requests := scriptedAPI(t,
		msg("tool_use", `{"type":"tool_use","id":"tu1","name":"get_pr","input":{"number":7}}`),
		msg("tool_use", `{"type":"tool_use","id":"tu2","name":"submit_review","input":{"summary":"Bug.","verdict":"request_changes","findings":[{"severity":"blocker","file":"src/discount.js","line":3,"title":"Percent not divided by 100","body":"applyDiscount(50,10) = -450"}]}}`),
		msg("end_turn", `{"type":"text","text":"Done."}`),
	)
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	opts := Options{Model: "claude-opus-5-5", Effort: "high", MaxTurns: 10}

	review, stats, err := RunReview(context.Background(), client, fakeGitHub{}, ghclient.Repo{Owner: "acme", Name: "shop"}, 7, opts)
	if err != nil {
		t.Fatal(err)
	}
	if review.Verdict != "request_changes" || len(review.Findings) != 1 || review.Findings[0].Line != 3 {
		t.Fatalf("unexpected review: %+v", review)
	}
	if stats.Turns != 3 || stats.InputTokens != 30 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	first := (*requests)[0]
	if first["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v, want default", first["fallbacks"])
	}
	if eff := first["output_config"].(map[string]any)["effort"]; eff != "high" {
		t.Errorf("effort = %v", eff)
	}
	names := map[string]bool{}
	for _, tl := range first["tools"].([]any) {
		names[tl.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"get_pr", "get_file", "submit_review"} {
		if !names[want] {
			t.Errorf("tool %s not offered", want)
		}
	}
	if names["read_local_file"] {
		t.Error("local tools offered without -repo-dir")
	}
	// The second request must carry get_pr's result back to the model.
	second, _ := json.Marshal((*requests)[1]["messages"])
	if !strings.Contains(string(second), "Add discount helper") {
		t.Error("get_pr result not sent back to the model")
	}
}

func TestRunReviewWithoutSubmit(t *testing.T) {
	srv, _ := scriptedAPI(t, msg("end_turn", `{"type":"text","text":"Looks fine."}`))
	client := anthropic.NewClient(option.WithBaseURL(srv.URL), option.WithAPIKey("test"), option.WithMaxRetries(0))
	_, _, err := RunReview(context.Background(), client, fakeGitHub{}, ghclient.Repo{Owner: "a", Name: "b"}, 1,
		Options{Model: "m", Effort: "high", MaxTurns: 5})
	if err != ErrNoResult {
		t.Fatalf("err = %v, want ErrNoResult", err)
	}
}

func TestSubmitSchemaHasEnums(t *testing.T) {
	tool, _ := submitTool[Review](false, "submit_review", "")
	schema, _ := json.Marshal(tool.InputSchema())
	for _, want := range []string{`"request_changes"`, `"blocker"`, `"nit"`} {
		if !strings.Contains(string(schema), want) {
			t.Errorf("schema missing %s: %s", want, schema)
		}
	}
}

func TestLocalFSConfinement(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "src"), 0o755)
	os.WriteFile(filepath.Join(root, "src", "a.go"), []byte("package a\nfunc Hello() {}\n"), 0o644)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link"))

	l := localFS{root: mustEval(t, root)}
	if _, err := l.resolve("src/a.go"); err != nil {
		t.Errorf("in-repo path refused: %v", err)
	}
	if p, err := l.resolve("../../src/a.go"); err != nil || !strings.HasPrefix(p, l.root) {
		t.Errorf("traversal not clamped to root: %q, %v", p, err)
	}
	if _, err := l.resolve("link"); err == nil {
		t.Error("symlink out of the repo was allowed")
	}
}

func mustEval(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"**/*.go", "main.go", true},
		{"**/*.go", "internal/agent/agent.go", true},
		{"src/*.js", "src/a.js", true},
		{"src/*.js", "src/lib/a.js", false},
		{"*.md", "docs/a.md", false},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.glob)
		if err != nil {
			t.Fatal(err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%s ~ %s = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

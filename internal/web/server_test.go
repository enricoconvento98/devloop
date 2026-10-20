package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/enricoconvento98/devloop/internal/tasks"
)

func setup(t *testing.T) (*tasks.Store, *httptest.Server, string) {
	t.Helper()
	store, err := tasks.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv := httptest.NewServer(New(store, "/work").Handler())
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var page strings.Builder
	buf := make([]byte, 64<<10)
	for {
		n, err := res.Body.Read(buf)
		page.Write(buf[:n])
		if err != nil {
			break
		}
	}
	m := regexp.MustCompile(`TOKEN = "([0-9a-f]{32})"`).FindStringSubmatch(page.String())
	if m == nil {
		t.Fatal("token not found in page")
	}
	return store, srv, m[1]
}

func call(t *testing.T, method, url, token, body string, host ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("X-Devloop-Token", token)
	}
	if len(host) > 0 {
		req.Host = host[0]
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestAPIRequiresToken(t *testing.T) {
	_, srv, token := setup(t)
	if res := call(t, "GET", srv.URL+"/api/tasks", "", ""); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token: %d", res.StatusCode)
	}
	if res := call(t, "POST", srv.URL+"/api/tasks", "deadbeef", `{"command":"triage a/b"}`); res.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", res.StatusCode)
	}
	if res := call(t, "GET", srv.URL+"/api/tasks", token, ""); res.StatusCode != http.StatusOK {
		t.Errorf("right token: %d", res.StatusCode)
	}
}

func TestRejectsForeignHost(t *testing.T) {
	_, srv, token := setup(t)
	for _, host := range []string{"evil.example", "evil.example:7777", "10.0.0.5"} {
		if res := call(t, "GET", srv.URL+"/api/tasks", token, "", host); res.StatusCode != http.StatusForbidden {
			t.Errorf("host %s: %d, want 403", host, res.StatusCode)
		}
	}
	if res := call(t, "GET", srv.URL+"/", "", "", "localhost:7777"); res.StatusCode != http.StatusOK {
		t.Errorf("localhost: %d", res.StatusCode)
	}
}

func TestAddCancelRetry(t *testing.T) {
	store, srv, token := setup(t)
	res := call(t, "POST", srv.URL+"/api/tasks", token, `{"command":"implement -plan p.json -test \"go test ./...\""}`)
	var added struct{ ID int64 }
	json.NewDecoder(res.Body).Decode(&added)
	got, err := store.Get(context.Background(), added.ID)
	if err != nil || got.Kind != "implement" || got.Args[3] != "go test ./..." || got.Dir != "/work" {
		t.Fatalf("added task = %+v, %v", got, err)
	}
	if res := call(t, "POST", srv.URL+"/api/tasks", token, `{"command":"sh -c id"}`); res.StatusCode != http.StatusBadRequest {
		t.Errorf("arbitrary command: %d", res.StatusCode)
	}
	if res := call(t, "POST", srv.URL+"/api/tasks/1/cancel", token, ""); res.StatusCode != http.StatusOK {
		t.Errorf("cancel: %d", res.StatusCode)
	}
	if res := call(t, "POST", srv.URL+"/api/tasks/1/retry", token, ""); res.StatusCode != http.StatusOK {
		t.Errorf("retry: %d", res.StatusCode)
	}
	if res := call(t, "GET", srv.URL+"/api/tasks/99/log", token, ""); res.StatusCode != http.StatusNotFound {
		t.Errorf("missing task log: %d", res.StatusCode)
	}
	list, _ := store.List(context.Background(), 10)
	if len(list) != 2 || list[1].Status != tasks.Canceled || list[0].Status != tasks.Queued {
		t.Errorf("tasks = %+v", list)
	}
}

// Package web serves devloop's local task dashboard.
package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/enricoconvento98/devloop/internal/tasks"
)

//go:embed index.html
var indexHTML string

var indexTmpl = template.Must(template.New("index").Parse(indexHTML))

const maxLogBytes = 256 << 10

type Server struct {
	store *tasks.Store
	token string
	dir   string // where queued tasks run
}

// New returns a dashboard over store; tasks queued from it run in dir.
// Every API call must carry the per-run token, which only the dashboard page
// knows, so other websites can't drive it.
func New(store *tasks.Store, dir string) *Server {
	b := make([]byte, 16)
	rand.Read(b)
	return &Server{store: store, token: hex.EncodeToString(b), dir: dir}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/tasks", s.api(s.list))
	mux.HandleFunc("POST /api/tasks", s.api(s.add))
	mux.HandleFunc("GET /api/tasks/{id}/log", s.api(s.log))
	mux.HandleFunc("POST /api/tasks/{id}/cancel", s.api(s.cancel))
	mux.HandleFunc("POST /api/tasks/{id}/retry", s.api(s.retry))
	return localOnly(mux)
}

// localOnly rejects requests whose Host isn't loopback, which blocks DNS-rebinding attacks.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "devloop only serves localhost", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
	indexTmpl.Execute(w, map[string]any{"Token": s.token, "Kinds": tasks.Kinds, "Dir": s.dir})
}

type apiFunc func(r *http.Request) (any, error)

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func (s *Server) api(fn apiFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Devloop-Token")), []byte(s.token)) != 1 {
			http.Error(w, "missing or wrong token; reload the page", http.StatusUnauthorized)
			return
		}
		v, err := fn(r)
		if err != nil {
			code := http.StatusBadRequest
			var he httpError
			switch {
			case errors.As(err, &he):
				code = he.code
			case errors.Is(err, tasks.ErrNotFound):
				code = http.StatusNotFound
			}
			http.Error(w, err.Error(), code)
			return
		}
		if text, ok := v.(string); ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			io.WriteString(w, text)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
}

func taskID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return 0, httpError{http.StatusBadRequest, "bad task id"}
	}
	return id, nil
}

func (s *Server) list(r *http.Request) (any, error) {
	list, err := s.store.List(r.Context(), 200)
	if list == nil {
		list = []tasks.Task{}
	}
	return list, err
}

func (s *Server) add(r *http.Request) (any, error) {
	var req struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		return nil, err
	}
	kind, args, err := tasks.Parse(req.Command)
	if err != nil {
		return nil, err
	}
	id, err := s.store.Add(r.Context(), kind, args, s.dir)
	return map[string]int64{"id": id}, err
}

func (s *Server) log(r *http.Request) (any, error) {
	id, err := taskID(r)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.Get(r.Context(), id); err != nil {
		return nil, err
	}
	return readTail(s.store.LogPath(id), maxLogBytes)
}

func (s *Server) cancel(r *http.Request) (any, error) {
	id, err := taskID(r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.store.Cancel(r.Context(), id)
}

func (s *Server) retry(r *http.Request) (any, error) {
	id, err := taskID(r)
	if err != nil {
		return nil, err
	}
	newID, err := s.store.Retry(r.Context(), id)
	return map[string]int64{"id": newID}, err
}

// readTail returns at most n bytes from the end of a file ("" if it doesn't exist yet).
func readTail(path string, n int64) (string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := max(info.Size()-n, 0)
	b := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	text := string(b)
	if start > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = "…\n" + text[i+1:]
		}
	}
	return text, nil
}

// ListenAndServe serves until ctx is canceled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

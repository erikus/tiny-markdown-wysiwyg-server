package server

import (
	"bytes"
	"encoding/json"
	"html/template"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/erikus/tiny-markdown-wysiwyg-server/internal/store"
)

const (
	testHost      = "localhost:8585"
	testRootName  = "notes"
	testIndexHTML = `<!doctype html><title>t</title><meta name="mdwebd-view-base" content="{{.ViewBase}}"><script src="/static/app.js"></script>`
	testViewBase  = `https://files.example.com/"><script>`
)

type fixture struct {
	h    http.Handler
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	st, err := store.New(store.Config{
		Roots:      []store.Root{{Name: testRootName, Dir: root}},
		Extensions: []string{".md"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assets := fstest.MapFS{
		"index.html": {Data: []byte(testIndexHTML)},
		"app.js":     {Data: []byte("// js")},
	}
	h, err := New(Config{
		Store:         st,
		AllowedHosts:  []string{"Edit.Example.TS.net"},
		ViewBase:      testViewBase,
		Assets:        assets,
		IndexTemplate: template.Must(template.New("index.html").Parse(testIndexHTML)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{h: h, root: root}
}

func (f *fixture) write(t *testing.T, name string, data []byte) {
	t.Helper()
	p := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// do runs a request through the handler. Host defaults to testHost.
func (f *fixture) do(method string, target string, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://"+testHost+target, strings.NewReader(body))
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); ct != contentTypeJSON {
		t.Fatalf("content type %q, body %q", ct, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid JSON %q: %v", rec.Body.String(), err)
	}
	return m
}

func writeHeaders(extra map[string]string) map[string]string {
	h := map[string]string{writeHeader: writeHeaderValue, "Content-Type": "application/json"}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

func putBody(path string, content string, version string) string {
	b, _ := json.Marshal(map[string]string{"path": path, "content": content, "version": version})
	return string(b)
}

func TestHostCheck(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		host string
		want int
	}{
		{"localhost:8585", http.StatusOK},
		{"localhost", http.StatusOK},
		{"127.0.0.1:8585", http.StatusOK},
		{"[::1]:8585", http.StatusOK},
		{"edit.example.ts.net", http.StatusOK},
		{"EDIT.example.ts.net:443", http.StatusOK},
		{"evil.example.com", http.StatusBadRequest},
		{"localhost.evil.com", http.StatusBadRequest},
		{"", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			rec := f.do(http.MethodGet, "/", "", map[string]string{"Host": tc.host})
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want != http.StatusOK {
				if m := decodeJSON(t, rec); m["error"] == "" {
					t.Fatal("missing error message")
				}
			}
		})
	}
}

func TestWriteGuard(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", []byte("hello\n"))
	version := decodeJSON(t, f.do(http.MethodGet, "/api/file?path=notes/a.md", "", nil))["version"].(string)
	body := putBody("notes/a.md", "changed\n", version)

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"no custom header", map[string]string{"Content-Type": "application/json"}, http.StatusForbidden},
		{"wrong custom header value", map[string]string{writeHeader: "yes"}, http.StatusForbidden},
		{"bad origin", writeHeaders(map[string]string{"Origin": "http://evil.example.com"}), http.StatusForbidden},
		{"null origin", writeHeaders(map[string]string{"Origin": "null"}), http.StatusForbidden},
		{"https origin on http server", writeHeaders(map[string]string{"Origin": "https://localhost:8585"}), http.StatusForbidden},
		{"origin with wrong port", writeHeaders(map[string]string{"Origin": "http://localhost:9999"}), http.StatusForbidden},
		{"bad sec-fetch-site", writeHeaders(map[string]string{"Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		{"same-site sec-fetch-site", writeHeaders(map[string]string{"Sec-Fetch-Site": "same-site"}), http.StatusForbidden},
		{"good origin", writeHeaders(map[string]string{"Origin": "http://localhost:8585"}), http.StatusOK},
		{"good origin different case", writeHeaders(map[string]string{"Origin": "HTTP://LOCALHOST:8585"}), http.StatusOK},
		{"same-origin", writeHeaders(map[string]string{"Sec-Fetch-Site": "same-origin"}), http.StatusOK},
		{"sec-fetch-site none", writeHeaders(map[string]string{"Sec-Fetch-Site": "none"}), http.StatusOK},
		{"no origin headers", writeHeaders(nil), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.write(t, "a.md", []byte("hello\n"))
			rec := f.do(http.MethodPut, "/api/file", body, tc.headers)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			decodeJSON(t, rec)
		})
	}
}

func TestPagesAndHeaders(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", []byte("hello\n"))

	t.Run("edit page", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/edit/notes/does-not-need-to-exist.md", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d", rec.Code)
		}
		h := rec.Header()
		if h.Get("Content-Security-Policy") != cspPage {
			t.Fatalf("CSP %q", h.Get("Content-Security-Policy"))
		}
		if h.Get("Content-Type") != contentTypeHTML || h.Get("Cache-Control") != cacheNoStore ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("headers %v", h)
		}
		body := rec.Body.String()
		if strings.Contains(body, "<script>") && !strings.Contains(body, "&lt;script&gt;") {
			t.Fatalf("view base not escaped: %s", body)
		}
		if !strings.Contains(body, `content="https://files.example.com/&#34;&gt;&lt;script&gt;"`) {
			t.Fatalf("view base missing or wrongly escaped: %s", body)
		}
	})
	t.Run("root page", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/", "", nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Security-Policy") != cspPage {
			t.Fatalf("got %d, CSP %q", rec.Code, rec.Header().Get("Content-Security-Policy"))
		}
		if !strings.Contains(rec.Body.String(), "<li>notes</li>") {
			t.Fatalf("root names missing: %s", rec.Body.String())
		}
	})
	t.Run("api no-store", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/api/file?path=notes/a.md", "", nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != cacheNoStore {
			t.Fatalf("got %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
		}
	})
	t.Run("static cacheable", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/static/app.js", "", nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "" {
			t.Fatalf("got %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("nosniff missing on static")
		}
	})
	t.Run("static listing refused", func(t *testing.T) {
		if rec := f.do(http.MethodGet, "/static/", "", nil); rec.Code != http.StatusNotFound {
			t.Fatalf("got %d", rec.Code)
		}
	})
	t.Run("unknown path is JSON 404", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/nope", "", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("got %d", rec.Code)
		}
		decodeJSON(t, rec)
	})
}

func TestReadAndWrite(t *testing.T) {
	f := newFixture(t)
	f.write(t, "a.md", []byte("\xEF\xBB\xBFhello\r\nworld\r\n"))

	rec := f.do(http.MethodGet, "/api/file?path=notes/a.md", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("read: %d %s", rec.Code, rec.Body.String())
	}
	m := decodeJSON(t, rec)
	if m["content"] != "hello\nworld\n" || m["path"] != "notes/a.md" {
		t.Fatalf("read body %v", m)
	}
	norm := m["normalized"].(map[string]any)
	if norm["crlf"] != true || norm["bom"] != true {
		t.Fatalf("normalized %v", norm)
	}
	version := m["version"].(string)

	rec = f.do(http.MethodPut, "/api/file", putBody("notes/a.md", "bye\n", version), writeHeaders(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("write: %d %s", rec.Code, rec.Body.String())
	}
	wm := decodeJSON(t, rec)
	if wm["path"] != "notes/a.md" || wm["version"] == version || wm["size"].(float64) != float64(len("\xEF\xBB\xBFbye\r\n")) {
		t.Fatalf("write body %v", wm)
	}
	got, err := os.ReadFile(filepath.Join(f.root, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "\xEF\xBB\xBFbye\r\n" {
		t.Fatalf("on disk %q", got)
	}

	t.Run("conflict body includes current version", func(t *testing.T) {
		rec := f.do(http.MethodPut, "/api/file", putBody("notes/a.md", "again\n", version), writeHeaders(nil))
		if rec.Code != http.StatusConflict {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
		cm := decodeJSON(t, rec)
		if cm["version"] != wm["version"] || cm["error"] == "" {
			t.Fatalf("conflict body %v", cm)
		}
	})
	t.Run("status mapping", func(t *testing.T) {
		f.write(t, "bin.md", []byte("\xff\xfe"))
		f.write(t, "x.txt", []byte("text"))
		cases := []struct {
			name string
			path string
			want int
		}{
			{"missing", "notes/nope.md", http.StatusNotFound},
			{"traversal", "notes/../a.md", http.StatusBadRequest},
			{"git", "notes/.git/a.md", http.StatusForbidden},
			{"extension", "notes/x.txt", http.StatusForbidden},
			{"not text", "notes/bin.md", http.StatusUnsupportedMediaType},
			{"unknown root", "other/a.md", http.StatusNotFound},
		}
		for _, tc := range cases {
			rec := f.do(http.MethodGet, "/api/file?path="+tc.path, "", nil)
			if rec.Code != tc.want {
				t.Errorf("%s: got %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			decodeJSON(t, rec)
		}
	})
	t.Run("bad write bodies", func(t *testing.T) {
		cases := []struct {
			name string
			body string
			want int
		}{
			{"not json", "nope", http.StatusBadRequest},
			{"missing path", `{"content":"x","version":"v"}`, http.StatusBadRequest},
			{"missing content", `{"path":"notes/a.md","version":"v"}`, http.StatusBadRequest},
			{"missing version", `{"path":"notes/a.md","content":"x"}`, http.StatusBadRequest},
			{"unknown field", `{"path":"notes/a.md","content":"x","version":"v","extra":1}`, http.StatusBadRequest},
			{"missing path query", "", http.StatusBadRequest},
		}
		for _, tc := range cases {
			rec := f.do(http.MethodPut, "/api/file", tc.body, writeHeaders(nil))
			if rec.Code != tc.want {
				t.Errorf("%s: got %d, want %d (%s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
			decodeJSON(t, rec)
		}
		if rec := f.do(http.MethodGet, "/api/file", "", nil); rec.Code != http.StatusBadRequest {
			t.Errorf("GET without path: got %d", rec.Code)
		}
	})
	t.Run("oversized body", func(t *testing.T) {
		big := strings.Repeat("x", int(store.DefaultMaxBytes*bodyMultiplier+bodySlackBytes)+1)
		rec := f.do(http.MethodPut, "/api/file", putBody("notes/a.md", big, "v"), writeHeaders(nil))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
		decodeJSON(t, rec)
	})
}

func TestImage(t *testing.T) {
	f := newFixture(t)
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	f.write(t, "sub/pic.png", buf.Bytes())
	f.write(t, "sub/notes.md", []byte("# hi\n"))
	f.write(t, "sub/fake.png", []byte("just text\n"))

	t.Run("png served", func(t *testing.T) {
		rec := f.do(http.MethodGet, "/img/notes/sub/pic.png", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d %s", rec.Code, rec.Body.String())
		}
		h := rec.Header()
		if h.Get("Content-Type") != "image/png" || h.Get("Content-Security-Policy") != cspImage ||
			h.Get("Content-Disposition") != "inline" || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Cache-Control") != cacheNoStore {
			t.Fatalf("headers %v", h)
		}
		if !bytes.Equal(rec.Body.Bytes(), buf.Bytes()) {
			t.Fatal("body mismatch")
		}
	})
	cases := []struct {
		name string
		path string
		want int
	}{
		{"markdown refused", "/img/notes/sub/notes.md", http.StatusUnsupportedMediaType},
		{"text with png extension refused", "/img/notes/sub/fake.png", http.StatusUnsupportedMediaType},
		{"missing", "/img/notes/sub/nope.png", http.StatusNotFound},
		{"traversal", "/img/notes/sub/%2e%2e/pic.png", http.StatusBadRequest},
		{"git", "/img/notes/.git/pic.png", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodGet, tc.path, "", nil)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			decodeJSON(t, rec)
		})
	}
}

func TestHTTPSOriginCheck(t *testing.T) {
	root := t.TempDir()
	st, err := store.New(store.Config{Roots: []store.Root{{Name: testRootName, Dir: root}}, Extensions: []string{".md"}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		Store:         st,
		HTTPS:         true,
		AllowedHosts:  []string{"edit.example.ts.net"},
		Assets:        fstest.MapFS{},
		IndexTemplate: template.Must(template.New("index.html").Parse(testIndexHTML)),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{h: h, root: root}
	f.write(t, "a.md", []byte("x\n"))
	version := decodeJSON(t, f.do(http.MethodGet, "/api/file?path=notes/a.md", "", map[string]string{"Host": "edit.example.ts.net"}))["version"].(string)
	body := putBody("notes/a.md", "y\n", version)

	rec := f.do(http.MethodPut, "/api/file", body, writeHeaders(map[string]string{"Host": "edit.example.ts.net", "Origin": "http://edit.example.ts.net"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("http origin on https server: got %d", rec.Code)
	}
	rec = f.do(http.MethodPut, "/api/file", body, writeHeaders(map[string]string{"Host": "edit.example.ts.net", "Origin": "https://edit.example.ts.net"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("https origin: got %d %s", rec.Code, rec.Body.String())
	}
}

// Package server is the HTTP surface of mdwebd: routes, JSON errors and the
// security middleware described in DESIGN.md.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/erikus/tiny-markdown-wysiwyg-server/internal/store"
	"github.com/erikus/tiny-markdown-wysiwyg-server/internal/web"
)

const (
	// writeHeader must be present with writeHeaderValue on every non-safe
	// request. Browsers only send it after a successful CORS preflight, and
	// mdwebd never answers preflights.
	writeHeader      = "X-Mdwebd"
	writeHeaderValue = "1"

	// bodyMultiplier and bodySlackBytes bound the PUT body:
	// maxBytes*bodyMultiplier + bodySlackBytes (JSON escaping overhead).
	bodyMultiplier     int64 = 2
	bodySlackBytes     int64 = 64 * 1024
	schemeHTTP               = "http"
	schemeHTTPS              = "https"
	secFetchSameOrigin       = "same-origin"
	secFetchNone             = "none"

	// cspPage is the policy for the editor page and the root listing.
	cspPage = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'self'; " +
		"form-action 'none'; frame-ancestors 'none'; object-src 'none'"
	// cspImage is the policy for files served by /img/.
	cspImage = "sandbox; default-src 'none'"

	staticPrefix = "/static/"
	editPrefix   = "/edit/"
	imgPrefix    = "/img/"
	apiFilePath  = "/api/file"

	contentTypeJSON = "application/json; charset=utf-8"
	contentTypeHTML = "text/html; charset=utf-8"
	cacheNoStore    = "no-store"

	// rootListTemplate is the tiny page at GET /.
	rootListTemplate = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>mdwebd</title></head>
<body><h1>mdwebd</h1><p>Roots:</p><ul>
{{range .}}<li>{{.}}</li>
{{end}}</ul></body></html>
`
)

// alwaysAllowedHosts are accepted in the Host header regardless of config.
var alwaysAllowedHosts = []string{"localhost", "127.0.0.1", "::1"}

// Config configures the handler.
type Config struct {
	Store *store.Store
	// AllowedHosts are extra Host header values (lower-case, no port) to
	// accept in addition to alwaysAllowedHosts.
	AllowedHosts []string
	// HTTPS reports whether the server is reached over TLS (tsnet). It
	// selects the scheme used for the Origin check.
	HTTPS bool
	// ViewBase, if set, is exposed to the editor page via index.html.
	ViewBase string
	// Assets is the static file tree ("app.js" at its root). Nil means
	// web.Assets().
	Assets fs.FS
	// IndexTemplate renders the editor page. Nil means web.IndexTemplate.
	IndexTemplate *template.Template
}

type server struct {
	store        *store.Store
	allowedHosts map[string]bool
	scheme       string
	indexPage    []byte
	rootPage     []byte
	static       http.Handler
	maxBody      int64
}

// New builds the handler. Pages are rendered once, at construction.
func New(cfg Config) (http.Handler, error) {
	if cfg.Store == nil {
		return nil, errors.New("server: Store is required")
	}
	assets := cfg.Assets
	if assets == nil {
		var err error
		assets, err = web.Assets()
		if err != nil {
			return nil, fmt.Errorf("server: assets: %w", err)
		}
	}
	indexTmpl := cfg.IndexTemplate
	if indexTmpl == nil {
		indexTmpl = web.IndexTemplate
	}
	var index bytes.Buffer
	if err := indexTmpl.Execute(&index, web.IndexData{ViewBase: cfg.ViewBase}); err != nil {
		return nil, fmt.Errorf("server: render index.html: %w", err)
	}
	rootTmpl, err := template.New("root").Parse(rootListTemplate)
	if err != nil {
		return nil, fmt.Errorf("server: root template: %w", err)
	}
	var rootPage bytes.Buffer
	if err := rootTmpl.Execute(&rootPage, cfg.Store.RootNames()); err != nil {
		return nil, fmt.Errorf("server: render root page: %w", err)
	}

	s := &server{
		store:        cfg.Store,
		allowedHosts: make(map[string]bool, len(cfg.AllowedHosts)+len(alwaysAllowedHosts)),
		scheme:       schemeHTTP,
		indexPage:    index.Bytes(),
		rootPage:     rootPage.Bytes(),
		static:       http.StripPrefix(staticPrefix, http.FileServerFS(assets)),
		maxBody:      cfg.Store.MaxBytes()*bodyMultiplier + bodySlackBytes,
	}
	if cfg.HTTPS {
		s.scheme = schemeHTTPS
	}
	for _, h := range alwaysAllowedHosts {
		s.allowedHosts[h] = true
	}
	for _, h := range cfg.AllowedHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return nil, errors.New("server: empty allowed host")
		}
		s.allowedHosts[h] = true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET "+editPrefix+"{path...}", s.handleEdit)
	mux.HandleFunc("GET "+apiFilePath, s.handleRead)
	mux.HandleFunc("PUT "+apiFilePath, s.handleWrite)
	mux.HandleFunc("GET "+imgPrefix+"{path...}", s.handleImage)
	mux.HandleFunc("GET "+staticPrefix, s.handleStatic)
	mux.HandleFunc("/", s.handleNotFound)

	return s.hostCheck(s.writeGuard(s.commonHeaders(mux))), nil
}

// --- middleware ---

// hostCheck rejects requests whose Host header is not in the allowed set
// (DNS-rebinding defence).
func (s *server) hostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostWithoutPort(r.Host)
		if host == "" || !s.allowedHosts[host] {
			writeError(w, http.StatusBadRequest, "unknown host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostWithoutPort lower-cases a Host header value and strips any port and
// IPv6 brackets.
func hostWithoutPort(hostport string) string {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	return strings.ToLower(host)
}

// writeGuard enforces the custom header and Origin / Sec-Fetch-Site rules
// on every non-safe request.
func (s *server) writeGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get(writeHeader) != writeHeaderValue {
			writeError(w, http.StatusForbidden, "missing "+writeHeader+" header")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !s.sameOrigin(origin, r.Host) {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != secFetchSameOrigin && site != secFetchNone {
			writeError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin reports whether origin equals scheme://host for this server.
func (s *server) sameOrigin(origin string, host string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Scheme, s.scheme) && strings.EqualFold(u.Host, host) &&
		u.Path == "" && u.RawQuery == "" && u.User == nil
}

// commonHeaders sets the headers every response carries.
func (s *server) commonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, staticPrefix) {
			h.Set("Cache-Control", cacheNoStore)
		}
		next.ServeHTTP(w, r)
	})
}

// --- handlers ---

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	servePage(w, s.rootPage)
}

func (s *server) handleEdit(w http.ResponseWriter, r *http.Request) {
	servePage(w, s.indexPage)
}

func servePage(w http.ResponseWriter, page []byte) {
	h := w.Header()
	h.Set("Content-Type", contentTypeHTML)
	h.Set("Content-Security-Policy", cspPage)
	h.Set("Content-Length", strconv.Itoa(len(page)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	s.static.ServeHTTP(w, r)
}

func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not found")
}

// fileResponse is the body of GET /api/file.
type fileResponse struct {
	Path       string     `json:"path"`
	Content    string     `json:"content"`
	Version    string     `json:"version"`
	Size       int64      `json:"size"`
	Modified   time.Time  `json:"modified"`
	Normalized normalized `json:"normalized"`
}

type normalized struct {
	CRLF bool `json:"crlf"`
	BOM  bool `json:"bom"`
}

func (s *server) handleRead(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "missing path")
		return
	}
	f, err := s.store.Read(path)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fileResponse{
		Path:       f.Path,
		Content:    f.Content,
		Version:    f.Version,
		Size:       f.Size,
		Modified:   f.Modified.UTC(),
		Normalized: normalized{CRLF: f.CRLF, BOM: f.BOM},
	})
}

// writeRequest is the body of PUT /api/file.
type writeRequest struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
	Version string  `json:"version"`
}

// writeResponse is the success body of PUT /api/file.
type writeResponse struct {
	Path    string `json:"path"`
	Version string `json:"version"`
	Size    int64  `json:"size"`
}

func (s *server) handleWrite(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)
	var req writeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	switch {
	case req.Path == "":
		writeError(w, http.StatusBadRequest, "missing path")
		return
	case req.Content == nil:
		writeError(w, http.StatusBadRequest, "missing content")
		return
	case req.Version == "":
		writeError(w, http.StatusBadRequest, "missing version")
		return
	}
	res, err := s.store.Save(req.Path, *req.Content, req.Version)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, writeResponse{Path: res.Path, Version: res.Version, Size: res.Size})
}

func (s *server) handleImage(w http.ResponseWriter, r *http.Request) {
	path := r.PathValue("path")
	data, ct, err := s.store.OpenImage(path)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.Itoa(len(data)))
	h.Set("Content-Security-Policy", cspImage)
	h.Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// --- JSON helpers ---

type errorResponse struct {
	Error   string `json:"error"`
	Version string `json:"version,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		log.Printf("server: marshal response: %v", err)
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", contentTypeJSON)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}

// writeStoreError maps store errors to HTTP statuses.
func writeStoreError(w http.ResponseWriter, err error) {
	var conflict *store.ErrConflict
	switch {
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, errorResponse{Error: "version conflict", Version: conflict.Current})
	case errors.Is(err, store.ErrBadPath):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, store.ErrNotText), errors.Is(err, store.ErrNotImage):
		writeError(w, http.StatusUnsupportedMediaType, err.Error())
	default:
		log.Printf("server: internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

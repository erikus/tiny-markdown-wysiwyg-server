// Package store resolves mdwebd file paths against configured roots and
// implements the read / conditional-write rules from DESIGN.md.
package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	// DefaultMaxBytes is the default maximum editable file size (10 MiB).
	DefaultMaxBytes int64 = 10 * 1024 * 1024
	// DefaultMaxImageBytes is the default maximum size served by /img/ (20 MiB).
	DefaultMaxImageBytes int64 = 20 * 1024 * 1024
	// DefaultExtensions is the default comma-separated extension allowlist.
	DefaultExtensions = ".md,.markdown"

	// pathSeparator separates segments of a URL file path.
	pathSeparator = "/"
	// tempPrefix is the prefix of temp files created next to the target.
	tempPrefix = ".mdwebd-"
	// gitDirName is the directory name that is never traversed.
	gitDirName = ".git"
	// imageTypePrefix is the content type prefix OpenImage accepts.
	imageTypePrefix = "image/"
	// minSegments is the minimum number of segments in a file path: root + file.
	minSegments = 2
	// crlf and lf are the two line-ending conventions.
	crlf = "\r\n"
	lf   = "\n"
)

var (
	// utf8BOM is the UTF-8 byte order mark.
	utf8BOM = []byte{0xEF, 0xBB, 0xBF}
	// rootNameRE constrains root names so they are safe in URLs.
	rootNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Typed errors; the server maps these to HTTP statuses.
var (
	// ErrBadPath is a malformed path (400).
	ErrBadPath = errors.New("bad path")
	// ErrForbidden is a containment or allowlist violation (403).
	ErrForbidden = errors.New("forbidden")
	// ErrNotFound means the root or file does not exist (404).
	ErrNotFound = errors.New("not found")
	// ErrTooLarge means the file or content exceeds a size limit (413).
	ErrTooLarge = errors.New("too large")
	// ErrNotText means the bytes are not valid UTF-8 (415).
	ErrNotText = errors.New("not valid UTF-8 text")
	// ErrNotImage means the file does not sniff as an image (415).
	ErrNotImage = errors.New("not an image")
)

// ErrConflict is returned by Save when the expected version does not match
// the bytes on disk (409). Current is the version currently on disk.
type ErrConflict struct {
	Current string
}

func (e *ErrConflict) Error() string {
	return "version conflict: current version is " + e.Current
}

// Root maps a URL prefix name to a directory on disk.
type Root struct {
	Name string
	Dir  string
}

// Config configures a Store.
type Config struct {
	Roots []Root
	// Extensions is the allowlist of editable extensions, with or without
	// leading dot, any case. Empty entries are ignored.
	Extensions []string
	// MaxBytes is the maximum editable file size; 0 means DefaultMaxBytes.
	MaxBytes int64
	// MaxImageBytes is the maximum image size; 0 means DefaultMaxImageBytes.
	MaxImageBytes int64
}

// Store resolves file paths and reads / writes files under the roots.
type Store struct {
	roots         map[string]string // name -> absolute, symlink-resolved dir
	rootNames     []string          // sorted
	exts          map[string]bool   // lower-case with leading dot
	maxBytes      int64
	maxImageBytes int64

	mu     sync.Mutex
	rename func(oldpath string, newpath string) error // os.Rename; swapped in tests
}

// File is the result of Read.
type File struct {
	Path     string
	Content  string // UTF-8, LF line endings, no BOM
	Version  string // hex SHA-256 of the raw bytes on disk
	Size     int64
	Modified time.Time
	CRLF     bool // the file on disk uses CRLF
	BOM      bool // the file on disk starts with a UTF-8 BOM
}

// SaveResult is the result of Save.
type SaveResult struct {
	Path    string
	Version string
	Size    int64
}

// New validates the configuration and resolves every root directory.
func New(cfg Config) (*Store, error) {
	if len(cfg.Roots) == 0 {
		return nil, errors.New("store: at least one root is required")
	}
	s := &Store{
		roots:         make(map[string]string, len(cfg.Roots)),
		exts:          make(map[string]bool, len(cfg.Extensions)),
		maxBytes:      cfg.MaxBytes,
		maxImageBytes: cfg.MaxImageBytes,
		rename:        os.Rename,
	}
	if s.maxBytes == 0 {
		s.maxBytes = DefaultMaxBytes
	}
	if s.maxImageBytes == 0 {
		s.maxImageBytes = DefaultMaxImageBytes
	}
	if s.maxBytes < 0 || s.maxImageBytes < 0 {
		return nil, errors.New("store: size limits must not be negative")
	}
	for _, r := range cfg.Roots {
		if !rootNameRE.MatchString(r.Name) {
			return nil, fmt.Errorf("store: invalid root name %q (want %s)", r.Name, rootNameRE)
		}
		if _, dup := s.roots[r.Name]; dup {
			return nil, fmt.Errorf("store: duplicate root name %q", r.Name)
		}
		if r.Dir == "" {
			return nil, fmt.Errorf("store: root %q has an empty directory", r.Name)
		}
		abs, err := filepath.Abs(r.Dir)
		if err != nil {
			return nil, fmt.Errorf("store: root %q: %w", r.Name, err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return nil, fmt.Errorf("store: root %q: %w", r.Name, err)
		}
		fi, err := os.Stat(real)
		if err != nil {
			return nil, fmt.Errorf("store: root %q: %w", r.Name, err)
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf("store: root %q: %s is not a directory", r.Name, real)
		}
		s.roots[r.Name] = real
		s.rootNames = append(s.rootNames, r.Name)
	}
	sort.Strings(s.rootNames)
	for _, e := range cfg.Extensions {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		s.exts[e] = true
	}
	if len(s.exts) == 0 {
		return nil, errors.New("store: extension allowlist is empty")
	}
	return s, nil
}

// RootNames returns the configured root names, sorted.
func (s *Store) RootNames() []string {
	out := make([]string, len(s.rootNames))
	copy(out, s.rootNames)
	return out
}

// MaxBytes returns the maximum editable file size.
func (s *Store) MaxBytes() int64 {
	return s.maxBytes
}

// resolved is the outcome of resolving a URL file path.
type resolved struct {
	real string      // real path of the regular file
	info fs.FileInfo // Stat of real
}

// resolve applies the path rules from DESIGN.md ("Store rules").
func (s *Store) resolve(path string, checkExt bool) (resolved, error) {
	segments := strings.Split(path, pathSeparator)
	if len(segments) < minSegments {
		return resolved{}, fmt.Errorf("%w: want ROOT/file", ErrBadPath)
	}
	if segments[0] == "" {
		return resolved{}, fmt.Errorf("%w: empty root name", ErrBadPath)
	}
	rootDir, ok := s.roots[segments[0]]
	if !ok {
		return resolved{}, fmt.Errorf("%w: unknown root %q", ErrNotFound, segments[0])
	}
	rel := segments[1:]
	for _, seg := range rel {
		switch {
		case seg == "", seg == ".", seg == "..":
			return resolved{}, fmt.Errorf("%w: invalid path segment %q", ErrBadPath, seg)
		case strings.ContainsAny(seg, "\x00\\"):
			return resolved{}, fmt.Errorf("%w: invalid character in segment", ErrBadPath)
		case strings.EqualFold(seg, gitDirName):
			return resolved{}, fmt.Errorf("%w: %s is not accessible", ErrForbidden, gitDirName)
		}
	}
	last := rel[len(rel)-1]
	if checkExt {
		ext := strings.ToLower(filepath.Ext(last))
		if ext == "" || !s.exts[ext] {
			return resolved{}, fmt.Errorf("%w: extension %q is not editable", ErrForbidden, ext)
		}
	}

	full := filepath.Join(append([]string{rootDir}, rel...)...)
	realParent, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return resolved{}, mapFSError(err)
	}
	if !contained(rootDir, realParent) {
		return resolved{}, fmt.Errorf("%w: parent directory escapes root", ErrForbidden)
	}

	candidate := filepath.Join(realParent, last)
	li, err := os.Lstat(candidate)
	if err != nil {
		return resolved{}, mapFSError(err)
	}
	real := candidate
	info := li
	if li.Mode()&fs.ModeSymlink != 0 {
		real, err = filepath.EvalSymlinks(candidate)
		if err != nil {
			return resolved{}, mapFSError(err)
		}
		if !contained(rootDir, real) {
			return resolved{}, fmt.Errorf("%w: symlink target escapes root", ErrForbidden)
		}
		info, err = os.Stat(real)
		if err != nil {
			return resolved{}, mapFSError(err)
		}
	}
	if !info.Mode().IsRegular() {
		return resolved{}, fmt.Errorf("%w: not a regular file", ErrBadPath)
	}
	return resolved{real: real, info: info}, nil
}

// contained reports whether p is root itself or lies beneath it. Both must
// already be symlink-resolved.
func contained(root string, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// mapFSError turns filesystem errors into the store's typed errors.
func mapFSError(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrInvalid) {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %v", ErrForbidden, err)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) && errors.Is(pe.Err, syscall.ENOTDIR) {
		// A path component is a file, not a directory.
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// readLimited reads a regular file whose size must not exceed limit.
func readLimited(r resolved, limit int64) ([]byte, error) {
	if r.info.Size() > limit {
		return nil, fmt.Errorf("%w: %d bytes exceeds limit of %d", ErrTooLarge, r.info.Size(), limit)
	}
	raw, err := os.ReadFile(r.real)
	if err != nil {
		return nil, mapFSError(err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%w: %d bytes exceeds limit of %d", ErrTooLarge, len(raw), limit)
	}
	return raw, nil
}

// version returns the hex SHA-256 of raw.
func version(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// splitBOM strips a leading UTF-8 BOM and reports whether one was present.
func splitBOM(raw []byte) ([]byte, bool) {
	if bytes.HasPrefix(raw, utf8BOM) {
		return raw[len(utf8BOM):], true
	}
	return raw, false
}

// Read resolves path and returns its normalized content.
func (s *Store) Read(path string) (*File, error) {
	r, err := s.resolve(path, true)
	if err != nil {
		return nil, err
	}
	raw, err := readLimited(r, s.maxBytes)
	if err != nil {
		return nil, err
	}
	text, bom := splitBOM(raw)
	if !utf8.Valid(text) {
		return nil, ErrNotText
	}
	hasCRLF := bytes.Contains(text, []byte(crlf))
	if hasCRLF {
		text = bytes.ReplaceAll(text, []byte(crlf), []byte(lf))
	}
	return &File{
		Path:     path,
		Content:  string(text),
		Version:  version(raw),
		Size:     r.info.Size(),
		Modified: r.info.ModTime(),
		CRLF:     hasCRLF,
		BOM:      bom,
	}, nil
}

// Save atomically replaces the file at path with content, provided the file's
// current version equals expectedVersion. The original file's conventions
// (BOM, CRLF, trailing newline) are re-applied to content.
func (s *Store) Save(path string, content string, expectedVersion string) (*SaveResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, err := s.resolve(path, true)
	if err != nil {
		return nil, err
	}
	current, err := readLimited(r, s.maxBytes)
	if err != nil {
		return nil, err
	}
	if cv := version(current); cv != expectedVersion {
		return nil, &ErrConflict{Current: cv}
	}
	if !utf8.ValidString(content) {
		return nil, ErrNotText
	}
	if int64(len(content)) > s.maxBytes {
		return nil, fmt.Errorf("%w: content is %d bytes, limit is %d", ErrTooLarge, len(content), s.maxBytes)
	}

	out := applyConventions(current, []byte(content))
	if int64(len(out)) > s.maxBytes {
		return nil, fmt.Errorf("%w: content is %d bytes after normalization, limit is %d", ErrTooLarge, len(out), s.maxBytes)
	}
	if err := s.writeAtomic(r, out); err != nil {
		return nil, err
	}
	return &SaveResult{Path: path, Version: version(out), Size: int64(len(out))}, nil
}

// applyConventions re-applies the BOM, line-ending and trailing-newline
// conventions of original to content (which is LF-normalized text).
func applyConventions(original []byte, content []byte) []byte {
	orig, bom := splitBOM(original)
	hasCRLF := bytes.Contains(orig, []byte(crlf))

	out := bytes.ReplaceAll(content, []byte(crlf), []byte(lf))
	origEndsNL := bytes.HasSuffix(orig, []byte(lf))
	newEndsNL := bytes.HasSuffix(out, []byte(lf))
	switch {
	case origEndsNL && !newEndsNL:
		out = append(out, lf...)
	case !origEndsNL && newEndsNL:
		out = out[:len(out)-len(lf)]
	}
	if hasCRLF {
		out = bytes.ReplaceAll(out, []byte(lf), []byte(crlf))
	}
	if bom {
		out = append(append([]byte{}, utf8BOM...), out...)
	}
	return out
}

// writeAtomic writes data to a temp file beside r.real with the same mode
// bits, fsyncs it, and renames it over r.real. The temp file is removed on
// any failure.
func (s *Store) writeAtomic(r resolved, data []byte) (err error) {
	dir := filepath.Dir(r.real)
	tmp, err := os.CreateTemp(dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(r.info.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("fsync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := s.rename(tmpName, r.real); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	renamed = true
	return nil
}

// OpenImage resolves path with the same rules as Read, minus the extension
// allowlist, and returns the bytes and sniffed content type if and only if
// the content sniffs as image/*.
func (s *Store) OpenImage(path string) ([]byte, string, error) {
	r, err := s.resolve(path, false)
	if err != nil {
		return nil, "", err
	}
	raw, err := readLimited(r, s.maxImageBytes)
	if err != nil {
		return nil, "", err
	}
	ct := http.DetectContentType(raw)
	if !strings.HasPrefix(ct, imageTypePrefix) {
		return nil, "", fmt.Errorf("%w: sniffed as %s", ErrNotImage, ct)
	}
	return raw, ct, nil
}

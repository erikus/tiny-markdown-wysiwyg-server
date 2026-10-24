package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const (
	testRoot     = "notes"
	testMaxBytes = 64
)

// newTestStore creates a store with one root ("notes") in a temp dir and
// returns the store plus the root directory. outside is a sibling directory
// that is not under any root.
func newTestStore(t *testing.T, maxBytes int64) (s *Store, root string, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var err error
	s, err = New(Config{
		Roots:      []Root{{Name: testRoot, Dir: root}},
		Extensions: strings.Split(DefaultExtensions, ","),
		MaxBytes:   maxBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, root, outside
}

func writeFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNewRejectsBadConfig(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	writeFile(t, file, []byte("x"), 0o644)
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no roots", Config{Extensions: []string{".md"}}},
		{"bad name", Config{Roots: []Root{{Name: "a/b", Dir: dir}}, Extensions: []string{".md"}}},
		{"empty name", Config{Roots: []Root{{Name: "", Dir: dir}}, Extensions: []string{".md"}}},
		{"duplicate", Config{Roots: []Root{{Name: "a", Dir: dir}, {Name: "a", Dir: dir}}, Extensions: []string{".md"}}},
		{"missing dir", Config{Roots: []Root{{Name: "a", Dir: filepath.Join(dir, "nope")}}, Extensions: []string{".md"}}},
		{"dir is file", Config{Roots: []Root{{Name: "a", Dir: file}}, Extensions: []string{".md"}}},
		{"no extensions", Config{Roots: []Root{{Name: "a", Dir: dir}}}},
		{"negative limit", Config{Roots: []Root{{Name: "a", Dir: dir}}, Extensions: []string{".md"}, MaxBytes: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestResolvePathRules(t *testing.T) {
	s, root, outside := newTestStore(t, 0)
	writeFile(t, filepath.Join(root, "a.md"), []byte("a\n"), 0o644)
	writeFile(t, filepath.Join(root, "sub", "b.md"), []byte("b\n"), 0o644)
	writeFile(t, filepath.Join(root, "UPPER.MD"), []byte("u\n"), 0o644)
	writeFile(t, filepath.Join(root, "plain.txt"), []byte("t\n"), 0o644)
	writeFile(t, filepath.Join(root, ".git", "c.md"), []byte("g\n"), 0o644)
	writeFile(t, filepath.Join(root, "noext"), []byte("n\n"), 0o644)
	if err := os.Mkdir(filepath.Join(root, "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "fifo.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(outside, "x.md"), []byte("x\n"), 0o644)
	// Symlinks: in-root file, out-of-root file, out-of-root dir, in-root dir.
	mustSymlink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	mustSymlink(filepath.Join(root, "a.md"), filepath.Join(root, "link-in.md"))
	mustSymlink(filepath.Join(outside, "x.md"), filepath.Join(root, "link-out.md"))
	mustSymlink(outside, filepath.Join(root, "dir-out"))
	mustSymlink(filepath.Join(root, "sub"), filepath.Join(root, "dir-in"))
	mustSymlink(filepath.Join(root, "missing-target.md"), filepath.Join(root, "dangling.md"))

	cases := []struct {
		name string
		path string
		want error // nil means success
	}{
		{"plain file", "notes/a.md", nil},
		{"nested file", "notes/sub/b.md", nil},
		{"upper-case extension", "notes/UPPER.MD", nil},
		{"in-root file symlink", "notes/link-in.md", nil},
		{"in-root dir symlink", "notes/dir-in/b.md", nil},
		{"empty path", "", ErrBadPath},
		{"root only", "notes", ErrBadPath},
		{"root with trailing slash", "notes/", ErrBadPath},
		{"absolute", "/etc/passwd", ErrBadPath},
		{"dot-dot", "notes/../a.md", ErrBadPath},
		{"dot-dot nested", "notes/sub/../a.md", ErrBadPath},
		{"dot segment", "notes/./a.md", ErrBadPath},
		{"empty segment", "notes//a.md", ErrBadPath},
		{"backslash", "notes/sub\\b.md", ErrBadPath},
		{"nul", "notes/a\x00.md", ErrBadPath},
		{"directory", "notes/dir.md", ErrBadPath},
		{"fifo", "notes/fifo.md", ErrBadPath},
		{"git lower", "notes/.git/c.md", ErrForbidden},
		{"git upper", "notes/.GIT/c.md", ErrForbidden},
		{"git mixed", "notes/.Git/c.md", ErrForbidden},
		{"extension not allowed", "notes/plain.txt", ErrForbidden},
		{"no extension", "notes/noext", ErrForbidden},
		{"out-of-root file symlink", "notes/link-out.md", ErrForbidden},
		{"out-of-root dir symlink", "notes/dir-out/x.md", ErrForbidden},
		{"unknown root", "other/a.md", ErrNotFound},
		{"missing file", "notes/nope.md", ErrNotFound},
		{"missing dir", "notes/nope/a.md", ErrNotFound},
		{"dangling symlink", "notes/dangling.md", ErrNotFound},
		{"file as dir", "notes/a.md/b.md", ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Read(tc.path)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenImage(t *testing.T) {
	s, root, _ := newTestStore(t, 0)
	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 16))
	writeFile(t, filepath.Join(root, "pic.png"), png, 0o644)
	writeFile(t, filepath.Join(root, "noext"), png, 0o644)
	writeFile(t, filepath.Join(root, "fake.png"), []byte("hello, not an image\n"), 0o644)
	writeFile(t, filepath.Join(root, ".git", "pic.png"), png, 0o644)

	cases := []struct {
		name string
		path string
		want error
		ct   string
	}{
		{"png", "notes/pic.png", nil, "image/png"},
		{"png without extension", "notes/noext", nil, "image/png"},
		{"text with png extension", "notes/fake.png", ErrNotImage, ""},
		{"missing", "notes/nope.png", ErrNotFound, ""},
		{"git", "notes/.git/pic.png", ErrForbidden, ""},
		{"traversal", "notes/../pic.png", ErrBadPath, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, ct, err := s.OpenImage(tc.path)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("got %v, want %v", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ct != tc.ct || !bytes.Equal(data, png) {
				t.Fatalf("got %q/%d bytes", ct, len(data))
			}
		})
	}

	t.Run("too large", func(t *testing.T) {
		small, err := New(Config{
			Roots:         []Root{{Name: testRoot, Dir: root}},
			Extensions:    []string{".md"},
			MaxImageBytes: int64(len(png) - 1),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := small.OpenImage("notes/pic.png"); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("got %v, want ErrTooLarge", err)
		}
	})
}

func TestReadNormalization(t *testing.T) {
	s, root, _ := newTestStore(t, 0)
	cases := []struct {
		name    string
		raw     string
		content string
		crlf    bool
		bom     bool
	}{
		{"lf", "a\nb\n", "a\nb\n", false, false},
		{"crlf", "a\r\nb\r\n", "a\nb\n", true, false},
		{"bom", "\xEF\xBB\xBFa\n", "a\n", false, true},
		{"bom crlf", "\xEF\xBB\xBFa\r\nb", "a\nb", true, true},
		{"no trailing newline", "a", "a", false, false},
		{"empty", "", "", false, false},
		{"lone cr kept", "a\rb\n", "a\rb\n", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(root, tc.name+".md")
			writeFile(t, p, []byte(tc.raw), 0o644)
			f, err := s.Read("notes/" + tc.name + ".md")
			if err != nil {
				t.Fatal(err)
			}
			if f.Content != tc.content || f.CRLF != tc.crlf || f.BOM != tc.bom {
				t.Fatalf("got content=%q crlf=%v bom=%v", f.Content, f.CRLF, f.BOM)
			}
			if f.Version != version([]byte(tc.raw)) || f.Size != int64(len(tc.raw)) {
				t.Fatalf("bad version/size: %q %d", f.Version, f.Size)
			}
		})
	}

	t.Run("invalid utf8", func(t *testing.T) {
		writeFile(t, filepath.Join(root, "bad.md"), []byte("a\xffb"), 0o644)
		if _, err := s.Read("notes/bad.md"); !errors.Is(err, ErrNotText) {
			t.Fatalf("got %v, want ErrNotText", err)
		}
	})
	t.Run("too large", func(t *testing.T) {
		smallStore, err := New(Config{Roots: []Root{{Name: testRoot, Dir: root}}, Extensions: []string{".md"}, MaxBytes: testMaxBytes})
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(root, "big.md"), bytes.Repeat([]byte("x"), testMaxBytes+1), 0o644)
		if _, err := smallStore.Read("notes/big.md"); !errors.Is(err, ErrTooLarge) {
			t.Fatalf("got %v, want ErrTooLarge", err)
		}
	})
}

func TestSaveConventions(t *testing.T) {
	s, root, _ := newTestStore(t, 0)
	cases := []struct {
		name    string
		raw     string // original bytes on disk
		content string // what the editor sends
		want    string // bytes expected on disk afterwards
	}{
		{"lf keeps lf", "a\n", "b\n", "b\n"},
		{"crlf restored", "a\r\nb\r\n", "c\nd\n", "c\r\nd\r\n"},
		{"crlf restored even if content has crlf", "a\r\n", "c\r\nd\n", "c\r\nd\r\n"},
		{"bom restored", "\xEF\xBB\xBFa\n", "b\n", "\xEF\xBB\xBFb\n"},
		{"bom and crlf restored", "\xEF\xBB\xBFa\r\n", "b\nc", "\xEF\xBB\xBFb\r\nc\r\n"},
		{"trailing newline added", "a\n", "b", "b\n"},
		{"trailing newline removed", "a", "b\n", "b"},
		{"no trailing newline kept", "a", "b", "b"},
		{"trailing newline kept", "a\n", "b\n", "b\n"},
		{"empty original gets no newline", "", "b\n", "b"},
		{"empty content on newline file", "a\n", "", "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(root, "f.md")
			writeFile(t, p, []byte(tc.raw), 0o644)
			f, err := s.Read("notes/f.md")
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Save("notes/f.md", tc.content, f.Version)
			if err != nil {
				t.Fatal(err)
			}
			got := readFile(t, p)
			if string(got) != tc.want {
				t.Fatalf("on disk %q, want %q", got, tc.want)
			}
			if res.Version != version(got) || res.Size != int64(len(got)) {
				t.Fatalf("result version/size mismatch: %+v", res)
			}
			// A read-back round-trips to the saved content (normalized).
			again, err := s.Read("notes/f.md")
			if err != nil {
				t.Fatal(err)
			}
			if again.Version != res.Version {
				t.Fatalf("read-back version %q != saved %q", again.Version, res.Version)
			}
		})
	}
}

func TestSaveConflict(t *testing.T) {
	s, root, _ := newTestStore(t, 0)
	p := filepath.Join(root, "f.md")
	writeFile(t, p, []byte("one\n"), 0o644)
	f, err := s.Read("notes/f.md")
	if err != nil {
		t.Fatal(err)
	}
	// Someone else changes the file behind our back.
	writeFile(t, p, []byte("two\n"), 0o644)
	_, err = s.Save("notes/f.md", "three\n", f.Version)
	var conflict *ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if conflict.Current != version([]byte("two\n")) {
		t.Fatalf("conflict current = %q", conflict.Current)
	}
	if got := readFile(t, p); string(got) != "two\n" {
		t.Fatalf("file was modified on conflict: %q", got)
	}
}

func TestSaveRejects(t *testing.T) {
	s, root, _ := newTestStore(t, testMaxBytes)
	p := filepath.Join(root, "f.md")
	writeFile(t, p, []byte("a\n"), 0o644)
	f, err := s.Read("notes/f.md")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		content string
		want    error
	}{
		{"too large", strings.Repeat("x", testMaxBytes+1), ErrTooLarge},
		{"too large after crlf expansion is fine under limit", strings.Repeat("x", testMaxBytes-1) + "\n", nil},
		{"invalid utf8", "a\xffb", ErrNotText},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, p, []byte("a\n"), 0o644)
			_, err := s.Save("notes/f.md", tc.content, f.Version)
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if got := readFile(t, p); string(got) != "a\n" {
				t.Fatalf("file modified on rejected save: %q", got)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		if _, err := s.Save("notes/nope.md", "x", f.Version); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})
}

func TestSaveThroughSymlinkKeepsSymlink(t *testing.T) {
	s, root, _ := newTestStore(t, 0)
	real := filepath.Join(root, "sub", "real.md")
	link := filepath.Join(root, "link.md")
	writeFile(t, real, []byte("a\n"), 0o644)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f, err := s.Read("notes/link.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save("notes/link.md", "b\n", f.Version); err != nil {
		t.Fatal(err)
	}
	li, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if li.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	if got := readFile(t, real); string(got) != "b\n" {
		t.Fatalf("target content %q", got)
	}
}

func TestSavePreservesModeBits(t *testing.T) {
	s, root, _ := newTestStore(t, 0)
	for _, mode := range []os.FileMode{0o600, 0o644, 0o755} {
		p := filepath.Join(root, "f.md")
		writeFile(t, p, []byte("a\n"), mode)
		f, err := s.Read("notes/f.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Save("notes/f.md", "b\n", f.Version); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Fatalf("mode %o, want %o", fi.Mode().Perm(), mode)
		}
	}
}

func TestSaveCleansTempFileOnFailure(t *testing.T) {
	s, root, _ := newTestStore(t, 0)
	p := filepath.Join(root, "f.md")
	writeFile(t, p, []byte("a\n"), 0o644)
	f, err := s.Read("notes/f.md")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected rename failure")
	s.rename = func(string, string) error { return injected }
	if _, err := s.Save("notes/f.md", "b\n", f.Version); !errors.Is(err, injected) {
		t.Fatalf("got %v, want injected error", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), tempPrefix) {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if got := readFile(t, p); string(got) != "a\n" {
		t.Fatalf("file modified on failed save: %q", got)
	}
}

func TestRootNamesSorted(t *testing.T) {
	dir := t.TempDir()
	s, err := New(Config{
		Roots:      []Root{{Name: "zeta", Dir: dir}, {Name: "alpha", Dir: dir}},
		Extensions: []string{"md", " .Markdown "},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.RootNames(); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("got %v", got)
	}
	if !s.exts[".md"] || !s.exts[".markdown"] {
		t.Fatalf("extensions not normalized: %v", s.exts)
	}
}

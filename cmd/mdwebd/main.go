// Command mdwebd is a single-binary browser Markdown editor for directories
// that already exist on disk. See DESIGN.md and README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/erikus/tiny-markdown-wysiwyg-server/internal/server"
	"github.com/erikus/tiny-markdown-wysiwyg-server/internal/store"
)

const (
	defaultListen = "127.0.0.1:8585"
	// tsnetListenAddr is the HTTPS address served when running as a tsnet node.
	tsnetListenAddr = ":443"
	// tsnetStateSubdir is appended to the XDG state directory for tsnet state.
	tsnetStateSubdir = "mdwebd/tsnet"
	// xdgStateFallback is used under $HOME when XDG_STATE_HOME is unset.
	xdgStateFallback = ".local/state"
	// extSeparator separates entries in --ext.
	extSeparator = ","
	// rootFlagSeparator separates NAME and DIR in --root.
	rootFlagSeparator = "="

	shutdownTimeout   = 10 * time.Second
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	// tsnetUpTimeout bounds how long we wait for the tsnet node to come up
	// (including interactive login).
	tsnetUpTimeout = 15 * time.Minute
)

// rootsFlag collects repeatable --root NAME=DIR values.
type rootsFlag []store.Root

func (r *rootsFlag) String() string {
	parts := make([]string, 0, len(*r))
	for _, root := range *r {
		parts = append(parts, root.Name+rootFlagSeparator+root.Dir)
	}
	return strings.Join(parts, " ")
}

func (r *rootsFlag) Set(v string) error {
	name, dir, ok := strings.Cut(v, rootFlagSeparator)
	if !ok || name == "" || dir == "" {
		return fmt.Errorf("want NAME%sDIR, got %q", rootFlagSeparator, v)
	}
	*r = append(*r, store.Root{Name: name, Dir: dir})
	return nil
}

// stringsFlag collects repeatable string flags.
type stringsFlag []string

func (s *stringsFlag) String() string { return strings.Join(*s, " ") }

func (s *stringsFlag) Set(v string) error {
	if v == "" {
		return errors.New("empty value")
	}
	*s = append(*s, v)
	return nil
}

// options holds the parsed command line.
type options struct {
	roots         rootsFlag
	listen        string
	tsnetHostname string
	tsnetDir      string
	allowedHosts  stringsFlag
	viewBase      string
	ext           string
	maxBytes      int64
	maxImageBytes int64
}

func defaultTsnetDir() string {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, tsnetStateSubdir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(xdgStateFallback, tsnetStateSubdir)
	}
	return filepath.Join(home, xdgStateFallback, tsnetStateSubdir)
}

func parseFlags(args []string) (*options, error) {
	opts := &options{}
	fs := flag.NewFlagSet("mdwebd", flag.ContinueOnError)
	fs.Var(&opts.roots, "root", "NAME=DIR: expose DIR under the URL prefix NAME (required, repeatable)")
	fs.StringVar(&opts.listen, "listen", defaultListen, "plain HTTP listen address; ignored with --tsnet-hostname")
	fs.StringVar(&opts.tsnetHostname, "tsnet-hostname", "", "run as a Tailscale node with this name, serving HTTPS on :443")
	fs.StringVar(&opts.tsnetDir, "tsnet-dir", defaultTsnetDir(), "tsnet state directory")
	fs.Var(&opts.allowedHosts, "allowed-host", "extra Host header value to accept (repeatable)")
	fs.StringVar(&opts.viewBase, "view-base", "", "if set, the editor links to URL + \"/\" + path for viewing")
	fs.StringVar(&opts.ext, "ext", store.DefaultExtensions, "comma-separated, case-insensitive editable extensions")
	fs.Int64Var(&opts.maxBytes, "max-bytes", store.DefaultMaxBytes, "maximum editable file and request body size")
	fs.Int64Var(&opts.maxImageBytes, "max-image-bytes", store.DefaultMaxImageBytes, "maximum size served by /img/")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	if len(opts.roots) == 0 {
		return nil, errors.New("at least one --root NAME=DIR is required")
	}
	if opts.maxBytes <= 0 || opts.maxImageBytes <= 0 {
		return nil, errors.New("--max-bytes and --max-image-bytes must be positive")
	}
	if opts.tsnetHostname == "" {
		if _, _, err := net.SplitHostPort(opts.listen); err != nil {
			return nil, fmt.Errorf("--listen: %w", err)
		}
	}
	return opts, nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("mdwebd: ")
	opts, err := parseFlags(os.Args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatal(err)
	}
	if err := run(opts); err != nil {
		log.Fatal(err)
	}
}

func run(opts *options) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.New(store.Config{
		Roots:         opts.roots,
		Extensions:    strings.Split(opts.ext, extSeparator),
		MaxBytes:      opts.maxBytes,
		MaxImageBytes: opts.maxImageBytes,
	})
	if err != nil {
		return err
	}
	for _, r := range opts.roots {
		log.Printf("root %s=%s", r.Name, r.Dir)
	}

	allowedHosts := []string(opts.allowedHosts)
	var ln net.Listener
	var closeNode func() error = func() error { return nil }
	https := opts.tsnetHostname != ""
	if https {
		ts := &tsnet.Server{Hostname: opts.tsnetHostname, Dir: opts.tsnetDir}
		closeNode = ts.Close
		upCtx, cancel := context.WithTimeout(ctx, tsnetUpTimeout)
		defer cancel()
		if _, err := ts.Up(upCtx); err != nil {
			return fmt.Errorf("tsnet up: %w", err)
		}
		lc, err := ts.LocalClient()
		if err != nil {
			return fmt.Errorf("tsnet local client: %w", err)
		}
		status, err := lc.Status(ctx)
		if err != nil {
			return fmt.Errorf("tsnet status: %w", err)
		}
		if status.Self == nil {
			return errors.New("tsnet status has no Self node")
		}
		if status.Self.HostName != "" {
			allowedHosts = append(allowedHosts, status.Self.HostName)
		}
		if fqdn := strings.TrimSuffix(status.Self.DNSName, "."); fqdn != "" {
			allowedHosts = append(allowedHosts, fqdn)
		}
		ln, err = ts.ListenTLS("tcp", tsnetListenAddr)
		if err != nil {
			return fmt.Errorf("tsnet listen: %w", err)
		}
		log.Printf("serving https://%s/", strings.TrimSuffix(status.Self.DNSName, "."))
	} else {
		host, _, err := net.SplitHostPort(opts.listen)
		if err != nil {
			return fmt.Errorf("--listen: %w", err)
		}
		if host != "" {
			allowedHosts = append(allowedHosts, host)
		}
		ln, err = net.Listen("tcp", opts.listen)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		log.Printf("serving http://%s/", ln.Addr())
	}
	defer func() {
		if err := closeNode(); err != nil {
			log.Printf("closing tsnet node: %v", err)
		}
	}()

	handler, err := server.New(server.Config{
		Store:        st,
		AllowedHosts: allowedHosts,
		HTTPS:        https,
		ViewBase:     opts.viewBase,
	})
	if err != nil {
		return err
	}
	log.Printf("allowed hosts: %s (plus localhost, 127.0.0.1, ::1)", strings.Join(allowedHosts, ", "))

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

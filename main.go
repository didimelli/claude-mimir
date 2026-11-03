package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseConfig reads the command line; MIMIR_API_KEY and MIMIR_ANTHROPIC_URL come from the environment (--mimir-url
// overrides the latter).
func parseConfig(args []string, getenv func(string) string) (config, error) {
	var cfg config
	var prefixes multiFlag
	var efforts multiFlag
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet("it-router", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.host, "host", "127.0.0.1", "listen address")
	fs.IntVar(&cfg.port, "port", 4000, "listen port")
	fs.StringVar(&cfg.anthropic, "anthropic", "https://api.anthropic.com", "upstream for everything that is not a Mimir model")
	fs.Var(&prefixes, "prefix", "model prefix routed to Mimir (default cubbit/)")
	fs.StringVar(&cfg.logPath, "log", filepath.Join(home, "Library/Logs/it-router.jsonl"), "metadata-only request log")
	fs.StringVar(&cfg.mimirURL, "mimir-url", getenv("MIMIR_ANTHROPIC_URL"), "Mimir's Anthropic-compatible endpoint (default $MIMIR_ANTHROPIC_URL)")
	fs.Var(&efforts, "mimir-effort", "model=effort sent to Mimir as adaptive thinking (repeatable; 'none' sends nothing; default cubbit/mimir-pro=high)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	cfg.prefix = prefixes
	if len(cfg.prefix) == 0 {
		cfg.prefix = []string{"cubbit/"}
	}
	cfg.effort = map[string]string{"cubbit/mimir-pro": "high"}
	for _, e := range efforts {
		if e == "none" {
			cfg.effort = nil
			continue
		}
		model, level, ok := strings.Cut(e, "=")
		if !ok || model == "" || !validEffort[level] {
			return cfg, fmt.Errorf("--mimir-effort %q: want model=low|medium|high|xhigh|max, or none", e)
		}
		if cfg.effort == nil {
			cfg.effort = map[string]string{}
		}
		cfg.effort[model] = level
	}
	cfg.anthropic, cfg.mimirURL = strings.TrimRight(cfg.anthropic, "/"), strings.TrimRight(cfg.mimirURL, "/")
	if cfg.mimirURL == "" {
		return cfg, errors.New("MIMIR_ANTHROPIC_URL is missing from the environment (or pass --mimir-url)")
	}
	if cfg.mimirKey = getenv("MIMIR_API_KEY"); cfg.mimirKey == "" {
		return cfg, errors.New("MIMIR_API_KEY is missing from the environment")
	}
	return cfg, nil
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "it-router:", err)
		os.Exit(2)
	}
	g, err := newGateway(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "it-router:", err)
		os.Exit(2)
	}
	srv := &http.Server{Addr: fmt.Sprintf("%s:%d", cfg.host, cfg.port), Handler: g, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
	fmt.Fprintf(os.Stderr, "router on %s: %v -> %s, everything else -> %s; log %s\n", srv.Addr, cfg.prefix, cfg.mimirURL, cfg.anthropic, cfg.logPath)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "it-router:", err)
			os.Exit(1)
		}
	case <-sig:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = srv.Close()
	}
}

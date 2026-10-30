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

// parseConfig reads the command line. The flags are the same as the Python router's, plus --mimir-url, so the
// same test suite runs against either implementation.
func parseConfig(args []string, getenv func(string) string) (config, error) {
	var cfg config
	var prefixes multiFlag
	mimirURL := ""
	home, _ := os.UserHomeDir()
	litellmKey := getenv("IT_LITELLM_KEY")
	if litellmKey == "" {
		litellmKey = "sk-local-gateway"
	}
	fs := flag.NewFlagSet("it-router", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&cfg.host, "host", "127.0.0.1", "listen address")
	fs.IntVar(&cfg.port, "port", 4000, "listen port")
	fs.StringVar(&cfg.anthropic, "anthropic", "https://api.anthropic.com", "upstream for everything that is not a Mimir model")
	fs.StringVar(&cfg.litellm, "litellm", "http://127.0.0.1:4001", "LiteLLM base URL for Mimir models (LiteLLM mode)")
	fs.StringVar(&cfg.litellmKey, "litellm-key", litellmKey, "LiteLLM key")
	fs.Var(&prefixes, "prefix", "model prefix routed to Mimir (default cubbit/)")
	fs.StringVar(&cfg.logPath, "log", filepath.Join(home, "Library/Logs/it-router.jsonl"), "metadata-only request log")
	fs.StringVar(&mimirURL, "mimir-url", "", "Mimir's Anthropic-compatible endpoint: send Mimir models there directly, without LiteLLM")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	cfg.prefix = prefixes
	if len(cfg.prefix) == 0 {
		cfg.prefix = []string{"cubbit/"}
	}
	cfg.anthropic, cfg.litellm = strings.TrimRight(cfg.anthropic, "/"), strings.TrimRight(cfg.litellm, "/")
	if mimirURL != "" {
		cfg.mimirURL = strings.TrimRight(mimirURL, "/")
		if cfg.mimirKey = getenv("MIMIR_API_KEY"); cfg.mimirKey == "" {
			return cfg, errors.New("--mimir-url needs MIMIR_API_KEY in the environment")
		}
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
	mimirTarget := cfg.litellm + " (LiteLLM)"
	if cfg.mimirURL != "" {
		mimirTarget = cfg.mimirURL + " (direct)"
	}
	fmt.Fprintf(os.Stderr, "router on %s: %v -> %s, everything else -> %s; log %s\n", srv.Addr, cfg.prefix, mimirTarget, cfg.anthropic, cfg.logPath)
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

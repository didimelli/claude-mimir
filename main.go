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
	"strconv"
	"strings"
	"syscall"
	"time"
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// parseConfig reads the command line and the environment: MIMIR_API_KEY, MIMIR_API (anthropic or openai), and the
// endpoint of the chosen API, MIMIR_ANTHROPIC_URL or MIMIR_OPENAI_URL (--mimir-url overrides it).
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
	api := getenv("MIMIR_API")
	if api == "" {
		api = "anthropic"
	}
	fs.StringVar(&cfg.mimirAPI, "mimir-api", api, "which Mimir API to talk to: anthropic (/anthropic, passthrough) or openai (/v1, translated) (default $MIMIR_API or anthropic)")
	fs.StringVar(&cfg.mimirURL, "mimir-url", "", "Mimir endpoint for the chosen API (default $MIMIR_ANTHROPIC_URL or $MIMIR_OPENAI_URL)")
	fs.Var(&efforts, "mimir-effort", "model=effort, the reasoning effort sent to Mimir (repeatable; 'none' alone sends nothing; default cubbit/mimir-pro=high on anthropic, cubbit/mimir-pro=medium on openai)")
	maxText := 24 << 10
	if v := getenv("IT_MAX_REPLY_TEXT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("IT_MAX_REPLY_TEXT %q: want a positive number of bytes", v)
		}
		maxText = n
	}
	runaway := getenv("IT_RUNAWAY_GUARD")
	if runaway == "" {
		runaway = "log"
	}
	fs.StringVar(&cfg.runaway, "runaway-guard", runaway, "what to do about a runaway reply (very long text, or one line repeated): log (record it in the request log) or off (default $IT_RUNAWAY_GUARD or log)")
	fs.IntVar(&cfg.maxReplyText, "max-reply-text", maxText, "visible reply bytes from which a reply counts as a runaway (default $IT_MAX_REPLY_TEXT or 24576)")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	switch cfg.runaway {
	case "log", "off":
	default:
		return cfg, fmt.Errorf("--runaway-guard %q: want log or off", cfg.runaway)
	}
	if cfg.maxReplyText <= 0 {
		return cfg, fmt.Errorf("--max-reply-text %d: want a positive number of bytes", cfg.maxReplyText)
	}
	cfg.prefix = prefixes
	if len(cfg.prefix) == 0 {
		cfg.prefix = []string{"cubbit/"}
	}
	valid, urlVar := anthropicEfforts, "MIMIR_ANTHROPIC_URL"
	cfg.effort = map[string]string{"cubbit/mimir-pro": "high"}
	switch cfg.mimirAPI {
	case "anthropic":
	case "openai":
		valid, urlVar, cfg.effort = openaiEfforts, "MIMIR_OPENAI_URL", map[string]string{"cubbit/mimir-pro": "medium"}
	default:
		return cfg, fmt.Errorf("--mimir-api %q: want anthropic or openai", cfg.mimirAPI)
	}
	if cfg.mimirURL == "" {
		cfg.mimirURL = getenv(urlVar)
	}
	for _, e := range efforts {
		if e == "none" {
			cfg.effort = nil
			continue
		}
		model, level, ok := strings.Cut(e, "=")
		if !ok || model == "" || !valid[level] {
			return cfg, fmt.Errorf("--mimir-effort %q: want model=<effort> with an effort the %s API accepts, or none", e, cfg.mimirAPI)
		}
		if cfg.effort == nil {
			cfg.effort = map[string]string{}
		}
		cfg.effort[model] = level
	}
	cfg.anthropic, cfg.mimirURL = strings.TrimRight(cfg.anthropic, "/"), strings.TrimRight(cfg.mimirURL, "/")
	if cfg.mimirURL == "" {
		return cfg, fmt.Errorf("%s is missing from the environment (or pass --mimir-url)", urlVar)
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

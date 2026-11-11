package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	host, anthropic, logPath, mimirURL, mimirKey string
	mimirAPI                                     string // "anthropic" (mimirURL is Mimir's /anthropic) or "openai" (mimirURL is its /v1)
	port                                         int
	prefix                                       []string
	effort                                       map[string]string         // request model -> reasoning effort sent to Mimir
	replay                                       map[string]map[string]any // openai: model -> chat_template_kwargs; listed models get earlier thinking replayed
	runaway                                      string                    // "log" (default) or "off": measure runaway replies in the request log
	maxReplyText                                 int                       // visible reply bytes from which a reply counts as a runaway
}

type gateway struct {
	cfg       config
	transport *http.Transport
	log       *reqLogger
	anth      *httputil.ReverseProxy
	mimir     *httputil.ReverseProxy
	anthURL   *url.URL
	mimirU    *url.URL
}

type ctxKey struct{}

// reqInfo follows one request from ServeHTTP through the proxy callbacks to its log line.
type reqInfo struct {
	g      *gateway
	ctx    context.Context
	route  string
	start  time.Time
	rec    map[string]any
	logIt  bool
	status int
	reqID  string
	errMsg string
	sent   int64
	fix    *fixStats
	oai    *oaiStats
	once   sync.Once
}

func (ri *reqInfo) finish() {
	ri.once.Do(func() {
		if !ri.logIt {
			return
		}
		if ri.status != 0 {
			ri.rec["status"] = ri.status
		}
		if ri.reqID != "" {
			ri.rec["request_id"] = ri.reqID
		} else {
			ri.rec["request_id"] = nil
		}
		if ri.errMsg != "" {
			ri.rec["error"] = ri.errMsg
		}
		if ri.sent > 0 {
			ri.rec["resp_bytes"] = ri.sent
		}
		if ri.fix != nil {
			if c := ri.fix.counts(); len(c) > 0 {
				ri.rec["stream_fixes"] = c
			}
		}
		if s := ri.oai; s != nil {
			if s.HaveUsage {
				ri.rec["input_tokens"], ri.rec["output_tokens"] = s.Usage.PromptTokens, s.Usage.CompletionTokens
			}
			ri.rec["reasoning_chunks"], ri.rec["tool_calls"] = s.ReasoningChunks, s.ToolCalls
			if s.Measured && ri.g.cfg.runaway != "off" {
				ri.rec["text_chars"], ri.rec["reasoning_chars"] = s.Text.Chars, s.Reasoning.Chars
				ri.rec["text_max_repeat"], ri.rec["reasoning_max_repeat"] = s.Text.MaxRepeat, s.Reasoning.MaxRepeat
				if why := s.runaway(ri.g.cfg.maxReplyText); why != "" {
					ri.rec["runaway"] = why
				}
			}
			if c := s.counts(); len(c) > 0 {
				ri.rec["stream_fixes"] = c
			}
			if s.StreamError != "" && ri.errMsg == "" {
				ri.rec["error"] = "mimir stream error: " + s.StreamError
			}
		}
		ri.rec["ms"] = time.Since(ri.start).Milliseconds()
		ri.g.log.write(ri.rec)
	})
}

// countingBody counts the response bytes it streams and closes the request's log line when the body ends. A client
// that closes the connection after the response was complete (the end of the body, or an SSE message_stop) is not an
// error; one that leaves before is logged as "client went away".
type countingBody struct {
	rc   io.ReadCloser
	ri   *reqInfo
	tail []byte
	eof  bool
}

func (c *countingBody) complete() bool {
	return c.eof || bytes.Contains(c.tail, []byte("message_stop"))
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.ri.sent += int64(n)
	if n > 0 {
		c.tail = append(c.tail, p[:n]...)
		if len(c.tail) > 256 {
			c.tail = c.tail[len(c.tail)-256:]
		}
	}
	if err == io.EOF {
		c.eof = true
	} else if err != nil && c.ri.errMsg == "" && !c.complete() {
		if c.ri.ctx.Err() != nil {
			c.ri.errMsg = "client went away"
		} else {
			c.ri.errMsg = fmt.Sprintf("upstream stream broke: %v", err)
		}
	}
	return n, err
}

func (c *countingBody) Close() error {
	err := c.rc.Close()
	if c.ri.ctx.Err() != nil && c.ri.errMsg == "" && !c.complete() {
		c.ri.errMsg = "client went away"
	}
	c.ri.finish()
	return err
}

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // never pick up proxy settings from the environment
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   30 * time.Second,
		DisableCompression:    true, // forward Accept-Encoding as the client sent it and never decode responses
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 30 * time.Minute, // a long prefill is not an error
	}
}

func newGateway(cfg config) (*gateway, error) {
	g := &gateway{cfg: cfg, transport: newTransport(), log: &reqLogger{path: cfg.logPath}}
	var err error
	if g.anthURL, err = url.Parse(cfg.anthropic); err != nil {
		return nil, err
	}
	if g.mimirU, err = url.Parse(cfg.mimirURL); err != nil {
		return nil, err
	}
	g.anth = g.newProxy(func(pr *httputil.ProxyRequest) { pr.SetURL(g.anthURL) })
	g.mimir = g.newProxy(func(pr *httputil.ProxyRequest) {
		// only what Mimir needs: the Claude login, beta flags and client fingerprints are not forwarded
		h := http.Header{}
		keep := []string{"Content-Type", "Accept", "Anthropic-Version", "User-Agent"}
		if g.cfg.mimirAPI == "openai" {
			keep = []string{"Content-Type", "User-Agent"}
		}
		for _, k := range keep {
			if v := pr.In.Header.Values(k); len(v) > 0 {
				h[k] = append([]string(nil), v...)
			}
		}
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "application/json")
		}
		if g.cfg.mimirAPI == "openai" {
			// every Messages request becomes a Chat Completions request; the client's query (?beta=true) is Anthropic's
			pr.Out.URL.Scheme, pr.Out.URL.Host = g.mimirU.Scheme, g.mimirU.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath, pr.Out.URL.RawQuery = strings.TrimRight(g.mimirU.Path, "/")+"/chat/completions", "", ""
			pr.Out.Host = ""
			h.Set("Authorization", "Bearer "+g.cfg.mimirKey)
		} else {
			pr.SetURL(g.mimirU)
			h.Set("X-Api-Key", g.cfg.mimirKey)
		}
		pr.Out.Header = h
	})
	return g, nil
}

func (g *gateway) newProxy(rewrite func(*httputil.ProxyRequest)) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite:        rewrite,
		Transport:      g.transport,
		FlushInterval:  -1,
		ModifyResponse: g.modifyResponse,
		ErrorHandler:   g.errorHandler,
	}
}

func infoOf(r *http.Request) *reqInfo {
	ri, _ := r.Context().Value(ctxKey{}).(*reqInfo)
	return ri
}

func (g *gateway) modifyResponse(resp *http.Response) error {
	ri := infoOf(resp.Request)
	if ri == nil {
		return nil
	}
	ri.status = resp.StatusCode
	ri.reqID = resp.Header.Get("request-id")
	if ri.reqID == "" {
		ri.reqID = resp.Header.Get("x-request-id")
	}
	ri.rec["ttfb_ms"] = time.Since(ri.start).Milliseconds()
	openai := ri.route == "mimir" && g.cfg.mimirAPI == "openai"
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if openai {
			st, nb := translateOpenAIError(resp.StatusCode, payload)
			resp.StatusCode, resp.Status, payload, ri.status = st, fmt.Sprintf("%d %s", st, http.StatusText(st)), nb, st
			resp.Header = http.Header{"Content-Type": {"application/json"}}
		} else if ri.route == "mimir" {
			if st, nb, changed := rewriteMimirError(resp.StatusCode, payload); changed {
				resp.StatusCode, resp.Status = st, fmt.Sprintf("%d %s", st, http.StatusText(st))
				payload = nb
				resp.Header.Set("Content-Type", "application/json")
				resp.Header.Del("Content-Encoding")
				ri.status = st
				ri.rec["error_rewritten"] = true
			}
		}
		ri.errMsg = errorMessage(payload, resp.Header.Get("Content-Encoding"))
		resp.Body = io.NopCloser(bytes.NewReader(payload))
		resp.ContentLength = int64(len(payload))
		resp.Header.Del("Content-Length")
		ri.finish()
		return nil
	}
	if openai {
		ri.oai = &oaiStats{}
		if isEventStream(resp.Header.Get("Content-Type")) {
			resp.Body = newChatStream(resp.Body, ri.oai)
			resp.Header = http.Header{"Content-Type": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
		} else {
			payload, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
			_ = resp.Body.Close()
			nb, usage, terr := fromChatCompletion(payload)
			if err != nil || terr != nil {
				st, eb := translateOpenAIError(502, []byte(fmt.Sprintf(`{"error":{"message":"could not translate Mimir's reply: %v %v"}}`, err, terr)))
				resp.StatusCode, resp.Status, ri.status, nb = st, fmt.Sprintf("%d %s", st, http.StatusText(st)), st, eb
				ri.errMsg = "untranslatable reply"
			} else {
				ri.oai.Usage, ri.oai.HaveUsage = usage, true
			}
			resp.Header = http.Header{"Content-Type": {"application/json"}}
			resp.Body = io.NopCloser(bytes.NewReader(nb))
			resp.ContentLength = int64(len(nb))
		}
		resp.Body = &countingBody{rc: resp.Body, ri: ri}
		return nil
	}
	if ri.route == "mimir" && isEventStream(resp.Header.Get("Content-Type")) && resp.Header.Get("Content-Encoding") == "" {
		ri.fix = &fixStats{}
		resp.Body = &sseFixer{rc: resp.Body, stats: ri.fix}
	}
	resp.Body = &countingBody{rc: resp.Body, ri: ri}
	return nil
}

func (g *gateway) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	ri := infoOf(r)
	if errors.Is(err, context.Canceled) {
		if ri != nil {
			ri.errMsg = "client went away"
			ri.finish()
		}
		return
	}
	route := "anthropic"
	if ri != nil {
		route = ri.route
		ri.status = http.StatusBadGateway
		ri.errMsg = fmt.Sprintf("%T: %v", err, err)
		ri.finish()
	}
	writeError(w, http.StatusBadGateway, fmt.Sprintf("router: %s upstream unreachable: %v", route, err))
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": msg}})
}

func (g *gateway) isMimir(model string) bool {
	for _, p := range g.cfg.prefix {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

func (g *gateway) health(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	mimir := "direct (" + g.mimirU.Host + ")"
	if g.cfg.mimirAPI == "openai" {
		mimir = "direct (" + g.mimirU.Host + ", openai)"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"router": "ok", "mimir": mimir, "anthropic": g.cfg.anthropic})
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodGet && path == "/health/liveliness" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode("I'm alive!")
		return
	}
	if r.Method == http.MethodGet && path == "/router/health" {
		g.health(w)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<30))
	_ = r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "router: could not read the request body: "+err.Error())
		return
	}
	var shape *requestShape
	if r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/messages") {
		shape = sniff(body, r.Header.Get("Content-Encoding"))
	}
	model := ""
	if shape != nil {
		model = shape.Model
	}
	mimir := g.isMimir(model)
	route, proxy, upstream := "anthropic", g.anth, g.anthURL
	if mimir {
		route, proxy, upstream = "mimir", g.mimir, g.mimirU
	}
	ri := &reqInfo{g: g, route: route, start: time.Now(), logIt: shape != nil}
	ri.rec = map[string]any{
		"ts": ri.start.Format("2006-01-02T15:04:05"), "ts_utc": ri.start.UTC().Format(time.RFC3339), "route": route, "method": r.Method, "path": path, "model": nilIfEmpty(model),
		"req_bytes": len(body), "session": nilIfEmpty(r.Header.Get("X-Claude-Code-Session-Id")),
		"request_class": nilIfEmpty(r.Header.Get("X-Claude-Code-Request-Class")), "agent": nilIfEmpty(r.Header.Get("X-Claude-Code-Agent-Type")),
	}
	ri.rec["upstream_host"] = upstream.Host
	if shape != nil && mimir {
		src := decodeBody(body, r.Header.Get("Content-Encoding"))
		if strings.HasSuffix(path, "/count_tokens") { // Mimir has no count_tokens: answer with a local estimate
			ri.status = http.StatusOK
			ri.rec["estimated"] = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimateTokens(src)})
			ri.finish()
			return
		}
		if g.cfg.mimirAPI == "openai" {
			ri.rec["mimir_api"] = "openai"
			translated, info, err := toChatCompletions(src, g.cfg.effort[model], g.cfg.replay[model])
			if err != nil {
				ri.status = http.StatusBadRequest
				ri.errMsg = "untranslatable request: " + err.Error()
				ri.finish()
				writeError(w, http.StatusBadRequest, "router: could not translate the request for Mimir: "+err.Error())
				return
			}
			body = translated
			if info.Effort != "" {
				ri.rec["effort"] = info.Effort
			}
			ri.rec["folded"], ri.rec["dropped_blocks"], ri.rec["hoisted"] = info.Folded, info.foldStats.DroppedBlocks+info.DroppedBlocks, info.Hoisted
			if info.Replayed > 0 {
				ri.rec["replayed_thinking"] = info.Replayed
			}
			if info.DroppedTools > 0 {
				ri.rec["dropped_tools"] = info.DroppedTools
			}
		} else if shaped, info, err := shapeMimirBody(src, g.cfg.effort[model]); err != nil {
			ri.rec["shape_error"] = err.Error() // forward as received and let Mimir answer
		} else {
			body = shaped
			if s2 := sniff(body, ""); s2 != nil {
				shape = s2
			}
			if info.Effort != "" {
				ri.rec["effort"] = info.Effort
			}
			ri.rec["folded"], ri.rec["dropped_fields"], ri.rec["dropped_blocks"], ri.rec["hoisted"] = info.Folded, info.Dropped, info.DroppedBlocks, info.Hoisted
		}
	}
	if shape != nil {
		for k, v := range facts(shape) {
			ri.rec[k] = v
		}
	}
	ri.ctx = context.WithValue(r.Context(), ctxKey{}, ri)
	r = r.WithContext(ri.ctx)
	r.Body = io.NopCloser(bytes.NewReader(body)) // the body goes on exactly as received
	r.ContentLength = int64(len(body))
	if len(body) == 0 {
		r.Body = http.NoBody
	}
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	proxy.ServeHTTP(w, r)
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

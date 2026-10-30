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
	host, anthropic, litellm, litellmKey, logPath, mimirURL, mimirKey string
	port                                                              int
	prefix                                                            []string
}

type gateway struct {
	cfg       config
	transport *http.Transport
	log       *reqLogger
	anth      *httputil.ReverseProxy
	lite      *httputil.ReverseProxy
	mimir     *httputil.ReverseProxy
	anthURL   *url.URL
	liteURL   *url.URL
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
		if ri.fix != nil && (ri.fix.ThinkingDropped > 0 || ri.fix.StopFixed > 0) {
			ri.rec["stream_fixes"] = map[string]int{"thinking_dropped": ri.fix.ThinkingDropped, "stop_reason_fixed": ri.fix.StopFixed}
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
	if g.liteURL, err = url.Parse(cfg.litellm); err != nil {
		return nil, err
	}
	g.anth = g.newProxy(func(pr *httputil.ProxyRequest) {
		pr.SetURL(g.anthURL)
		pr.Out.Header.Del("X-Litellm-Api-Key")
	})
	g.lite = g.newProxy(func(pr *httputil.ProxyRequest) {
		pr.SetURL(g.liteURL)
		// the Claude login never goes to LiteLLM or Mimir
		pr.Out.Header.Del("Authorization")
		pr.Out.Header.Del("X-Api-Key")
		pr.Out.Header.Set("X-Litellm-Api-Key", "Bearer "+g.cfg.litellmKey)
	})
	if cfg.mimirURL != "" {
		if g.mimirU, err = url.Parse(cfg.mimirURL); err != nil {
			return nil, err
		}
		g.mimir = g.newProxy(func(pr *httputil.ProxyRequest) {
			pr.SetURL(g.mimirU)
			// only what Mimir needs: the Claude login, beta flags and client fingerprints are not forwarded
			h := http.Header{}
			for _, k := range []string{"Content-Type", "Accept", "Anthropic-Version", "User-Agent"} {
				if v := pr.In.Header.Values(k); len(v) > 0 {
					h[k] = append([]string(nil), v...)
				}
			}
			if h.Get("Content-Type") == "" {
				h.Set("Content-Type", "application/json")
			}
			h.Set("X-Api-Key", g.cfg.mimirKey)
			pr.Out.Header = h
		})
	}
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
	if resp.StatusCode >= 400 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if g.mimir != nil && ri.route == "mimir" {
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
	if g.mimir != nil && ri.route == "mimir" && isEventStream(resp.Header.Get("Content-Type")) && resp.Header.Get("Content-Encoding") == "" {
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
	litellm := "ok"
	if g.mimir == nil {
		c := &http.Client{Transport: g.transport, Timeout: 3 * time.Second}
		resp, err := c.Get(g.cfg.litellm + "/health/liveliness")
		if err != nil {
			litellm = fmt.Sprintf("down (%T)", err)
		} else {
			_ = resp.Body.Close()
			if resp.StatusCode != 200 {
				litellm = fmt.Sprintf("http %d", resp.StatusCode)
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	mimir := "via litellm"
	if g.mimir != nil {
		litellm, mimir = "not used", "direct ("+g.mimirU.Host+")"
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"router": "ok", "litellm": litellm, "mimir": mimir, "anthropic": g.cfg.anthropic})
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
		route, proxy, upstream = "mimir", g.lite, g.liteURL
		if g.mimir != nil {
			proxy, upstream = g.mimir, g.mimirU
		}
	}
	ri := &reqInfo{g: g, route: route, start: time.Now(), logIt: shape != nil}
	ri.rec = map[string]any{
		"ts": time.Now().Format("2006-01-02T15:04:05"), "route": route, "method": r.Method, "path": path, "model": nilIfEmpty(model),
		"req_bytes": len(body), "session": nilIfEmpty(r.Header.Get("X-Claude-Code-Session-Id")),
		"request_class": nilIfEmpty(r.Header.Get("X-Claude-Code-Request-Class")), "agent": nilIfEmpty(r.Header.Get("X-Claude-Code-Agent-Type")),
	}
	ri.rec["upstream_host"] = upstream.Host
	if shape != nil && mimir && g.mimir != nil {
		src := decodeBody(body, r.Header.Get("Content-Encoding"))
		if strings.HasSuffix(path, "/count_tokens") { // Mimir has no count_tokens: answer with a local estimate
			ri.status = http.StatusOK
			ri.rec["estimated"] = true
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimateTokens(src)})
			ri.finish()
			return
		}
		if shaped, info, err := shapeMimirBody(src); err != nil {
			ri.rec["shape_error"] = err.Error() // forward as received and let Mimir answer
		} else {
			body = shaped
			if s2 := sniff(body, ""); s2 != nil {
				shape = s2
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

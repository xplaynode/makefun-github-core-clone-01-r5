package main

// lucky-forge: a small, fast GitHub-core clone. One Go process serves the
// REST v3 surface, the GraphQL v4 surface, an MCP endpoint and the git
// smart-HTTP protocol, all over one in-memory store.
//
// Lifecycle (the harness contract): bind an ephemeral port, POST the port
// to $MAKEFUN_ENV_URL/register, serve until GET $MAKEFUN_ENV_URL/done
// answers {"done": true}, then exit 0.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
)

var (
	store     = newStore()
	shopToken string
	reqSeq    atomic.Uint64
)

var reqPrefix string

func init() {
	var b [6]byte
	_, _ = rand.Read(b[:])
	reqPrefix = hex.EncodeToString(b[:])
}

// requestID returns a unique non-empty id without a per-request syscall.
func requestID() string {
	return fmt.Sprintf("%s-%d", reqPrefix, reqSeq.Add(1))
}

// ---- JSON writing helpers ----

type jw struct{ b bytes.Buffer }

func (w *jw) raw(s string) *jw { w.b.WriteString(s); return w }
func (w *jw) str(s string) *jw { writeJSONString(&w.b, s); return w }
func (w *jw) num(v int) *jw    { fmt.Fprintf(&w.b, "%d", v); return w }
func (w *jw) boolean(v bool) *jw {
	if v {
		w.b.WriteString("true")
	} else {
		w.b.WriteString("false")
	}
	return w
}
func (w *jw) bytes() []byte { return w.b.Bytes() }

func writeJSONString(buf *bytes.Buffer, s string) {
	enc, _ := json.Marshal(s)
	buf.Write(enc)
}

// marshalJSON is encoding/json for the less hot paths (GraphQL, MCP).
func marshalJSON(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"message":"internal error"}`)
	}
	return out
}

func sendBytes(w http.ResponseWriter, status int, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

const jsonType = "application/json; charset=utf-8"

func sendJSON(w http.ResponseWriter, status int, body []byte) {
	sendBytes(w, status, jsonType, body)
}

func sendError(w http.ResponseWriter, status int, message string) {
	var buf bytes.Buffer
	buf.WriteString(`{"message":`)
	writeJSONString(&buf, message)
	buf.WriteString("}\n")
	sendJSON(w, status, buf.Bytes())
}

// ---- request context ----

type reqCtx struct {
	w        http.ResponseWriter
	r        *http.Request
	body     []byte
	bodyRead bool
}

func (c *reqCtx) readBody() []byte {
	if !c.bodyRead {
		c.bodyRead = true
		if c.r.Body != nil {
			c.body, _ = io.ReadAll(c.r.Body)
			c.r.Body.Close()
		}
	}
	return c.body
}

// jsonBody parses the request body as a JSON object; ok=false when malformed.
func (c *reqCtx) jsonBody() (map[string]any, bool) {
	raw := c.readBody()
	if len(raw) == 0 {
		return map[string]any{}, true
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&m); err != nil {
		return nil, false
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true
}

func strField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func boolField(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

// authorised reports whether the request carries the shop token.
func authorised(r *http.Request) bool {
	if shopToken == "" {
		return true
	}
	return r.Header.Get("Authorization") == "Bearer "+shopToken
}

// ---- routing ----

func route(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-GitHub-Request-Id", requestID())
	path := r.URL.Path
	c := &reqCtx{w: w, r: r}

	switch {
	case r.Method == http.MethodGet && path == "/healthz":
		sendJSON(w, http.StatusOK, []byte("{\"ok\":true}\n"))
		return
	case path == "/api/graphql":
		handleGraphQL(c)
		return
	case path == "/mcp":
		handleMCP(c)
		return
	case strings.HasPrefix(path, "/api/v3"):
		handleREST(c)
		return
	case strings.HasSuffix(path, ".git/info/refs") || strings.HasSuffix(path, ".git/git-upload-pack") || strings.HasSuffix(path, ".git/git-receive-pack"):
		handleGit(c)
		return
	default:
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
}

// ---- lifecycle ----

func envURL() string {
	return strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")
}

func retryDelay(attempt int) time.Duration {
	d := 250 * time.Millisecond << min(attempt, 8)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

func register(env string, port int) error {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		body := fmt.Sprintf(`{"port":%d}`, port)
		resp, err := client.Post(env+"/register", "application/json", strings.NewReader(body))
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func waitForDone(env string) {
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(env + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&body)
			resp.Body.Close()
			if resp.StatusCode == 200 && body.Done {
				return
			}
		}
		time.Sleep(retryDelay(attempt))
	}
}

func main() {
	// The forge is short-lived and memory-capped; a lazier GC trades heap
	// headroom for lower latency jitter on the request path.
	debug.SetGCPercent(400)
	env := envURL()
	if env == "" {
		fmt.Fprintln(os.Stderr, "[entry] MAKEFUN_ENV_URL is not set: there is no env to register with")
		os.Exit(1)
	}
	shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "[entry] listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{
		Handler:           http.HandlerFunc(route),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	fmt.Printf("[entry] listening on port %d\n", port)

	if err := register(env, port); err != nil {
		fmt.Fprintln(os.Stderr, "[entry]", err)
		os.Exit(1)
	}
	fmt.Println("[entry] registered; waiting for the env to say done")

	waitForDone(env)
	_ = srv.Shutdown(context.Background())
	fmt.Println("[entry] done")
}

// ate-local-gateway is a local-development bridge between a host process (the RM agent's skill_engine) and Agent
// Substrate running in kind. It stands in for what an in-cluster sandbox client would do directly:
//
//   - Control API (HTTP/JSON, --control):
//     POST   /v1/actors                         {"atespace","name","template","timeout_seconds"} -> create + resume
//     POST   /v1/actors/{atespace}/{name}/resume
//     POST   /v1/actors/{atespace}/{name}/suspend
//     POST   /v1/actors/{atespace}/{name}/touch  {"timeout_seconds"} -> reset the idle timer
//     GET    /v1/actors/{atespace}/{name}
//     DELETE /v1/actors/{atespace}/{name}
//
//   - Data plane (raw TCP, --proxy): the first request on a connection must target
//     /_ate/{atespace}/{name}/{port}/{rest...}. The prefix is stripped from the request line and the connection is
//     tunnelled through atenet-router (mTLS + CONNECT) to that actor port; after that bytes flow untouched, so plain
//     HTTP, keep-alive and WebSocket upgrades all work. The router wakes a suspended actor on connect.
//
//   - Idle suspend: like E2B's auto_pause, an actor with no open tunnels for timeout_seconds is suspended.
//
//   - Egress: actors are default-deny outbound. If --egress-policy names a protojson EgressPolicy file, every actor
//     created here gets it (before its first wake), e.g. TLS passthrough to *:443 + plain HTTP to the host's /mcp.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/ateclient"
	"go.ate.dev/substrate/internal/resources"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type actorKey struct{ atespace, name string }

type idleState struct {
	open     int
	lastSeen time.Time
	timeout  time.Duration
}

type gateway struct {
	api     *ateclient.Client
	dialer  *ateclient.RouterDialer
	egress  *ateapi.EgressPolicy // applied to every actor created here; nil = none
	maxIdle time.Duration

	mu   sync.Mutex
	idle map[actorKey]*idleState
}

func main() {
	control := flag.String("control", "127.0.0.1:18700", "control API listen address")
	proxy := flag.String("proxy", "127.0.0.1:18701", "data-plane proxy listen address")
	kubeconfig := flag.String("kubeconfig", "", "kubeconfig path (default: standard loading rules)")
	kctx := flag.String("context", "kind-kind", "kubeconfig context")
	defaultTimeout := flag.Duration("default-timeout", 20*time.Minute, "idle timeout for actors created without one")
	egressFile := flag.String("egress-policy", "", "protojson EgressPolicy applied to every actor created via the control API")
	maxIdle := flag.Duration("max-idle", 0, "cap on any actor's idle timeout (0 = no cap). Each running actor holds a whole worker, so a small pool needs finished sandboxes suspended quickly")
	flag.Parse()

	var egress *ateapi.EgressPolicy
	if *egressFile != "" {
		raw, err := os.ReadFile(*egressFile)
		if err != nil {
			fail("read egress policy", err)
		}
		egress = &ateapi.EgressPolicy{}
		if err := protojson.Unmarshal(raw, egress); err != nil {
			fail("parse egress policy", err)
		}
	}

	ctx := context.Background()
	api, err := ateclient.NewClient(ctx, *kubeconfig, *kctx, "", "", false)
	if err != nil {
		fail("connect to ate-api-server", err)
	}
	dialer, err := ateclient.NewRouterDialer(ctx, *kubeconfig, *kctx)
	if err != nil {
		fail("connect to atenet-router", err)
	}
	g := &gateway{api: api, dialer: dialer, egress: egress, idle: map[actorKey]*idleState{}}

	g.maxIdle = *maxIdle
	go g.idleLoop(ctx, *defaultTimeout)
	go g.serveProxy(*proxy)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/actors", g.createActor)
	mux.HandleFunc("POST /v1/actors/{atespace}/{name}/resume", g.resumeActor)
	mux.HandleFunc("POST /v1/actors/{atespace}/{name}/suspend", g.suspendActor)
	mux.HandleFunc("POST /v1/actors/{atespace}/{name}/touch", g.touchActor)
	mux.HandleFunc("GET /v1/actors/{atespace}/{name}", g.getActor)
	mux.HandleFunc("DELETE /v1/actors/{atespace}/{name}", g.deleteActor)
	slog.Info("ate-local-gateway ready", "control", *control, "proxy", *proxy)
	fail("control API", http.ListenAndServe(*control, mux))
}

func fail(what string, err error) {
	slog.Error("fatal", "what", what, "err", err)
	os.Exit(1)
}

// ── control API ─────────────────────────────────────────────────────────────

func ref(r *http.Request) resources.ActorRef {
	return resources.ActorRef{Atespace: r.PathValue("atespace"), Name: r.PathValue("name")}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func actorJSON(a *ateapi.Actor, extra map[string]any) map[string]any {
	out := map[string]any{
		"atespace": a.GetMetadata().GetAtespace(),
		"name":     a.GetMetadata().GetName(),
		"state":    a.GetStatus().GetState().String(),
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func (g *gateway) fail(w http.ResponseWriter, op string, err error) {
	slog.Warn("control call failed", "op", op, "err", err)
	code := http.StatusBadGateway
	if status.Code(err) == codes.NotFound {
		// The actor (or its atespace) does not exist: clients treat this as gone, not as a gateway failure.
		code = http.StatusNotFound
	}
	writeJSON(w, code, map[string]any{"error": fmt.Sprintf("%s: %v", op, err)})
}

func (g *gateway) createActor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Atespace       string `json:"atespace"`
		Name           string `json:"name"`
		Template       string `json:"template"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Atespace == "" || body.Name == "" || body.Template == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "atespace, name and template are required"})
		return
	}
	start := time.Now()
	_, err := g.api.CreateActor(r.Context(), &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: body.Atespace, Name: body.Name},
		ActorTemplate: &ateapi.ObjectRef{Atespace: body.Atespace, Name: body.Template},
	}})
	if err != nil {
		g.fail(w, "CreateActor", err)
		return
	}
	if g.egress != nil {
		policy := proto.Clone(g.egress).(*ateapi.EgressPolicy)
		policy.Metadata = &ateapi.ResourceMetadata{Atespace: body.Atespace, Name: "default"} // singleton per actor
		if _, err := g.api.CreateActorEgressPolicy(r.Context(), &ateapi.CreateActorEgressPolicyRequest{
			Actor:        resources.ActorRef{Atespace: body.Atespace, Name: body.Name}.ToObjectRef(),
			EgressPolicy: policy,
		}); err != nil {
			g.fail(w, "CreateActorEgressPolicy", err)
			return
		}
	}
	created := time.Since(start)
	// Resume right away so the caller gets a running sandbox, like E2B's create. A full pool ("no worker has room")
	// frees up as idle actors get suspended, so wait for it rather than failing the create.
	var resp *ateapi.ResumeActorResponse
	for deadline := time.Now().Add(90 * time.Second); ; {
		resp, err = g.api.ResumeActor(r.Context(), &ateapi.ResumeActorRequest{
			Actor: resources.ActorRef{Atespace: body.Atespace, Name: body.Name}.ToObjectRef(),
		})
		if err == nil || !strings.Contains(err.Error(), "no worker has room") || time.Now().After(deadline) {
			break
		}
		slog.Info("pool full, waiting for a worker", "actor", body.Atespace+"/"+body.Name)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		g.fail(w, "ResumeActor", err)
		return
	}
	g.touch(actorKey{body.Atespace, body.Name}, time.Duration(body.TimeoutSeconds)*time.Second)
	writeJSON(w, http.StatusOK, actorJSON(resp.GetActor(), map[string]any{
		"create_ms": created.Milliseconds(), "resume_ms": (time.Since(start) - created).Milliseconds(),
	}))
}

func (g *gateway) resumeActor(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	resp, err := g.api.ResumeActor(r.Context(), &ateapi.ResumeActorRequest{Actor: ref(r).ToObjectRef()})
	if err != nil {
		g.fail(w, "ResumeActor", err)
		return
	}
	g.touch(actorKey{r.PathValue("atespace"), r.PathValue("name")}, 0)
	writeJSON(w, http.StatusOK, actorJSON(resp.GetActor(), map[string]any{
		"resumed": resp.GetResumed(), "resume_ms": time.Since(start).Milliseconds(),
	}))
}

func (g *gateway) suspendActor(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	resp, err := g.api.SuspendActor(r.Context(), &ateapi.SuspendActorRequest{Actor: ref(r).ToObjectRef()})
	if err != nil {
		g.fail(w, "SuspendActor", err)
		return
	}
	writeJSON(w, http.StatusOK, actorJSON(resp.GetActor(), map[string]any{"suspend_ms": time.Since(start).Milliseconds()}))
}

func (g *gateway) touchActor(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	g.touch(actorKey{r.PathValue("atespace"), r.PathValue("name")}, time.Duration(body.TimeoutSeconds)*time.Second)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (g *gateway) getActor(w http.ResponseWriter, r *http.Request) {
	a, err := g.api.GetActor(r.Context(), &ateapi.GetActorRequest{Actor: ref(r).ToObjectRef()})
	if err != nil {
		g.fail(w, "GetActor", err)
		return
	}
	writeJSON(w, http.StatusOK, actorJSON(a, nil))
}

func (g *gateway) deleteActor(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	a, err := g.api.DeleteActor(r.Context(), &ateapi.DeleteActorRequest{Actor: ref(r).ToObjectRef()})
	if err != nil {
		g.fail(w, "DeleteActor", err)
		return
	}
	g.mu.Lock()
	delete(g.idle, actorKey{r.PathValue("atespace"), r.PathValue("name")})
	g.mu.Unlock()
	writeJSON(w, http.StatusOK, actorJSON(a, map[string]any{"delete_ms": time.Since(start).Milliseconds()}))
}

// ── idle suspend ────────────────────────────────────────────────────────────

func (g *gateway) touch(k actorKey, timeout time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.idle[k]
	if s == nil {
		s = &idleState{}
		g.idle[k] = s
	}
	s.lastSeen = time.Now()
	if timeout > 0 {
		s.timeout = timeout
	}
}

func (g *gateway) connOpened(k actorKey) {
	g.touch(k, 0)
	g.mu.Lock()
	g.idle[k].open++
	g.mu.Unlock()
}

func (g *gateway) connClosed(k actorKey) {
	g.mu.Lock()
	if s := g.idle[k]; s != nil {
		s.open--
		s.lastSeen = time.Now()
	}
	g.mu.Unlock()
}

func (g *gateway) idleLoop(ctx context.Context, def time.Duration) {
	for range time.Tick(5 * time.Second) {
		var due []actorKey
		g.mu.Lock()
		for k, s := range g.idle {
			t := s.timeout
			if t == 0 {
				t = def
			}
			if g.maxIdle > 0 && t > g.maxIdle {
				t = g.maxIdle
			}
			if s.open == 0 && time.Since(s.lastSeen) > t {
				due = append(due, k)
			}
		}
		for _, k := range due {
			delete(g.idle, k) // re-registered on the next touch / connection
		}
		g.mu.Unlock()
		for _, k := range due {
			_, err := g.api.SuspendActor(ctx, &ateapi.SuspendActorRequest{
				Actor: resources.ActorRef{Atespace: k.atespace, Name: k.name}.ToObjectRef(),
			})
			slog.Info("idle suspend", "actor", k.atespace+"/"+k.name, "err", err)
		}
	}
}

// ── data plane ──────────────────────────────────────────────────────────────

func (g *gateway) serveProxy(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fail("proxy listen", err)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			slog.Warn("accept", "err", err)
			continue
		}
		go g.handleConn(c)
	}
}

// readHead reads the request head (through the blank line) without consuming any body bytes beyond what bufio buffered.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head bytes.Buffer
	for {
		line, err := br.ReadSlice('\n')
		head.Write(line)
		if err != nil {
			return nil, err
		}
		if len(line) <= 2 && strings.TrimRight(string(line), "\r\n") == "" {
			return head.Bytes(), nil
		}
		if head.Len() > 64<<10 {
			return nil, errors.New("request head too large")
		}
	}
}

func badRequest(c net.Conn, msg string) {
	fmt.Fprintf(c, "HTTP/1.1 400 Bad Request\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
}

func (g *gateway) handleConn(c net.Conn) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 64<<10)
	head, err := readHead(br)
	if err != nil {
		return
	}
	// Request line: METHOD /_ate/{atespace}/{name}/{port}/rest HTTP/1.1
	lineEnd := bytes.Index(head, []byte("\r\n"))
	parts := strings.SplitN(string(head[:lineEnd]), " ", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[1], "/_ate/") {
		badRequest(c, "path must start with /_ate/{atespace}/{name}/{port}/")
		return
	}
	seg := strings.SplitN(strings.TrimPrefix(parts[1], "/_ate/"), "/", 4)
	if len(seg) < 3 {
		badRequest(c, "path must start with /_ate/{atespace}/{name}/{port}/")
		return
	}
	port, err := strconv.Atoi(seg[2])
	if err != nil {
		badRequest(c, "bad port")
		return
	}
	rest := "/"
	if len(seg) == 4 {
		rest += seg[3]
	}
	k := actorKey{seg[0], seg[1]}
	newHead := append([]byte(parts[0]+" "+rest+" "+parts[2]), head[lineEnd:]...)

	start := time.Now()
	up, err := g.dialer.DialActor(context.Background(), resources.ActorRef{Atespace: k.atespace, Name: k.name}, port)
	if err != nil {
		msg := fmt.Sprintf("dial %s/%s:%d: %v", k.atespace, k.name, port, err)
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(msg), msg)
		return
	}
	defer up.Close()
	slog.Debug("tunnel open", "actor", k.atespace+"/"+k.name, "port", port, "dial_ms", time.Since(start).Milliseconds())
	g.connOpened(k)
	defer g.connClosed(k)

	if _, err := up.Write(newHead); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, br); done <- struct{}{} }() // br still holds any buffered body bytes
	go func() { _, _ = io.Copy(c, up); done <- struct{}{} }()
	// When either side finishes, close both: the router tunnel has no reliable half-close, and a lingering tunnel
	// would keep the actor counted as busy (blocking idle suspend).
	<-done
	_ = up.Close()
	_ = c.Close()
	<-done
}

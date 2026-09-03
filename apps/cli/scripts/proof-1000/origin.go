package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const maxSeenRequests = 200_000

// originBody is what the local origin answers on every path. The harness
// generated ServerNonce at startup, so a fetch that returns it proves the
// platform reached this process; RequestID is minted per request and
// recorded, so the fetched body can be matched to the exact request the
// origin saw for that Host.
type originBody struct {
	ServerNonce string `json:"server_nonce"`
	RequestID   string `json:"request_id"`
	Host        string `json:"host"`
	Path        string `json:"path"`
	Seq         int64  `json:"seq"`
}

type originStats struct {
	Total  int            `json:"total_requests"`
	ByHost map[string]int `json:"requests_by_host"`
}

// origin is the single loopback HTTP server every proof share targets.
type origin struct {
	nonce  string
	port   int
	server *http.Server

	mu     sync.Mutex
	seq    int64
	byHost map[string]int
	// seen and older form a two-generation window: a request id stays
	// answerable for at least maxSeenRequests further requests, so a fetch in
	// flight across a generation swap is never reported as unseen.
	seen  map[string]string
	older map[string]string
}

func startOrigin(ctx context.Context, port int) (*origin, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	o := &origin{nonce: hex.EncodeToString(nonce), port: port, byHost: map[string]int{}, seen: map[string]string{}}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		o.port = addr.Port
	}
	o.server = &http.Server{Handler: o, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = o.server.Serve(listener) }()
	return o, nil
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		http.Error(w, "nonce", http.StatusInternalServerError)
		return
	}
	body := originBody{ServerNonce: o.nonce, RequestID: hex.EncodeToString(id), Host: r.Host, Path: r.URL.Path}
	o.mu.Lock()
	o.seq++
	body.Seq = o.seq
	o.byHost[r.Host]++
	if len(o.seen) >= maxSeenRequests {
		o.older, o.seen = o.seen, map[string]string{}
	}
	o.seen[body.RequestID] = r.Host
	o.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func (o *origin) targetURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(o.port)
}

func (o *origin) sawRequest(requestID string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	host, ok := o.seen[requestID]
	if !ok {
		host, ok = o.older[requestID]
	}
	return host, ok
}

func (o *origin) snapshot() originStats {
	o.mu.Lock()
	defer o.mu.Unlock()
	stats := originStats{Total: int(o.seq), ByHost: make(map[string]int, len(o.byHost))}
	for host, count := range o.byHost {
		stats.ByHost[host] = count
	}
	return stats
}

func (o *origin) close() error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := o.server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

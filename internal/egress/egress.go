// Package egress is the agent container's only way out: an HTTP CONNECT proxy that opens a
// tunnel to an allowed host name on port 443 and to nothing else. The container itself sits on
// an internal Docker network with no route outside; the proxy sits on both networks.
package egress

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Proxy tunnels CONNECT requests to allowed hosts.
type Proxy struct {
	// Allow: "chatgpt.com" lets that host only; ".openai.com" lets every host under it.
	Allow []string
	Port  string // the only port a tunnel may reach; "" means 443
	Log   *slog.Logger
	// Dial opens the upstream connection; nil dials the network, names resolved by DNS (below).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// DNS servers (host:port) to ask directly; nil uses the system's. Docker's embedded resolver
	// now and then answers "server misbehaving", hence the retries; public servers asked
	// directly from Docker Desktop do not answer at all, hence the system's by default.
	DNS []string
}

// Allowed: a host name (not an IP address, which would skip the list) on the list.
func (p *Proxy) Allowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil {
		return false
	}
	for _, a := range p.Allow {
		a = strings.ToLower(a)
		if strings.HasPrefix(a, ".") {
			if strings.HasSuffix(host, a) {
				return true
			}
		} else if host == a {
			return true
		}
	}
	return false
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	if r.Method != http.MethodConnect {
		log.Warn("denied", "method", r.Method, "url", r.URL.String(), "why", "only CONNECT")
		http.Error(w, "only CONNECT to allowed hosts", http.StatusForbidden)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	want := p.Port
	if want == "" {
		want = "443"
	}
	if err != nil || port != want || !p.Allowed(host) {
		log.Warn("denied", "host", r.Host, "why", "not on the list")
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}
	dial := p.Dial
	if dial == nil {
		d := net.Dialer{Timeout: 10 * time.Second, Resolver: p.resolver()}
		dial = d.DialContext
	}
	var up net.Conn
	for try := 0; try < 3; try++ { // a lost DNS answer is worth another go
		if up, err = dial(r.Context(), "tcp", r.Host); err == nil || r.Context().Err() != nil {
			break
		}
		time.Sleep(time.Duration(300*(try+1)) * time.Millisecond)
	}
	if err != nil {
		log.Warn("upstream", "host", r.Host, "err", err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	down, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	log.Info("tunnel", "host", r.Host)
	down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // bytes the client sent along with the request are in buf
		defer wg.Done()
		io.Copy(up, buf)
		closeWrite(up)
	}()
	go func() {
		defer wg.Done()
		io.Copy(down, up)
		closeWrite(down)
	}()
	wg.Wait()
	up.Close()
	down.Close()
}

// resolver asks p.DNS in turn; nil when there are none.
func (p *Proxy) resolver() *net.Resolver {
	if len(p.DNS) == 0 {
		return nil
	}
	var n int
	var mu sync.Mutex
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		mu.Lock()
		server := p.DNS[n%len(p.DNS)]
		n++
		mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, server)
	}}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
}

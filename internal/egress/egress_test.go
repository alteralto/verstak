package egress

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAllowed(t *testing.T) {
	p := &Proxy{Allow: []string{"chatgpt.com", ".openai.com"}}
	for host, want := range map[string]bool{
		"chatgpt.com":          true,
		"ChatGPT.com.":         true,
		"api.openai.com":       true,
		"auth.openai.com":      true,
		"openai.com":           false, // ".openai.com" is the hosts under it
		"evilopenai.com":       false,
		"chatgpt.com.evil.org": false,
		"www.chatgpt.com":      false,
		"example.com":          false,
		"104.18.33.45":         false, // an address skips the list
		"::1":                  false,
		"":                     false,
	} {
		if got := p.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// echo is an upstream that answers each line with "echo: " and the line.
func echo(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					io.WriteString(c, "echo: "+sc.Text()+"\n")
				}
			}()
		}
	}()
	return l.Addr().String()
}

func proxy(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	up := echo(t)
	var dialed []string
	p := &Proxy{
		Allow: []string{"chatgpt.com"},
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			var d net.Dialer
			return d.DialContext(ctx, network, up) // every allowed host is the echo server
		},
	}
	s := httptest.NewServer(p)
	t.Cleanup(s.Close)
	return s, &dialed
}

func connect(t *testing.T, s *httptest.Server, req string) (*bufio.Reader, net.Conn, string) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(s.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	io.WriteString(c, req)
	br := bufio.NewReader(c)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return br, c, status
}

func TestTunnelToAnAllowedHost(t *testing.T) {
	s, dialed := proxy(t)
	br, c, status := connect(t, s, "CONNECT chatgpt.com:443 HTTP/1.1\r\nHost: chatgpt.com:443\r\n\r\n")
	if !strings.Contains(status, "200") {
		t.Fatalf("status %q", status)
	}
	br.ReadString('\n') // the empty line after the status
	io.WriteString(c, "hello\n")
	line, _ := br.ReadString('\n')
	if line != "echo: hello\n" {
		t.Errorf("through the tunnel: %q", line)
	}
	if len(*dialed) != 1 || (*dialed)[0] != "chatgpt.com:443" {
		t.Errorf("dialed %v", *dialed)
	}
}

func TestDenied(t *testing.T) {
	s, dialed := proxy(t)
	for _, req := range []string{
		"CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n",
		"CONNECT chatgpt.com:22 HTTP/1.1\r\nHost: chatgpt.com:22\r\n\r\n",
		"CONNECT 127.0.0.1:443 HTTP/1.1\r\nHost: 127.0.0.1:443\r\n\r\n",
		"GET http://chatgpt.com/ HTTP/1.1\r\nHost: chatgpt.com\r\n\r\n",
	} {
		_, _, status := connect(t, s, req)
		if !strings.Contains(status, "403") {
			t.Errorf("%q: status %q, want 403", strings.SplitN(req, "\r\n", 2)[0], status)
		}
	}
	if len(*dialed) != 0 {
		t.Errorf("dialed %v for denied requests", *dialed)
	}
}

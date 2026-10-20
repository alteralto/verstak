// Command egress is the allow-list proxy of the agent's container (package egress):
// egress -listen :3128 -allow chatgpt.com,.openai.com
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/alteralto/verstak/internal/egress"
)

func main() {
	listen := flag.String("listen", ":3128", "address to listen on")
	allow := flag.String("allow", "", "comma-separated hosts; a leading dot allows the hosts under it")
	dns := flag.String("dns", "", "comma-separated DNS servers to ask directly (host:port); empty uses the system's")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	p := &egress.Proxy{Allow: strings.Split(*allow, ","), Log: log}
	if *dns != "" {
		p.DNS = strings.Split(*dns, ",")
	}
	log.Info("egress up", "listen", *listen, "allow", p.Allow)
	srv := &http.Server{Addr: *listen, Handler: p, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("egress", "err", err)
		os.Exit(1)
	}
}

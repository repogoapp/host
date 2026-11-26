// Command tunnelgateway serves `<slug>.repogo.dev` for RepoGo hosts. Each host
// keeps one WebSocket open at `tunnels.repogo.dev/connect`, proves its key,
// and answers the requests for the tunnels repogo.app opened for it. Fly ends
// TLS in front of it, so tunnels are not end-to-end encrypted.
package main

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/repogoapp/host/internal/tunnelgateway"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	domain := cmp.Or(os.Getenv("TUNNEL_DOMAIN"), "repogo.dev")
	upstashURL, upstashToken := os.Getenv("UPSTASH_TUNNEL_REST_URL"), os.Getenv("UPSTASH_TUNNEL_REST_TOKEN")
	if upstashURL == "" || upstashToken == "" {
		log.Error("UPSTASH_TUNNEL_REST_URL and UPSTASH_TUNNEL_REST_TOKEN are required")
		os.Exit(2)
	}

	srv := &tunnelgateway.Server{
		Domain:    domain,
		GatewayID: cmp.Or(os.Getenv("GATEWAY_ID"), "tunnels."+domain),
		Registry:  tunnelgateway.NewUpstash(upstashURL, upstashToken),
		Hub:       tunnelgateway.NewHub(log),
		Cache:     tunnelgateway.NewResponseCache(),
		Limits:    tunnelgateway.NewRateLimits(3000, 1200, time.Minute),
		Log:       log,
	}
	httpSrv := &http.Server{
		Addr:    ":" + cmp.Or(os.Getenv("PORT"), "8080"),
		Handler: srv.Handler(),
		// No read timeout: host sockets and visitor WebSockets stay open.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", httpSrv.Addr, "domain", domain)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

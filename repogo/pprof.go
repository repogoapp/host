package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
)

// pprofEnvVar names a loopback address to serve Go profiles on. Dev only:
// scripts/restart.sh sets it, and a release host leaves it unset.
const pprofEnvVar = "REPOGO_PPROF"

// servePprof serves the profiles on addr until the process exits. It refuses
// anything but loopback, since heap and goroutine dumps show chat content.
func servePprof(log *slog.Logger, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s: %w", pprofEnvVar, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("%s: %q is not a loopback address", pprofEnvVar, addr)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s: %w", pprofEnvVar, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	log.Info("pprof listening", "addr", "http://"+listener.Addr().String()+"/debug/pprof/")
	go func() {
		err := http.Serve(listener, mux)
		log.Warn("pprof stopped", "err", err)
	}()
	return nil
}

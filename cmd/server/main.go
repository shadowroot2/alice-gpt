package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"alice-gpt/internal/app"
	"github.com/redis/go-redis/v9"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		c := http.Client{Timeout: time.Second}
		r, e := c.Get("http://127.0.0.1:8080/readyz")
		if e != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			os.Exit(1)
		}
		return
	}
	if err := run(log); err != nil {
		log.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := app.LoadConfig()
	if err != nil {
		return err
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return err
	}
	opts.DialTimeout = 300 * time.Millisecond
	opts.ReadTimeout = 250 * time.Millisecond
	opts.WriteTimeout = 250 * time.Millisecond
	opts.MaxRetries = -1
	opts.ContextTimeoutEnabled = true
	client := redis.NewClient(opts)
	defer client.Close()
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: (&net.Dialer{Timeout: time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, MaxIdleConns: 32, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: cfg.GPTBackgroundTimeout}
	gpt := &app.OpenAI{Key: cfg.APIKey, Model: cfg.Model, URL: "https://api.openai.com/v1/responses", Instructions: cfg.Role, Client: &http.Client{Transport: transport, Timeout: cfg.GPTBackgroundTimeout}}
	appServer := app.NewServer(cfg, &app.RedisStore{Client: client, TTL: cfg.SessionTTL}, gpt, log)
	handler := appServer.Handler()
	srv := &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 4200 * time.Millisecond, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	serveErr := serve(ctx, srv, listener, log)
	waitCtx, cancel := context.WithTimeout(context.Background(), cfg.GPTBackgroundTimeout+2*time.Second)
	defer cancel()
	if err := appServer.Wait(waitCtx); err != nil && serveErr == nil {
		return err
	}
	return serveErr
}

func serve(ctx context.Context, srv *http.Server, listener net.Listener, log *slog.Logger) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	log.Info("server started", "addr", listener.Addr().String())
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
			<-serveErr
			return err
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		log.Info("shutdown complete")
		return nil
	}
}

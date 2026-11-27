package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fuzirao-png/cosco-ai-e-booking/internal/growth"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if err := growth.LoadEnv(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := growth.OpenStore(ctx, growth.EnvOr("GROWTH_DB_PATH", "runtime/growth.sqlite3"))
	if err != nil {
		return err
	}
	defer store.Close()

	mode := growth.EnvOr("GROWTH_EMBEDDINGS", "local")
	embedder, err := growth.EmbeddingsFromEnv(mode)
	if err != nil {
		return err
	}
	index, err := growth.NewIndex(ctx, embedder)
	if err != nil {
		return err
	}
	app := growth.NewServer(growth.Agent{Store: store, Index: index}, growth.ModelFromEnv("GROWTH_MODEL"), mode,
		growth.EnvOr("GROWTH_STATIC_DIR", "frontend/dist"))
	server := &http.Server{
		Addr: growth.EnvOr("GROWTH_ADDR", "127.0.0.1:8000"), Handler: app,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); app.RunWorker(ctx) }()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Print(err)
		}
	}()
	log.Printf("E-Booking Growth Agent listening on http://%s", server.Addr)
	err = server.ListenAndServe()
	stop()
	<-shutdownDone
	<-workerDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

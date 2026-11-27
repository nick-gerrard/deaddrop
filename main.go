package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	idleRoomTTL     = 48 * time.Hour
	cleanupInterval = time.Hour
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := env("DEADDROP_ADDR", "127.0.0.1:8080")
	dbPath := env("DEADDROP_DB", "deaddrop.db")

	store, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer store.Close()

	app, err := NewApp(store)
	if err != nil {
		log.Fatalf("init app: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go cleanupLoop(ctx, store, app.createLimiter)

	srv := &http.Server{
		Addr:              addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("deaddrop listening on %s (db %s)", addr, dbPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// cleanupLoop deletes rooms idle for longer than idleRoomTTL and prunes stale
// rate-limit entries, once an hour.
func cleanupLoop(ctx context.Context, store *Store, rl *rateLimiter) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if rl != nil {
				rl.Prune()
			}
			n, err := store.DeleteIdleRooms(ctx, idleRoomTTL)
			if err != nil {
				log.Printf("cleanup: %v", err)
			} else if n > 0 {
				log.Printf("cleanup: removed %d idle rooms", n)
			}
		}
	}
}

package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"chat/internal/server"
)

func main() {
	addr    := flag.String("addr", ":8080", "TCP address to listen on")
	dataDir := flag.String("data", "./data", "directory for persistent storage")
	workers := flag.Int("workers", 4, "number of message-persistence worker goroutines")
	flag.Parse()

	srv, err := server.New(*dataDir, *workers)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-quit
		log.Println("[server] shutting down…")
		srv.Shutdown()
	}()

	if err := srv.ListenAndServe(*addr); err != nil {
		log.Printf("[server] stopped: %v", err)
	}
}

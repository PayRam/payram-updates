package http

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/payram/payram-updater/internal/config"
	"github.com/payram/payram-updater/internal/jobs"
)

// Server represents the HTTP server.
type Server struct {
	httpServer *http.Server
	port       int
	config     *config.Config
	jobStore   *jobs.Store
}

// New creates a new HTTP server instance.
func New(cfg *config.Config, jobStore *jobs.Store) *Server {
	s := &Server{
		port:     cfg.Port,
		config:   cfg,
		jobStore: jobStore,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", HandleHealth())
	mux.HandleFunc("/upgrade/status", s.HandleUpgradeStatus())
	mux.HandleFunc("/upgrade/logs", s.HandleUpgradeLogs())
	mux.HandleFunc("/upgrade", s.HandleUpgrade())

	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	s.httpServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	return s
}

// Start starts the HTTP server and blocks until shutdown.
// It handles graceful shutdown on SIGINT and SIGTERM.
func (s *Server) Start() error {
	// Create a channel to listen for shutdown signals
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Create a channel to capture server errors
	serverErrors := make(chan error, 1)

	// Start the server in a goroutine
	go func() {
		log.Printf("Starting HTTP server on 127.0.0.1:%d", s.port)

		// Use a listener to ensure we bind only to 127.0.0.1
		listener, err := net.Listen("tcp", s.httpServer.Addr)
		if err != nil {
			serverErrors <- fmt.Errorf("failed to create listener: %w", err)
			return
		}

		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			serverErrors <- fmt.Errorf("HTTP server error: %w", err)
		}
	}()

	// Wait for either a signal or server error
	select {
	case err := <-serverErrors:
		return err
	case sig := <-stop:
		log.Printf("Received signal %v, initiating graceful shutdown", sig)
	}

	// Graceful shutdown with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown error: %w", err)
	}

	log.Println("Server stopped gracefully")
	return nil
}

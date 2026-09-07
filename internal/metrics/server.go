package metrics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tirante-dev/gascurve/internal/logger"
)

const (
	// serverReadHeaderTimeout bounds a slow request line and headers.
	serverReadHeaderTimeout = 10 * time.Second
	// serverShutdownTimeout bounds the graceful stop.
	serverShutdownTimeout = 5 * time.Second
)

// Server serves the exposition format and nothing else. The collector has
// no HTTP server of its own, and this one deliberately shares nothing with
// the follower goroutines: it reads the registry, so a follower stuck on an
// RPC call or on the database still scrapes.
type Server struct {
	ln  net.Listener
	srv *http.Server
	log *logger.Logger
}

// NewServer binds addr and prepares the handler. Binding here rather than
// in Run means a port already in use is reported at startup instead of
// leaving the process running unscraped. ctx bounds the bind alone; Run
// takes the context the server lives by.
func NewServer(ctx context.Context, addr string, g prometheus.Gatherer, log *logger.Logger) (*Server, error) {
	if log == nil {
		log = logger.Nop()
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.Handle(Path, Handler(g))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	return &Server{
		ln:  ln,
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: serverReadHeaderTimeout},
		log: log,
	}, nil
}

// ListenAddr is the address the server bound, useful when the port was 0.
func (s *Server) ListenAddr() string { return s.ln.Addr().String() }

// Run serves until ctx ends, then shuts down. A closed server is not an
// error.
func (s *Server) Run(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() {
		err := s.srv.Serve(s.ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()
	s.log.Info("serving metrics", "addr", s.ListenAddr(), "path", Path)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
	defer cancel()
	if err := s.srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("metrics shutdown: %w", err)
	}
	return <-errc
}

// Addr builds the listen address for a port, on every interface.
func Addr(port int) string { return net.JoinHostPort("", strconv.Itoa(port)) }

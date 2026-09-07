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
	// serverWriteTimeout bounds a scrape whose client stops reading, so a stalled reader cannot pin a
	// handler goroutine for the life of the process.
	serverWriteTimeout = 30 * time.Second
	// serverShutdownTimeout bounds the graceful stop.
	serverShutdownTimeout = 5 * time.Second
)

// Server serves the exposition format and optional health routes. It deliberately shares nothing with
// the follower goroutines: metrics read the registry and health reads the collector monitor, so a
// follower stuck on an RPC call or on the database still gets an answer.
type Server struct {
	ln  net.Listener
	srv *http.Server
	log *logger.Logger
}

// NewServer binds addr and prepares the handler. Binding here rather than in Run means a port already in
// use is reported before Run is reached, and the caller decides whether that is fatal. ctx bounds the
// bind alone.
func NewServer(ctx context.Context, addr string, g prometheus.Gatherer, log *logger.Logger, extra ...http.Handler) (*Server, error) {
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
	if len(extra) > 0 && extra[0] != nil {
		mux.Handle("/", extra[0])
	} else {
		mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "not found", http.StatusNotFound)
		})
	}
	return &Server{
		ln:  ln,
		srv: &http.Server{Handler: mux, ReadHeaderTimeout: serverReadHeaderTimeout, WriteTimeout: serverWriteTimeout},
		log: log,
	}, nil
}

// ListenAddr is the address the server bound, useful when the port was 0.
func (s *Server) ListenAddr() string { return s.ln.Addr().String() }

// Run serves until ctx ends, then shuts down. A closed server is not an error.
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
		// The graceful stop ran out of time and a connection is still being served. Close it rather than
		// returning while its goroutine runs on.
		_ = s.srv.Close()
		<-errc
		return fmt.Errorf("metrics shutdown: %w", err)
	}
	return <-errc
}

func Addr(port int) string { return net.JoinHostPort("", strconv.Itoa(port)) }

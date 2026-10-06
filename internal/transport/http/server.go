package httptransport

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/config"
)

type Server struct {
	http     *http.Server
	listener net.Listener
	done     chan struct{}
}

func NewServer(lc fx.Lifecycle, c config.Config, mux *http.ServeMux, log *slog.Logger, shutdown fx.Shutdowner) *Server {
	s := &Server{http: &http.Server{
		Addr: c.HTTPAddress, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}, done: make(chan struct{})}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", c.HTTPAddress)
			if err != nil {
				return err
			}
			s.listener = listener
			go func() {
				defer close(s.done)
				if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("HTTP server stopped unexpectedly")
					_ = shutdown.Shutdown(fx.ExitCode(1))
				}
			}()
			log.Info("HTTP server started", "address", listener.Addr().String())
			return nil
		},
		OnStop: func(ctx context.Context) error {
			err := s.http.Shutdown(ctx)
			if err != nil {
				_ = s.http.Close()
			}
			select {
			case <-s.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			log.Info("HTTP server stopped")
			return err
		},
	})
	return s
}

func (s *Server) Address() string { return s.listener.Addr().String() }

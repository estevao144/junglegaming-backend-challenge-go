package app

import (
	"time"

	"go.uber.org/fx"
	"jungle-gaming/internal/config"
	"jungle-gaming/internal/platform/logging"
	"jungle-gaming/internal/platform/messaging"
	"jungle-gaming/internal/platform/postgres"
	httptransport "jungle-gaming/internal/transport/http"
)

var Module = fx.Module("application", config.Module, logging.Module, postgres.Module, messaging.Module, httptransport.Module)

func New(options ...fx.Option) *fx.App {
	return fx.New(append([]fx.Option{Module, fx.WithLogger(logging.Events), fx.StartTimeout(30 * time.Second), fx.StopTimeout(15 * time.Second)}, options...)...)
}

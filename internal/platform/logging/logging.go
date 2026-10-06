package logging

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"
	"jungle-gaming/internal/config"
)

var Module = fx.Module("logging", fx.Provide(New))

func Events(log *slog.Logger) fxevent.Logger {
	return &fxevent.SlogLogger{Logger: log}
}

func New(c config.Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: c.LogLevel}))
}

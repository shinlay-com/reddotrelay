package logging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"

	"reddotrelay/internal/config"
)

func New(cfg config.LogConfig) (*slog.Logger, error) {
	return NewWithWriter(cfg, os.Stdout)
}

func NewWithWriter(cfg config.LogConfig, output io.Writer) (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.ToUpper(cfg.Level))); err != nil {
		return nil, err
	}
	options := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch cfg.Format {
	case "json":
		handler = slog.NewJSONHandler(output, options)
	case "text":
		handler = slog.NewTextHandler(output, options)
	default:
		return nil, errors.New("log.format must be json or text")
	}
	return slog.New(handler), nil
}

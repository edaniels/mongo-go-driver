package mongolog

import (
	"context"

	"go.uber.org/zap"
)

type ctxKey int

// Context keys used for logging
const (
	LoggerCtxKey ctxKey = iota
)

// FromContext tries to obtain a logger from the given context.
func FromContext(ctx context.Context) (*zap.SugaredLogger, bool) {
	logger := ctx.Value(LoggerCtxKey)
	if logger == nil {
		return nil, false
	}

	return logger.(*zap.SugaredLogger), true
}

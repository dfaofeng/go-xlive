package subscriber

import (
	"context"
	"sync"

	"go.uber.org/zap"
)

// NatsSubscriber does not handle NATS subscriptions anymore
type NatsSubscriber struct {
	logger *zap.Logger
}

// NewNatsSubscriber creates an empty NatsSubscriber instance
func NewNatsSubscriber(logger *zap.Logger) *NatsSubscriber {
	return &NatsSubscriber{
		logger: logger.Named("nats_subscriber_noop"), // Mark as no-op
	}
}

// Start no longer starts any subscriptions
func (ns *NatsSubscriber) Start(ctx context.Context, wg *sync.WaitGroup) {
	ns.logger.Info("NATS subscriber (Aggregation) is disabled, performing no action.")
	// Do not start goroutine, do not call wg.Add(1)
}

// Shutdown no longer needs to perform any action
func (ns *NatsSubscriber) Shutdown() {
	ns.logger.Info("NATS subscriber (Aggregation) shutdown (no-op).")
}

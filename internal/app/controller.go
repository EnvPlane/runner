package app

import (
	"context"
	"log/slog"
	"time"
)

type LifecycleController struct {
	service       *EnvironmentService
	interval      time.Duration
	idleThreshold time.Duration
	logger        *slog.Logger
}

func NewLifecycleController(service *EnvironmentService, interval time.Duration, logger *slog.Logger, idleThreshold ...time.Duration) *LifecycleController {
	if interval <= 0 {
		interval = time.Minute
	}
	var threshold time.Duration
	if len(idleThreshold) > 0 {
		threshold = idleThreshold[0]
	}
	return &LifecycleController{service: service, interval: interval, idleThreshold: threshold, logger: logger}
}

func (c *LifecycleController) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			unpinned, err := c.service.ReconcilePins()
			if err != nil {
				c.logger.Error("pin reconciliation failed", "error", err)
				continue
			}
			for _, env := range unpinned {
				c.logger.Info("temporary pin expired", "id", env.ID)
			}
			deleted, err := c.service.ReconcileExpired(ctx)
			if err != nil {
				c.logger.Error("ttl reconciliation failed", "error", err)
				continue
			}
			for _, env := range deleted {
				c.logger.Info("expired environment deleted", "id", env.ID)
			}
			idle, err := c.service.ReconcileIdle(c.idleThreshold)
			if err != nil {
				c.logger.Error("idle reconciliation failed", "error", err)
				continue
			}
			for _, env := range idle {
				c.logger.Info("environment marked idle", "id", env.ID)
			}
			runtime := c.service.runtimeSettings()
			autoDelete := true
			if runtime.AutoDeleteIdleEnvs != nil {
				autoDelete = *runtime.AutoDeleteIdleEnvs
			}
			if autoDelete {
				shutdown, err := c.service.ShutdownIdle(ctx)
				if err != nil {
					c.logger.Error("idle shutdown failed", "error", err)
					continue
				}
				for _, env := range shutdown {
					c.logger.Info("idle environment shutdown started", "id", env.ID)
				}
			}
		}
	}
}

// Command kibitz-scaler sizes the worker from the queue it drains.
//
// A worker pool has no inbound traffic to scale on -- that is the point of it
// -- so its instance count is set rather than derived. This reads the
// subscription's backlog from Cloud Monitoring and writes the instance count
// that backlog calls for, which is what lets the worker sit at zero between
// reviews.
//
// It reconciles once and exits, which is how it runs as a Cloud Run job on a
// Cloud Scheduler trigger. With -loop it stays up and reconciles on an
// interval instead, for running it anywhere else.
//
// See docs/deployment.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/scale"
	"github.com/yteraoka/kibitz/internal/telemetry"
)

// version is set at build time with -ldflags.
var version = "dev"

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "kibitz-scaler: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	loop := flag.Bool("loop", false, "keep running and reconcile every KIBITZ_SCALE_INTERVAL")
	flag.Parse()

	cfg, err := config.LoadScaler(config.OSEnv)
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}

	logger := telemetry.Setup(os.Stdout, telemetry.Options{
		Level:   cfg.Log.Level,
		Format:  cfg.Log.Format,
		Service: "kibitz-scaler",
		Version: version,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backlog, err := scale.NewPubSubBacklog(ctx, cfg.Queue.PubSub.ProjectID, cfg.Queue.PubSub.Subscription)
	if err != nil {
		return err
	}
	worker, err := scale.NewCloudRunWorkerPool(ctx, cfg.Scale.ProjectID, cfg.Scale.Region, cfg.Scale.WorkerPool)
	if err != nil {
		return err
	}

	scaler := &scale.Scaler{
		Source: backlog,
		Target: worker,
		Policy: scale.Policy{
			Min:         cfg.Scale.MinInstances,
			Max:         cfg.Scale.MaxInstances,
			PerInstance: cfg.Scale.MessagesPerInstance,
			IdleAfter:   cfg.Scale.IdleAfter,
		},
		Logger: logger,
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "starting",
		slog.String("subscription", backlog.String()),
		slog.String("worker_pool", worker.String()),
		slog.Int("min_instances", cfg.Scale.MinInstances),
		slog.Int("max_instances", cfg.Scale.MaxInstances),
		slog.Duration("idle_after", cfg.Scale.IdleAfter),
		slog.Bool("loop", *loop),
	)

	if *loop {
		return scaler.Run(ctx, cfg.Scale.Interval)
	}

	result, err := scaler.Reconcile(ctx)
	if err != nil {
		return err
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "reconciled", slog.String("result", result.String()))
	return nil
}

// Command kibitz-scaler sizes the worker from the queue it drains.
//
// A worker pool has no inbound traffic to scale on -- that is the point of it
// -- so its instance count is set rather than derived. This reads the
// subscription's backlog from Cloud Monitoring and writes the instance count
// that backlog calls for, which is what lets the worker sit at zero between
// reviews.
//
// With -serve it listens for POST /reconcile and reconciles once per request,
// which is how it runs on Cloud Run: a service that Cloud Scheduler calls and
// that is billed only while it answers. With -loop it stays up and reconciles
// on an interval, for running it anywhere else. With neither it reconciles
// once and exits.
//
// See docs/deployment.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/yteraoka/kibitz/internal/config"
	"github.com/yteraoka/kibitz/internal/httpx"
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
	serve := flag.Bool("serve", false, "listen on KIBITZ_LISTEN_ADDR and reconcile once per POST /reconcile")
	flag.Parse()
	if *loop && *serve {
		return fmt.Errorf("-loop and -serve are alternatives; pick one")
	}

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
		slog.Bool("serve", *serve),
	)

	if *serve {
		health := httpx.NewHealth(version)
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", health.Live)
		mux.Handle("POST /reconcile", &reconcileHandler{scaler: scaler, logger: logger})
		srv := &http.Server{
			Addr: cfg.ListenAddr,
			Handler: httpx.Chain(mux,
				httpx.RequestID,
				httpx.Recover(logger),
				httpx.Logging(logger),
			),
			ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		}
		return httpx.Serve(ctx, logger, "http", srv, cfg.ShutdownTimeout)
	}

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

package main

import (
	"context"
	"email-delivery-platform/internal/edp"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

var version = "development"

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	cfg, e := edp.LoadConfig()
	if e != nil {
		slog.Error("configuration", "error", e)
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	app, e := edp.New(ctx, cfg, version)
	if e != nil {
		slog.Error("startup", "error", e)
		os.Exit(1)
	}
	defer app.Close()
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "migrate":
		e = app.Migrate(ctx)
	case "seed":
		e = app.Seed(ctx, false)
	case "seed-demo":
		if !cfg.Dev {
			e = fmt.Errorf("seed-demo requires EDP_ENV=development")
		} else {
			e = app.Seed(ctx, true)
		}
	case "serve":
		e = app.Serve(ctx)
	case "worker":
		e = app.Work(ctx)
	case "reset-root":
		e = app.ResetRoot(ctx)
	default:
		e = fmt.Errorf("unknown command %q", cmd)
	}
	if e != nil {
		slog.Error("command failed", "command", cmd, "error", e)
		os.Exit(1)
	}
}

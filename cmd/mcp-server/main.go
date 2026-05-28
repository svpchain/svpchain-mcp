package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-server: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "path to TOML config file (required)")
	flag.Parse()
	if *cfgPath == "" {
		return fmt.Errorf("--config is required")
	}

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := BuildServer(ctx, cfg)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	defer srv.Close()

	return srv.Run(ctx)
}

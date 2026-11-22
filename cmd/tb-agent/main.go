// Command tb-agent is the Tossbox node agent.
//
//	tb-agent [-config /etc/tb-agent/node.yaml]          run the agent
//	tb-agent check [-config /etc/tb-agent/node.yaml]    validate config, print capabilities
//	tb-agent version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/finn-freitag/tb-agent/internal/agent"
	"github.com/finn-freitag/tb-agent/internal/config"
	"github.com/finn-freitag/tb-agent/internal/version"
)

func main() {
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("tb-agent", flag.ExitOnError)
	cfgPath := fs.String("config", envOr("TB_AGENT_CONFIG", "/etc/tb-agent/node.yaml"), "path to node config")
	_ = fs.Parse(args)

	switch cmd {
	case "version":
		fmt.Println("tb-agent", version.Version)
		return
	case "run", "check":
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (use run, check or version)\n", cmd)
		os.Exit(2)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	level := slog.LevelInfo
	if cfg.LogLevel == "debug" {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	a, err := agent.New(cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	if cmd == "check" {
		check(cfg, a)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("tb-agent starting", "version", version.Version, "node", cfg.Name, "dashboard", cfg.Dashboard.Endpoint, "drivers", cfg.DriverNames())
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "err", err)
		os.Exit(1)
	}
}

func check(cfg *config.Config, a *agent.Agent) {
	fmt.Printf("node:           %s\n", cfg.Name)
	fmt.Printf("dashboard:      %s (token set: %v)\n", cfg.Dashboard.Endpoint, cfg.Dashboard.Token != "")
	fmt.Printf("data_dir:       %s\n", cfg.DataDir)
	fmt.Printf("listen_address: %s\n", cfg.ListenAddress)
	fmt.Printf("limits:         ram=%d MB cpu=%d%% storage=%d GB (0 = unlimited)\n", cfg.RAMLimitMB, cfg.CPULimitPct, cfg.StorageLimitGB)
	fmt.Printf("drivers:        %v\n", cfg.DriverNames())
	c, err := net.DialTimeout("tcp", cfg.Dashboard.Endpoint, 5*time.Second)
	if err != nil {
		fmt.Printf("dashboard reachable: NO (%v)\n", err)
	} else {
		c.Close()
		fmt.Println("dashboard reachable: yes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, _ := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(a.Capabilities(ctx))
	fmt.Printf("capabilities:\n%s\n", b)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

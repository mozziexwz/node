package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mozziexwz/node/internal/buildinfo"
	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayfirewall"
	"github.com/mozziexwz/node/internal/relayruntime"
)

func main() {
	showVersion := flag.Bool("version", false, "Print the agent build version")
	capability := flag.String("capability", "executor", "executor, relay, relay-recovery, relay-enrollment-check, relay-health, relay-firewall, or relay-firewall-cleanup")
	server := flag.String("server", os.Getenv("MSBOOST_SERVER_URL"), "Control-plane HTTPS origin")
	state := flag.String("state-dir", "/var/lib/msboost-agent", "Relay runtime state directory")
	gost := flag.String("gost-binary", "/usr/local/bin/gost", "Pinned local GOST v3 binary for relay capability")
	offlinePolicy := flag.String("offline-policy", "keep_last", "Relay offline policy: keep_last (only supported mode)")
	recoveryAction := flag.String("recovery-action", "", "Local root recovery: snapshot or adopt")
	recoveryFile := flag.String("recovery-file", "", "Private snapshot output or trusted plan input file (never put tokens in arguments)")
	healthInvocation := flag.String("health-invocation", "", "Expected systemd InvocationID for a fresh authenticated sync")
	flag.Parse()
	if *showVersion {
		fmt.Println(buildinfo.Version)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch *capability {
	case "relay-firewall", "relay-firewall-cleanup":
		err = relayfirewall.Run(ctx, *capability == "relay-firewall-cleanup")
	case "relay-enrollment-check":
		err = relayruntime.CheckRelayEnrollment(ctx, *server, os.Getenv("MSBOOST_RELAY_ENROLLMENT_TOKEN"))
	case "relay-health":
		err = relayruntime.CheckRelayHealth(*state, *healthInvocation, *server)
	case "relay-recovery":
		err = relayruntime.RunV2RecoveryClient(ctx, *state, *recoveryAction, *recoveryFile)
	case "executor":
		e := executor.NewEngine()
		e.GostAMD64URL = os.Getenv("GOST_AMD64_URL")
		e.GostAMD64SHA256 = os.Getenv("GOST_AMD64_SHA256")
		e.GostARM64URL = os.Getenv("GOST_ARM64_URL")
		e.GostARM64SHA256 = os.Getenv("GOST_ARM64_SHA256")
		err = executor.Run(ctx, *server, os.Getenv("MSBOOST_EXECUTOR_TOKEN"), e)
	case "relay":
		err = relayruntime.Run(ctx, relayruntime.Config{ServerURL: *server, EnrollmentToken: os.Getenv("MSBOOST_RELAY_ENROLLMENT_TOKEN"), StateDir: *state, GostBinary: *gost, OfflinePolicy: *offlinePolicy, Version: buildinfo.Version, FirewallStatusPath: os.Getenv("MSBOOST_RELAY_FIREWALL_STATUS")})
	default:
		log.Fatal("unsupported capability; use --help for supported local roles")
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

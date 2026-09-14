package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mozziexwz/node/internal/executor"
	"github.com/mozziexwz/node/internal/relayruntime"
)

func main() {
	capability := flag.String("capability", "executor", "executor, relay, or local-root relay-recovery (separate tokens and machines recommended)")
	server := flag.String("server", os.Getenv("MSBOOST_SERVER_URL"), "Control-plane HTTPS origin")
	state := flag.String("state-dir", "/var/lib/msboost-agent", "Relay runtime state directory")
	gost := flag.String("gost-binary", "/usr/local/bin/gost", "Pinned local GOST v3 binary for relay capability")
	offlinePolicy := flag.String("offline-policy", "lease", "Relay policy: lease (v1) or explicitly enabled keep_last (v2)")
	recoveryAction := flag.String("recovery-action", "", "Local root recovery: snapshot or adopt")
	recoveryFile := flag.String("recovery-file", "", "Private snapshot output or trusted plan input file (never put tokens in arguments)")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch *capability {
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
		err = relayruntime.Run(ctx, relayruntime.Config{ServerURL: *server, EnrollmentToken: os.Getenv("MSBOOST_RELAY_ENROLLMENT_TOKEN"), StateDir: *state, GostBinary: *gost, OfflinePolicy: *offlinePolicy})
	default:
		log.Fatal("unsupported capability: use executor, relay, or relay-recovery")
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

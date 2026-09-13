package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mozziexwz/node/internal/control"
)

var version = "0.2.2-dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reinstall := env("REINSTALL_SCRIPT", "installers/reinstall/reinstall.sh")
	expected := os.Getenv("REINSTALL_SHA256")
	if expected == "" {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(reinstall), "manifest.json"))
		if err == nil {
			var manifest struct {
				ExecutionSHA256 string `json:"executionSHA256"`
			}
			if json.Unmarshal(raw, &manifest) == nil {
				expected = manifest.ExecutionSHA256
			}
		}
	}
	if raw, err := os.ReadFile(reinstall); err == nil {
		sum := sha256.Sum256(raw)
		if expected == "" || hex.EncodeToString(sum[:]) != expected {
			log.Fatal("reinstall script checksum missing or mismatched")
		}
	} else {
		log.Fatal("required pinned reinstall script cannot be read")
	}
	cfg := control.Config{Version: version, DataDir: env("DATA_DIR", "data"), DatabaseURL: os.Getenv("DATABASE_URL"), PublicURL: env("PUBLIC_URL", "http://127.0.0.1:8080"), MasterKey: os.Getenv("MASTER_KEY"), AdminEmail: os.Getenv("ADMIN_EMAIL"), AdminPassword: os.Getenv("ADMIN_PASSWORD"), NodeScript: env("NODE_SCRIPT", "installers/node/msboost.sh"), ReinstallScript: reinstall, ReinstallSHA256: expected, SecureCookies: os.Getenv("COOKIE_SECURE") == "true"}
	if cfg.DatabaseURL == "" && os.Getenv("DATABASE_HOST") != "" {
		databaseURL := url.URL{Scheme: "postgres", User: url.UserPassword(env("DATABASE_USER", "msboost"), os.Getenv("POSTGRES_PASSWORD")), Host: net.JoinHostPort(os.Getenv("DATABASE_HOST"), env("DATABASE_PORT", "5432")), Path: "/" + env("DATABASE_NAME", "msboost"), RawQuery: "sslmode=" + url.QueryEscape(env("DATABASE_SSLMODE", "require"))}
		cfg.DatabaseURL = databaseURL.String()
	}
	for _, cidr := range strings.Split(os.Getenv("TRUSTED_PROXY_CIDRS"), ",") {
		if strings.TrimSpace(cidr) != "" {
			cfg.TrustedProxyCIDRs = append(cfg.TrustedProxyCIDRs, strings.TrimSpace(cidr))
		}
	}
	app, err := control.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()
	mux := http.NewServeMux()
	app.RegisterIdentity(mux)
	app.RegisterCommerce(mux)
	app.RegisterRelay(mux)
	app.RegisterContent(mux)
	tasks := control.NewTaskService(app)
	tasks.Register(mux)
	tasks.Start(ctx)
	app.SetFrontProvisioner(tasks.ProvisionFront)
	backups := control.NewBackupService(app)
	backups.Register(mux)
	backups.Start(ctx)
	app.StartCommerce(ctx)
	app.StartRelay(ctx)
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		control.WriteJSON(w, 200, map[string]any{"status": "ok", "version": version})
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { control.Fail(w, 404, "接口不存在") })
	webDir := env("WEB_DIR", "apps/web/dist")
	files := http.FileServer(http.Dir(webDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", 405)
			return
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://challenges.cloudflare.com; style-src 'self' 'unsafe-inline'; img-src 'self' https: data: blob:; connect-src 'self'; frame-src https://challenges.cloudflare.com; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Frame-Options", "DENY")
		p := filepath.Clean(r.URL.Path)
		if strings.HasPrefix(p, "..") {
			http.NotFound(w, r)
			return
		}
		if _, err := os.Stat(filepath.Join(webDir, p)); err != nil {
			r.URL.Path = "/"
		}
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
	server := &http.Server{Addr: env("LISTEN_ADDR", "127.0.0.1:8080"), Handler: app.Authenticate(mux), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, WriteTimeout: 10 * time.Minute, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	log.Printf("MSBOOST %s listening on %s", version, server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

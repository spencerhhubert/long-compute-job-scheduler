package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/httpapi"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: lcjs <server|token|version>")
	}
	switch arguments[0] {
	case "server":
		return runServer(arguments[1:])
	case "token":
		return runToken(arguments[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q; usage: lcjs <server|token|version>", arguments[0])
	}
}

func runServer(arguments []string) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8080", "HTTP listen address")
	database := flags.String("db", "data/control.db", "SQLite database path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}

	token := os.Getenv("LCJS_BOOTSTRAP_TOKEN")
	if len(token) < 32 {
		return errors.New("LCJS_BOOTSTRAP_TOKEN must contain at least 32 characters; generate one with 'lcjs token'")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := sqlitestore.Open(ctx, *database)
	if err != nil {
		return err
	}
	defer store.Close()
	handler, err := httpapi.New(store, token)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}
	errChannel := make(chan error, 1)
	go func() {
		slog.Info("control plane listening", "address", *listen)
		errChannel <- server.ListenAndServe()
	}()

	select {
	case err := <-errChannel:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdownContext)
	}
}

func runToken(arguments []string) error {
	if len(arguments) == 0 {
		token, err := generateSecret("")
		if err != nil {
			return err
		}
		fmt.Println(token)
		return nil
	}
	if arguments[0] != "create" {
		return errors.New("usage: lcjs token [create --db PATH --name NAME]")
	}
	flags := flag.NewFlagSet("token create", flag.ContinueOnError)
	database := flags.String("db", "data/control.db", "SQLite database path")
	name := flags.String("name", "browser", "operator token name")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: lcjs token create [--db PATH] [--name NAME]")
	}
	rawToken, err := generateSecret("lcjs_")
	if err != nil {
		return err
	}
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, *database)
	if err != nil {
		return err
	}
	defer store.Close()
	if _, err := store.CreateAPIToken(ctx, *name, rawToken); err != nil {
		return err
	}
	// The raw token is intentionally returned exactly once. SQLite stores only
	// its SHA-256 hash.
	fmt.Println(rawToken)
	return nil
}

func generateSecret(prefix string) (string, error) {
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
}

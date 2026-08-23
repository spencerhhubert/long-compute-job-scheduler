package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
	"github.com/spencerhhubert/long-compute-job-scheduler/internal/httpapi"
	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
	workeragent "github.com/spencerhhubert/long-compute-job-scheduler/internal/worker"
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
	case "worker":
		return runWorker(arguments[1:])
	case "supervise":
		return runSupervise(arguments[1:])
	case "metric":
		return runMetric(arguments[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		return fmt.Errorf("unknown command %q; usage: lcjs <server|token|version>", arguments[0])
	}
}

func runMetric(arguments []string) error {
	flags := flag.NewFlagSet("metric", flag.ContinueOnError)
	name := flags.String("name", "", "metric series name")
	value := flags.Float64("value", 0, "scalar metric value")
	step := flags.Int64("step", -1, "optional monotonically increasing step")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *name == "" {
		return errors.New("usage: lcjs metric --name NAME --value NUMBER [--step NUMBER]")
	}
	path := os.Getenv("LCJS_METRICS_FILE")
	if path == "" {
		return errors.New("LCJS_METRICS_FILE is not set; this command must run inside an LCJS attempt")
	}
	sample := domain.MetricSample{Name: *name, Value: *value, ObservedAt: time.Now().UTC()}
	if *step >= 0 {
		sample.Step = step
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open metrics file: %w", err)
	}
	encodeErr := json.NewEncoder(file).Encode(sample)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(encodeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("append metric: %w", err)
	}
	return nil
}

type labelFlags map[string]string

func (labels *labelFlags) String() string {
	parts := make([]string, 0, len(*labels))
	for name, value := range *labels {
		parts = append(parts, name+"="+value)
	}
	return strings.Join(parts, ",")
}

func (labels *labelFlags) Set(raw string) error {
	name, value, found := strings.Cut(raw, "=")
	if !found || strings.TrimSpace(name) == "" || strings.TrimSpace(value) == "" {
		return errors.New("labels must use name=value")
	}
	if *labels == nil {
		*labels = make(map[string]string)
	}
	(*labels)[strings.TrimSpace(name)] = strings.TrimSpace(value)
	return nil
}

func runWorker(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: lcjs worker <create|run>")
	}
	switch arguments[0] {
	case "create":
		return runWorkerCreate(arguments[1:])
	case "run":
		return runWorkerAgent(arguments[1:])
	default:
		return errors.New("usage: lcjs worker <create|run>")
	}
}

func runWorkerCreate(arguments []string) error {
	flags := flag.NewFlagSet("worker create", flag.ContinueOnError)
	database := flags.String("db", "data/control.db", "control-plane SQLite database path")
	name := flags.String("name", "", "worker name")
	var labels labelFlags
	flags.Var(&labels, "label", "worker label in name=value form; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*name) == "" {
		return errors.New("usage: lcjs worker create --db PATH --name NAME [--label name=value]")
	}
	rawToken, err := generateSecret("lcjw_")
	if err != nil {
		return err
	}
	ctx := context.Background()
	store, err := sqlitestore.Open(ctx, *database)
	if err != nil {
		return err
	}
	defer store.Close()
	credential, err := store.CreateWorker(ctx, *name, rawToken, labels)
	if err != nil {
		return err
	}
	// This output is suitable for a permission-restricted worker environment
	// file. The control plane stores only the token hash.
	fmt.Printf("LCJS_WORKER_ID=%s\nLCJS_WORKER_TOKEN=%s\n", credential.WorkerID, rawToken)
	return nil
}

func runWorkerAgent(arguments []string) error {
	flags := flag.NewFlagSet("worker run", flag.ContinueOnError)
	serverURL := flags.String("server", "", "HTTPS control-plane URL")
	database := flags.String("db", "data/worker.db", "worker SQLite database path")
	workRoot := flags.String("work-root", "data/work", "isolated job work root")
	artifactRoot := flags.String("artifact-root", "data/artifacts", "artifact storage root")
	cpu := flags.Uint("cpu", uint(runtime.NumCPU()), "allocatable logical CPUs")
	memoryBytes := flags.Uint64("memory-bytes", 0, "allocatable memory bytes; zero is unspecified")
	gpus := flags.Uint("gpus", 0, "allocatable physical GPUs")
	gpuSharedSlots := flags.Uint("gpu-shared-slots", 0, "total explicitly enabled shared GPU slots")
	maxParallel := flags.Uint("max-parallel", 1, "maximum simultaneous attempts")
	var labels labelFlags
	flags.Var(&labels, "label", "worker label in name=value form; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *serverURL == "" {
		return errors.New("usage: lcjs worker run --server URL [--db PATH] [--work-root PATH] [--artifact-root PATH]")
	}
	workerID := os.Getenv("LCJS_WORKER_ID")
	token := os.Getenv("LCJS_WORKER_TOKEN")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	agent, err := workeragent.New(ctx, workeragent.Config{
		ServerURL: *serverURL, WorkerID: workerID, Token: token, Version: version,
		DatabasePath: *database, WorkRoot: *workRoot, ArtifactRoot: *artifactRoot,
		Capacity: domain.WorkerCapacity{
			CPU: uint32(*cpu), MemoryBytes: *memoryBytes, GPUCount: uint32(*gpus),
			GPUSharedSlots: uint32(*gpuSharedSlots), MaxParallel: uint32(*maxParallel), Labels: labels,
		},
	})
	if err != nil {
		return err
	}
	defer agent.Close()
	slog.Info("worker agent started", "worker_id", workerID, "server", *serverURL)
	return agent.Run(ctx)
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

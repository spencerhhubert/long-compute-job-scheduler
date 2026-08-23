package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spencerhhubert/long-compute-job-scheduler/internal/domain"
)

func runJob(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: lcjs job <submit|get|list|cancel|wait>")
	}
	switch arguments[0] {
	case "submit":
		return runJobSubmit(arguments[1:])
	case "get":
		return runJobGet(arguments[1:], false)
	case "list":
		return runJobList(arguments[1:])
	case "cancel":
		return runJobCancel(arguments[1:])
	case "wait":
		return runJobGet(arguments[1:], true)
	default:
		return errors.New("usage: lcjs job <submit|get|list|cancel|wait>")
	}
}

func runJobSubmit(arguments []string) error {
	flags := flag.NewFlagSet("job submit", flag.ContinueOnError)
	server := flags.String("server", "", "control-plane HTTPS URL")
	filePath := flags.String("file", "", "job JSON file, or - for stdin")
	idempotencyKey := flags.String("idempotency-key", "", "stable key for this logical submission")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *server == "" || *filePath == "" || *idempotencyKey == "" {
		return errors.New("usage: lcjs job submit --server URL --file JOB.json --idempotency-key KEY")
	}
	var reader io.Reader
	if *filePath == "-" {
		reader = io.LimitReader(os.Stdin, (1<<20)+1)
	} else {
		file, err := os.Open(*filePath)
		if err != nil {
			return err
		}
		defer file.Close()
		reader = io.LimitReader(file, (1<<20)+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("job specification exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var spec domain.JobSpec
	if err := decoder.Decode(&spec); err != nil {
		return fmt.Errorf("decode job specification: %w", err)
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("validate job specification: %w", err)
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return operatorRequest(context.Background(), http.MethodPost, *server, "/api/v1/jobs", canonical, map[string]string{"Idempotency-Key": *idempotencyKey})
}

func runJobGet(arguments []string, wait bool) error {
	name := "job get"
	if wait {
		name = "job wait"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	server := flags.String("server", "", "control-plane HTTPS URL")
	jobID := flags.String("id", "", "job ID")
	interval := flags.Duration("interval", 5*time.Second, "wait polling interval")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *server == "" || *jobID == "" || (wait && *interval < time.Second) {
		return fmt.Errorf("usage: lcjs %s --server URL --id JOB_ID", name)
	}
	for {
		body, err := operatorResponse(context.Background(), http.MethodGet, *server, "/api/v1/jobs/"+url.PathEscape(*jobID), nil, nil)
		if err != nil {
			return err
		}
		if !wait {
			_, err = os.Stdout.Write(append(body, '\n'))
			return err
		}
		var detail domain.JobDetail
		if err := json.Unmarshal(body, &detail); err != nil {
			return err
		}
		switch detail.Job.State {
		case domain.JobSucceeded, domain.JobFailed, domain.JobCanceled:
			_, err = os.Stdout.Write(append(body, '\n'))
			return err
		}
		time.Sleep(*interval)
	}
}

func runJobList(arguments []string) error {
	flags := flag.NewFlagSet("job list", flag.ContinueOnError)
	server := flags.String("server", "", "control-plane HTTPS URL")
	limit := flags.Int("limit", 100, "maximum jobs")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *server == "" || *limit < 1 || *limit > 1000 {
		return errors.New("usage: lcjs job list --server URL [--limit 100]")
	}
	return operatorRequest(context.Background(), http.MethodGet, *server, fmt.Sprintf("/api/v1/jobs?limit=%d", *limit), nil, nil)
}

func runJobCancel(arguments []string) error {
	flags := flag.NewFlagSet("job cancel", flag.ContinueOnError)
	server := flags.String("server", "", "control-plane HTTPS URL")
	jobID := flags.String("id", "", "job ID")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *server == "" || *jobID == "" {
		return errors.New("usage: lcjs job cancel --server URL --id JOB_ID")
	}
	return operatorRequest(context.Background(), http.MethodPost, *server, "/api/v1/jobs/"+url.PathEscape(*jobID)+"/cancel", nil, nil)
}

func operatorRequest(ctx context.Context, method, server, path string, body []byte, headers map[string]string) error {
	response, err := operatorResponse(ctx, method, server, path, body, headers)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(response, '\n'))
	return err
}

func operatorResponse(ctx context.Context, method, server, path string, body []byte, headers map[string]string) ([]byte, error) {
	parsed, err := url.Parse(strings.TrimRight(server, "/"))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost") {
		return nil, errors.New("operator server URL must use HTTPS outside loopback development")
	}
	token := os.Getenv("LCJS_TOKEN")
	if len(token) < 32 {
		return nil, errors.New("LCJS_TOKEN must contain an operator bearer token")
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(server, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("control plane returned %s: %s", response.Status, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

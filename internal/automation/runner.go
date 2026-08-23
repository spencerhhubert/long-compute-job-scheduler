// Package automation evaluates durable control-plane policies and delivers
// their signed webhook notifications.
package automation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

type Store interface {
	EvaluateHealthPolicies(context.Context) (int, error)
	DueWebhookDeliveries(context.Context, int) ([]sqlitestore.WebhookDelivery, error)
	RecordWebhookResult(context.Context, string, int, string) error
}

type Runner struct {
	store  Store
	client *http.Client
}

func New(store Store) *Runner {
	return &Runner{
		store: store,
		client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := r.Cycle(ctx); err != nil && ctx.Err() == nil {
			slog.Error("automation cycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) Cycle(ctx context.Context) error {
	if _, err := r.store.EvaluateHealthPolicies(ctx); err != nil {
		return fmt.Errorf("evaluate health policies: %w", err)
	}
	deliveries, err := r.store.DueWebhookDeliveries(ctx, 20)
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		responseCode, deliveryError := r.deliver(ctx, delivery)
		errorMessage := ""
		if deliveryError != nil {
			errorMessage = deliveryError.Error()
		}
		if err := r.store.RecordWebhookResult(ctx, delivery.ID, responseCode, errorMessage); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runner) deliver(ctx context.Context, delivery sqlitestore.WebhookDelivery) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.URL, bytes.NewReader(delivery.Payload))
	if err != nil {
		return 0, err
	}
	mac := hmac.New(sha256.New, delivery.Secret)
	_, _ = mac.Write(delivery.Payload)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "lcjs-webhook/1")
	request.Header.Set("X-LCJS-Delivery-ID", delivery.ID)
	request.Header.Set("X-LCJS-Event-ID", delivery.FiringID)
	request.Header.Set("X-LCJS-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := r.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("webhook returned %s", response.Status)
	}
	return response.StatusCode, nil
}

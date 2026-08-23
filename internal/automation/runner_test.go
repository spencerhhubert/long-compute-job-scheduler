package automation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlitestore "github.com/spencerhhubert/long-compute-job-scheduler/internal/store/sqlite"
)

type fakeStore struct {
	deliveries   []sqlitestore.WebhookDelivery
	evaluations  int
	resultID     string
	responseCode int
	resultError  string
}

func (s *fakeStore) EvaluateHealthPolicies(context.Context) (int, error) {
	s.evaluations++
	return 1, nil
}

func (s *fakeStore) DueWebhookDeliveries(context.Context, int) ([]sqlitestore.WebhookDelivery, error) {
	return s.deliveries, nil
}

func (s *fakeStore) RecordWebhookResult(_ context.Context, id string, responseCode int, resultError string) error {
	s.resultID, s.responseCode, s.resultError = id, responseCode, resultError
	return nil
}

func TestRunnerSignsAndRecordsWebhookDelivery(t *testing.T) {
	payload := []byte(`{"event_id":"hlf_test","kind":"health_policy_fired"}`)
	secret := []byte("lcjh_0123456789abcdef0123456789abcdef0123456789abc")
	received := false
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		received = true
		if request.Header.Get("X-LCJS-Delivery-ID") != "dlv_test" || request.Header.Get("X-LCJS-Event-ID") != "hlf_test" {
			t.Errorf("delivery headers = %v", request.Header)
		}
		mac := hmac.New(sha256.New, secret)
		_, _ = mac.Write(payload)
		wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if request.Header.Get("X-LCJS-Signature") != wantSignature {
			t.Errorf("signature = %q, want %q", request.Header.Get("X-LCJS-Signature"), wantSignature)
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store := &fakeStore{deliveries: []sqlitestore.WebhookDelivery{{
		ID: "dlv_test", FiringID: "hlf_test", TargetName: "session", URL: server.URL,
		Secret: secret, Payload: payload,
	}}}
	runner := New(store)
	if err := runner.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !received || store.evaluations != 1 || store.resultID != "dlv_test" || store.responseCode != http.StatusNoContent || store.resultError != "" {
		t.Fatalf("runner result = received %v, store %+v", received, store)
	}
}

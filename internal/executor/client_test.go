package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResultOutboxSurvivesOneFiveFifteenMinuteOutages(t *testing.T) {
	for _, outage := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute} {
		t.Run(outage.String(), func(t *testing.T) {
			start := time.Now()
			var elapsed atomic.Int64
			var attempts, acks atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				if time.Duration(elapsed.Load()) < outage {
					w.WriteHeader(503)
					return
				}
				acks.Add(1)
				w.WriteHeader(200)
			}))
			defer server.Close()
			p := resultRetryPolicy{maxAge: 30 * time.Minute, now: func() time.Time { return start.Add(time.Duration(elapsed.Load())) }, wait: func(_ context.Context, d time.Duration) bool { elapsed.Add(int64(d)); return true }}
			if err := deliverResult(context.Background(), server.Client(), server.URL, "test-token", []byte(`{"id":"single-result"}`), p); err != nil {
				t.Fatal(err)
			}
			if attempts.Load() <= 5 || acks.Load() != 1 {
				t.Fatalf("outbox abandoned/duplicated: attempts=%d acks=%d", attempts.Load(), acks.Load())
			}
		})
	}
}

func TestResultOutboxExpiresAndDoesNotTreatConflictAsAck(t *testing.T) {
	for _, status := range []int{503, 409, 401, 400} {
		now := time.Now()
		var attempts atomic.Int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { attempts.Add(1); w.WriteHeader(status) }))
		p := resultRetryPolicy{maxAge: time.Minute, now: func() time.Time { return now }, wait: func(_ context.Context, d time.Duration) bool { now = now.Add(d); return true }}
		err := deliverResult(context.Background(), server.Client(), server.URL, "test-token", []byte(`{}`), p)
		server.Close()
		if err == nil {
			t.Fatalf("status %d accepted as ACK", status)
		}
		if status != 503 && attempts.Load() != 1 {
			t.Fatal("permanent rejection retried")
		}
	}
}

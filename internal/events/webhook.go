package events

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

type Webhook struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

type WebhookRegistry struct {
	mu       sync.RWMutex
	hooks    map[string]*Webhook
	bus      *Bus
	log      *slog.Logger
	client   *http.Client
	workCh   chan deliveryJob
	stopOnce sync.Once
	stopCh   chan struct{}
}

type deliveryJob struct {
	hook  *Webhook
	event Event
}

func NewWebhookRegistry(bus *Bus, log *slog.Logger) *WebhookRegistry {
	r := &WebhookRegistry{
		hooks:  make(map[string]*Webhook),
		bus:    bus,
		log:    log,
		client: &http.Client{Timeout: 10 * time.Second},
		workCh: make(chan deliveryJob, 1000),
		stopCh: make(chan struct{}),
	}
	bus.Subscribe(r.dispatch)
	for i := 0; i < 10; i++ {
		go r.worker()
	}
	return r
}

func (r *WebhookRegistry) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
}

func (r *WebhookRegistry) Register(url, secret string) *Webhook {
	w := &Webhook{
		ID:     uuid.New().String(),
		URL:    url,
		Secret: secret,
	}
	r.mu.Lock()
	r.hooks[w.ID] = w
	r.mu.Unlock()
	return w
}

// RegisterIfNew registers a webhook only if no webhook with the same URL exists.
// Returns the existing or newly created webhook.
func (r *WebhookRegistry) RegisterIfNew(url, secret string) *Webhook {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, w := range r.hooks {
		if w.URL == url {
			return w
		}
	}
	w := &Webhook{
		ID:     uuid.New().String(),
		URL:    url,
		Secret: secret,
	}
	r.hooks[w.ID] = w
	return w
}

func (r *WebhookRegistry) Unregister(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.hooks[id]; ok {
		delete(r.hooks, id)
		return true
	}
	return false
}

func (r *WebhookRegistry) List() []*Webhook {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Webhook, 0, len(r.hooks))
	for _, w := range r.hooks {
		out = append(out, w)
	}
	return out
}

func (r *WebhookRegistry) dispatch(e Event) {
	r.mu.RLock()
	hooks := make([]*Webhook, 0, len(r.hooks))
	for _, w := range r.hooks {
		hooks = append(hooks, w)
	}
	r.mu.RUnlock()
	for _, w := range hooks {
		select {
		case r.workCh <- deliveryJob{hook: w, event: e}:
		case <-r.stopCh:
			return
		default:
			r.log.Warn("webhook delivery queue full, dropping event", "webhook_id", w.ID, "event", e.Type)
		}
	}
}

func (r *WebhookRegistry) worker() {
	for {
		select {
		case job := <-r.workCh:
			r.deliver(job)
		case <-r.stopCh:
			return
		}
	}
}

func (r *WebhookRegistry) deliver(job deliveryJob) {
	body, err := json.Marshal(job.event)
	if err != nil {
		r.log.Error("failed to marshal event", "error", err)
		return
	}

	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt))) * time.Second
			time.Sleep(backoff)
		}

		req, err := http.NewRequest(http.MethodPost, job.hook.URL, bytes.NewReader(body))
		if err != nil {
			r.log.Error("failed to create webhook request", "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		if job.hook.Secret != "" {
			mac := hmac.New(sha256.New, []byte(job.hook.Secret))
			mac.Write(body)
			sig := hex.EncodeToString(mac.Sum(nil))
			req.Header.Set("X-Signature-256", fmt.Sprintf("sha256=%s", sig))
		}

		resp, err := r.client.Do(req)
		if err != nil {
			r.log.Warn("webhook delivery failed", "url", job.hook.URL, "attempt", attempt+1, "error", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return
		}
		r.log.Warn("webhook delivery got non-2xx", "url", job.hook.URL, "status", resp.StatusCode, "attempt", attempt+1)
	}
	r.log.Error("webhook delivery exhausted retries", "url", job.hook.URL, "event", job.event.Type)
}

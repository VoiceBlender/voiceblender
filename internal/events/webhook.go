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
)

type Webhook struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Secret string `json:"secret,omitempty"`
}

type WebhookRegistry struct {
	mu             sync.RWMutex
	globalWebhook  *Webhook // from WEBHOOK_URL + WEBHOOK_SECRET env vars
	legWebhooks    map[string]*Webhook // leg_id → Webhook
	roomWebhooks   map[string]*Webhook // room_id → Webhook
	bus            *Bus
	log            *slog.Logger
	client         *http.Client
	workCh         chan deliveryJob
	stopOnce       sync.Once
	stopCh         chan struct{}
}

type deliveryJob struct {
	hook  *Webhook
	event Event
}

func NewWebhookRegistry(bus *Bus, log *slog.Logger, globalURL, globalSecret string) *WebhookRegistry {
	var global *Webhook
	if globalURL != "" {
		global = &Webhook{ID: "global", URL: globalURL, Secret: globalSecret}
	}

	r := &WebhookRegistry{
		globalWebhook: global,
		legWebhooks:   make(map[string]*Webhook),
		roomWebhooks:  make(map[string]*Webhook),
		bus:           bus,
		log:           log,
		client:        &http.Client{Timeout: 10 * time.Second},
		workCh:        make(chan deliveryJob, 1000),
		stopCh:        make(chan struct{}),
	}
	_ = bus.Subscribe(r.dispatch)
	for i := 0; i < 10; i++ {
		go r.worker()
	}
	return r
}

func (r *WebhookRegistry) Stop() {
	r.stopOnce.Do(func() { close(r.stopCh) })
}

func (r *WebhookRegistry) SetLegWebhook(legID, url, secret string) {
	r.mu.Lock()
	r.legWebhooks[legID] = &Webhook{ID: legID, URL: url, Secret: secret}
	r.mu.Unlock()
}

func (r *WebhookRegistry) ClearLegWebhook(legID string) {
	r.mu.Lock()
	delete(r.legWebhooks, legID)
	r.mu.Unlock()
}

func (r *WebhookRegistry) SetRoomWebhook(roomID, url, secret string) {
	r.mu.Lock()
	r.roomWebhooks[roomID] = &Webhook{ID: roomID, URL: url, Secret: secret}
	r.mu.Unlock()
}

func (r *WebhookRegistry) ClearRoomWebhook(roomID string) {
	r.mu.Lock()
	delete(r.roomWebhooks, roomID)
	r.mu.Unlock()
}

func (r *WebhookRegistry) enqueue(w *Webhook, e Event) {
	select {
	case r.workCh <- deliveryJob{hook: w, event: e}:
	case <-r.stopCh:
	default:
		r.log.Warn("webhook delivery queue full, dropping event", "webhook_id", w.ID, "event", e.Type)
	}
}

func (r *WebhookRegistry) dispatch(e Event) {
	legID := e.Data.GetLegID()
	roomID := e.Data.GetRoomID()

	r.mu.RLock()
	var target *Webhook
	if legID != "" {
		target = r.legWebhooks[legID]
	}
	if target == nil && roomID != "" {
		target = r.roomWebhooks[roomID]
	}
	if target == nil {
		target = r.globalWebhook
	}
	r.mu.RUnlock()

	if target != nil {
		r.enqueue(target, e)
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

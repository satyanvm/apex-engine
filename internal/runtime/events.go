package runtime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/satyanvm/apex-engine/internal/netguard"
)

// Events a run sends to its callback URL.
const (
	EventApproval = "run.waiting_approval"
	EventFinished = "run.finished"
)

// EventInput is one event delivery.
type EventInput struct {
	URL, Tenant, RunID, Event, Status, Error, Node, Message string
	// Delivery is the same on every retry, so a receiver can drop repeats.
	Delivery string
}

// CallbackLabel names the derived key that signs a tenant's events.
func CallbackLabel(tenant string) string { return "callback:" + tenant }

// Sign returns the X-Apex-Signature value for body.
func Sign(key, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// DeliverEvent posts one signed event. Delivery is at least once: a 2xx ends it,
// a 408, 429 or 5xx is retried, and any other answer is final. The address is
// checked like every user-supplied URL, and redirects are not followed.
func (a *Activities) DeliverEvent(ctx context.Context, in EventInput) error {
	body, err := json.Marshal(map[string]any{
		"event": in.Event, "runId": in.RunID, "status": in.Status, "error": in.Error,
		"node": in.Node, "message": in.Message, "delivery": in.Delivery, "at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fatal("%s", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.URL, bytes.NewReader(body))
	if err != nil {
		return fatal("%s", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Apex-Event", in.Event)
	req.Header.Set("X-Apex-Delivery", in.Delivery)
	req.Header.Set("X-Apex-Signature", Sign(a.Store.Box.Derive(CallbackLabel(in.Tenant)), body))
	resp, err := netguard.Client(15*time.Second, false).Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case retryableStatus(resp.StatusCode):
		return fmt.Errorf("callback answered HTTP %d", resp.StatusCode)
	}
	return fatal("callback answered HTTP %d", resp.StatusCode)
}

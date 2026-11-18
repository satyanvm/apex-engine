//go:build integration

package httpapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/converter"
)

func TestNewRunRecordsVersionAndCompletesDurableWait(t *testing.T) {
	h := setup(t)
	id := h.start(keyA, nil, nd("t", "trigger.manual", nil),
		nd("pause", "logic.wait", map[string]any{"seconds": 1}, "t"))
	run := h.waitRun(keyA, id, "succeeded", "failed")
	if run["status"] != "succeeded" || node(run, "pause")["status"] != "succeeded" {
		t.Fatalf("wait did not succeed: %v", run)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	it := h.tc.GetWorkflowHistory(ctx, "apex-run-"+id, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	var version, started, fired int64
	for it.HasNext() {
		e, err := it.Next()
		if err != nil {
			t.Fatal(err)
		}
		switch e.GetEventType() {
		case enumspb.EVENT_TYPE_MARKER_RECORDED:
			a := e.GetMarkerRecordedEventAttributes()
			if a.GetMarkerName() == "Version" {
				var changeID string
				var patch json.Number
				if err := converter.GetDefaultDataConverter().FromPayloads(a.GetDetails()["change-id"], &changeID); err != nil {
					t.Fatal(err)
				}
				if err := converter.GetDefaultDataConverter().FromPayloads(a.GetDetails()["version"], &patch); err != nil {
					t.Fatal(err)
				}
				if changeID == "apex-run" {
					v, err := patch.Int64()
					if err != nil || v < 1 {
						t.Fatalf("invalid runner version: %v", patch)
					}
					version = e.GetEventId()
				}
			}
		case enumspb.EVENT_TYPE_TIMER_STARTED:
			started = e.GetEventId()
		case enumspb.EVENT_TYPE_TIMER_FIRED:
			fired = e.GetEventId()
		}
	}
	if version == 0 || started <= version || fired <= started {
		t.Fatalf("history must record the version before the timer starts and fires: version=%d start=%d fire=%d", version, started, fired)
	}
}

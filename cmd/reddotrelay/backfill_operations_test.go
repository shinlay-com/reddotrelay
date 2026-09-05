package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"reddotrelay/internal/core"
)

func TestBackfillPreviewConfirmationAuthorizationAndActiveLimit(t *testing.T) {
	store, secret, listener := managementFixture(t, core.APIKeyAdmin)
	handler := healthHandler(store)
	body := map[string]any{"rpcListenerId": listener.ID, "fromBlock": uint64(0), "toBlock": uint64(2), "mode": "backfill-missing"}
	preview := postManagement(t, handler, secret, "/api/v1/backfills/preview", "", body)
	if preview.Code != http.StatusOK || strings.Contains(preview.Body.String(), "webhook-secret") {
		t.Fatalf("preview=%d %s", preview.Code, preview.Body.String())
	}
	unconfirmed := postManagement(t, handler, secret, "/api/v1/backfills", "", body)
	if unconfirmed.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed=%d %s", unconfirmed.Code, unconfirmed.Body.String())
	}
	body["confirm"] = true
	created := postManagement(t, handler, secret, "/api/v1/backfills", "", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("created=%d %s", created.Code, created.Body.String())
	}
	again := postManagement(t, handler, secret, "/api/v1/backfills", "", body)
	if again.Code != http.StatusConflict {
		t.Fatalf("duplicate active=%d %s", again.Code, again.Body.String())
	}
	var job core.BackfillJob
	if err := json.Unmarshal(created.Body.Bytes(), &job); err != nil || job.ConfigRevision != 1 || job.State != core.BackfillQueued {
		t.Fatalf("job=%#v err=%v", job, err)
	}
	readStore, readSecret, _ := managementFixture(t, core.APIKeyReadOnly)
	forbidden := postManagement(t, healthHandler(readStore), readSecret, "/api/v1/backfills/preview", "", body)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("read-only=%d", forbidden.Code)
	}
}

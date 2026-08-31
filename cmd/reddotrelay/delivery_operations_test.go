package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"
)

func TestDeliveryDiagnosticsAndTargetedRequeueAreSecretSafe(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	now := time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC)
	event := core.Event{ID: core.EventID{ChainID: 1, TransactionHash: "0x" + strings.Repeat("a", 64), LogIndex: 7}, BlockNumber: 100, BlockHash: "0xblock", Address: "0x0000000000000000000000000000000000000001", Name: "Transfer", Signature: "Transfer(address,address,uint256)", DecodedPayload: []byte(`{"from":"0xsender","to":"0xreceiver","value":"42"}`), ObservedAt: now}
	delivery := core.Delivery{EventID: event.ID, Destination: "https://user:password@receiver.example/private/path?token=secret", NextAttempt: now}
	if err := store.SaveEventsAndCheckpoint(context.Background(), []core.Event{event}, []core.Delivery{delivery}, core.Checkpoint{ChainID: 1, BlockNumber: 100, BlockHash: "0xblock"}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimDueDeliveries(context.Background(), now, time.Minute, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	if err := store.MarkDeliveryDead(context.Background(), event.ID, delivery.Destination, claimed[0].Delivery.LeaseToken, "private failure at https://secret.example", 503); err != nil {
		t.Fatal(err)
	}
	handler := healthHandler(store)

	events := diagnosticAuthenticatedRequest(t, handler, secret, http.MethodGet, "/api/v1/events?limit=1&deliveryStatus=dead", nil)
	if events.Code != http.StatusOK || strings.Contains(events.Body.String(), "password") || strings.Contains(events.Body.String(), "token=secret") {
		t.Fatalf("events response = %d %s", events.Code, events.Body.String())
	}
	var eventPage struct {
		DeliverySummary map[string]int `json:"deliverySummary"`
		Entries         []struct {
			ID          string         `json:"id"`
			DecodedData map[string]any `json:"decodedData"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(events.Body.Bytes(), &eventPage); err != nil || len(eventPage.Entries) != 1 || eventPage.Entries[0].ID != core.EventGUID(event.ID) || eventPage.Entries[0].DecodedData["value"] != "42" || eventPage.DeliverySummary["dead"] != 1 {
		t.Fatalf("event page = %#v, %v", eventPage, err)
	}

	deliveries := diagnosticAuthenticatedRequest(t, handler, secret, http.MethodGet, "/api/v1/events/"+eventPage.Entries[0].ID+"/deliveries?limit=1", nil)
	if deliveries.Code != http.StatusOK || strings.Contains(deliveries.Body.String(), "password") || strings.Contains(deliveries.Body.String(), "private/path") || strings.Contains(deliveries.Body.String(), "secret.example") {
		t.Fatalf("deliveries response = %d %s", deliveries.Code, deliveries.Body.String())
	}
	var deliveryPage struct {
		Entries []struct {
			ID             string `json:"id"`
			Destination    string `json:"destination"`
			FailureSummary string `json:"failureSummary"`
			Status         string `json:"status"`
			LastStatusCode int    `json:"lastStatusCode"`
			TotalAttempts  int    `json:"totalAttempts"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(deliveries.Body.Bytes(), &deliveryPage); err != nil || len(deliveryPage.Entries) != 1 {
		t.Fatalf("delivery page = %#v, %v", deliveryPage, err)
	}
	item := deliveryPage.Entries[0]
	if item.ID == "" || item.Destination != "https://receiver.example" || item.FailureSummary != "delivery attempt failed" || item.LastStatusCode != 503 || item.TotalAttempts != 1 || item.Status != "dead" {
		t.Fatalf("delivery diagnostic = %#v", item)
	}

	requeue := diagnosticAuthenticatedRequest(t, handler, secret, http.MethodPost, "/api/v1/deliveries/"+item.ID+"/requeue", map[string]any{"confirm": true})
	if requeue.Code != http.StatusOK || !strings.Contains(requeue.Body.String(), `"status":"pending"`) {
		t.Fatalf("requeue = %d %s", requeue.Code, requeue.Body.String())
	}
	again := diagnosticAuthenticatedRequest(t, handler, secret, http.MethodPost, "/api/v1/deliveries/"+item.ID+"/requeue", map[string]any{"confirm": true})
	if again.Code != http.StatusNotFound {
		t.Fatalf("repeat requeue = %d %s", again.Code, again.Body.String())
	}
	audit, err := store.DeliveryRequeueAudit(context.Background(), 2, 0)
	if err != nil || len(audit) != 1 || audit[0].ActorName != "create-client" || audit[0].DeliveryID != item.ID {
		t.Fatalf("requeue audit = %#v, %v", audit, err)
	}
}

func TestDeliveryRequeueRequiresAdminAndExplicitConfirmation(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyReadOnly)
	handler := healthHandler(store)
	id := core.NewConfigID()
	response := diagnosticAuthenticatedRequest(t, handler, secret, http.MethodPost, "/api/v1/deliveries/"+id+"/requeue", map[string]any{"confirm": true})
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only requeue = %d %s", response.Code, response.Body.String())
	}
	adminStore, adminSecret := emptyManagementFixture(t, core.APIKeyAdmin)
	response = diagnosticAuthenticatedRequest(t, healthHandler(adminStore), adminSecret, http.MethodPost, "/api/v1/deliveries/"+id+"/requeue", map[string]any{"confirm": false})
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed requeue = %d %s", response.Code, response.Body.String())
	}
}

func diagnosticAuthenticatedRequest(t *testing.T, handler http.Handler, secret, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var content string
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		content = string(encoded)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(content))
	request.Header.Set("Authorization", "Bearer "+secret)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

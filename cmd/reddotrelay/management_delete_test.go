package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"reddotrelay/internal/core"
	"reddotrelay/internal/store/sqlite"
)

func TestDeleteManagementResources(t *testing.T) {
	tests := []struct {
		name string
		path func(core.RPCListener, string) string
		gone func(core.RPCListenerSnapshot, core.RPCListener, string) bool
	}{
		{"listener", func(config core.RPCListener, _ string) string { return "/api/v1/rpc-listeners/" + config.ID }, func(snapshot core.RPCListenerSnapshot, _ core.RPCListener, _ string) bool {
			return len(snapshot.Listeners) == 0
		}},
		{"global webhook", func(_ core.RPCListener, globalID string) string {
			return "/api/v1/rpc-listeners/webhooks/" + globalID
		}, func(snapshot core.RPCListenerSnapshot, _ core.RPCListener, _ string) bool {
			return len(snapshot.GlobalWebhooks) == 0
		}},
		{"chain webhook", func(config core.RPCListener, _ string) string {
			return "/api/v1/rpc-listeners/" + config.ID + "/webhooks/" + config.Webhooks[0].ID
		}, func(snapshot core.RPCListenerSnapshot, _ core.RPCListener, _ string) bool {
			return len(snapshot.Listeners[0].Webhooks) == 0
		}},
		{"contract", func(config core.RPCListener, _ string) string {
			return "/api/v1/rpc-listeners/" + config.ID + "/contracts/" + config.Contracts[0].ID
		}, func(snapshot core.RPCListenerSnapshot, _ core.RPCListener, _ string) bool {
			return len(snapshot.Listeners[0].Contracts) == 0
		}},
		{"contract webhook", func(config core.RPCListener, _ string) string {
			return "/api/v1/rpc-listeners/" + config.ID + "/contracts/" + config.Contracts[0].ID + "/webhooks/" + config.Contracts[0].Webhooks[0].ID
		}, func(snapshot core.RPCListenerSnapshot, _ core.RPCListener, _ string) bool {
			return len(snapshot.Listeners[0].Contracts[0].Webhooks) == 0
		}},
		{"event", func(config core.RPCListener, _ string) string {
			return "/api/v1/rpc-listeners/" + config.ID + "/contracts/" + config.Contracts[0].ID + "/events/" + config.Contracts[0].Events[0].ID
		}, func(snapshot core.RPCListenerSnapshot, _ core.RPCListener, _ string) bool {
			return len(snapshot.Listeners[0].Contracts[0].Events) == 0
		}},
		{"event webhook", func(config core.RPCListener, _ string) string {
			return "/api/v1/rpc-listeners/" + config.ID + "/contracts/" + config.Contracts[0].ID + "/events/" + config.Contracts[0].Events[0].ID + "/webhooks/" + config.Contracts[0].Events[0].Webhooks[0].ID
		}, func(snapshot core.RPCListenerSnapshot, _ core.RPCListener, _ string) bool {
			return len(snapshot.Listeners[0].Contracts[0].Events[0].Webhooks) == 0
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
			config, globalID, revision := createPatchFixture(t, store)
			response := deleteManagement(handlerForDelete(store), secret, test.path(config, globalID), revisionETag(revision))
			assertDeleted(t, response, revision+1)
			snapshot, err := store.RPCListenerSnapshot(context.Background())
			if err != nil || snapshot.Revision != revision+1 || !test.gone(snapshot, config, globalID) {
				t.Fatalf("snapshot after delete = %#v, error %v", snapshot, err)
			}
			audit, err := store.RPCListenerAudit(context.Background(), 2, 0)
			if err != nil || len(audit) != 1 || audit[0].Action != core.AuditActionDelete || audit[0].ActorName != "create-client" {
				t.Fatalf("delete audit entries = %#v, error %v", audit, err)
			}
		})
	}
}

func TestDeleteWebhookRestoresInheritance(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config, _, revision := createPatchFixture(t, store)
	event := config.Contracts[0].Events[0]
	path := "/api/v1/rpc-listeners/" + config.ID + "/contracts/" + config.Contracts[0].ID + "/events/" + event.ID + "/webhooks/" + event.Webhooks[0].ID
	response := deleteManagement(handlerForDelete(store), secret, path, revisionETag(revision))
	assertDeleted(t, response, revision+1)
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.Listeners[0].Contracts[0]
	if len(got.Events[0].Webhooks) != 0 || len(got.Webhooks) != 1 {
		t.Fatalf("event did not fall back to contract routes: %#v", got)
	}
}

func TestDeleteLastEffectiveWebhookIsRejectedAndRolledBack(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config := deleteFixtureListener()
	revision, err := store.CreateRPCListener(context.Background(), config, 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/rpc-listeners/" + config.ID + "/webhooks/" + config.Webhooks[0].ID
	response := deleteManagement(handlerForDelete(store), secret, path, revisionETag(revision))
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("last-route delete = %d, body %s", response.Code, response.Body.String())
	}
	snapshot, err := store.RPCListenerSnapshot(context.Background())
	if err != nil || snapshot.Revision != revision || len(snapshot.Listeners[0].Webhooks) != 1 {
		t.Fatalf("rejected delete mutated configuration: %#v, %v", snapshot, err)
	}
}

func TestDeletePreservesOperationalHistory(t *testing.T) {
	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config := deleteFixtureListener()
	now := time.Now().UTC()
	revision, err := store.CreateRPCListener(context.Background(), config, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	id := core.EventID{ChainID: config.ChainID, TransactionHash: "0xtx", LogIndex: 1}
	event := core.Event{ID: id, BlockNumber: 10, BlockHash: "0xblock", Address: config.Contracts[0].Address, Name: "Transfer", ObservedAt: now}
	delivery := core.Delivery{EventID: id, Destination: config.Webhooks[0].URL, NextAttempt: now}
	checkpoint := core.Checkpoint{ChainID: config.ChainID, BlockNumber: 10, BlockHash: "0xblock"}
	if err := store.SaveEventsAndCheckpoint(context.Background(), []core.Event{event}, []core.Delivery{delivery}, checkpoint); err != nil {
		t.Fatal(err)
	}
	response := deleteManagement(handlerForDelete(store), secret, "/api/v1/rpc-listeners/"+config.ID, revisionETag(revision))
	assertDeleted(t, response, revision+1)
	gotCheckpoint, err := store.Checkpoint(context.Background(), config.ChainID)
	if err != nil || gotCheckpoint != checkpoint {
		t.Fatalf("checkpoint after delete = %#v, %v", gotCheckpoint, err)
	}
	items, err := store.DueDeliveries(context.Background(), now, 1)
	if err != nil || len(items) != 1 || items[0].Event.ID != id {
		t.Fatalf("outbox after delete = %#v, %v", items, err)
	}
}

func TestDeleteAuthorizationRevisionAndIDs(t *testing.T) {
	readOnlyStore, readOnlySecret := emptyManagementFixture(t, core.APIKeyReadOnly)
	readOnlyConfig, _, readOnlyRevision := createPatchFixture(t, readOnlyStore)
	response := deleteManagement(handlerForDelete(readOnlyStore), readOnlySecret, "/api/v1/rpc-listeners/"+readOnlyConfig.ID, revisionETag(readOnlyRevision))
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only delete = %d, body %s", response.Code, response.Body.String())
	}

	store, secret := emptyManagementFixture(t, core.APIKeyAdmin)
	config, _, revision := createPatchFixture(t, store)
	for _, test := range []struct {
		name, path, etag string
		want             int
	}{
		{"missing revision", "/api/v1/rpc-listeners/" + config.ID, "", http.StatusPreconditionRequired},
		{"stale revision", "/api/v1/rpc-listeners/" + config.ID, revisionETag(revision - 1), http.StatusPreconditionFailed},
		{"malformed id", "/api/v1/rpc-listeners/not-a-uuid", revisionETag(revision), http.StatusBadRequest},
		{"reserved word in id position", "/api/v1/rpc-listeners/contracts", revisionETag(revision), http.StatusBadRequest},
		{"noncanonical id", "/api/v1/rpc-listeners/" + strings.ToUpper(config.ID), revisionETag(revision), http.StatusBadRequest},
		{"missing listener", "/api/v1/rpc-listeners/" + core.NewConfigID(), revisionETag(revision), http.StatusNotFound},
		{"missing contract", "/api/v1/rpc-listeners/" + config.ID + "/contracts/" + core.NewConfigID(), revisionETag(revision), http.StatusNotFound},
		{"missing event", "/api/v1/rpc-listeners/" + config.ID + "/contracts/" + config.Contracts[0].ID + "/events/" + core.NewConfigID(), revisionETag(revision), http.StatusNotFound},
		{"missing webhook", "/api/v1/rpc-listeners/" + config.ID + "/webhooks/" + core.NewConfigID(), revisionETag(revision), http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := deleteManagement(handlerForDelete(store), secret, test.path, test.etag)
			if got.Code != test.want {
				t.Fatalf("DELETE %s = %d, want %d, body %s", test.path, got.Code, test.want, got.Body.String())
			}
		})
	}
}

func handlerForDelete(store *sqlite.Store) http.Handler {
	return healthHandler(store)
}

func deleteManagement(handler http.Handler, secret, path, etag string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodDelete, path, nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertDeleted(t *testing.T, response *httptest.ResponseRecorder, revision uint64) {
	t.Helper()
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || response.Header().Get("ETag") != revisionETag(revision) || response.Header().Get("X-Config-Revision") != revisionString(revision) {
		t.Fatalf("delete = %d, ETag %q, revision %q, body %q", response.Code, response.Header().Get("ETag"), response.Header().Get("X-Config-Revision"), response.Body.String())
	}
}

func deleteFixtureListener() core.RPCListener {
	return core.RPCListener{
		ID: core.NewConfigID(), Name: "private", ChainID: 1, RPCURL: "https://rpc.example.test", BatchSize: 100,
		PollInterval: time.Second, ReorgDepth: 5, RPCRetryAttempts: 2, RPCRetryBackoff: time.Second, RPCTimeout: 5 * time.Second,
		Webhooks: []core.WebhookConfig{{ID: core.NewConfigID(), URL: "https://chain.example.test/hook"}},
		Contracts: []core.ContractConfig{{ID: core.NewConfigID(), Address: "0x0000000000000000000000000000000000000001", ABI: []byte(patchFixtureABI),
			Events: []core.EventConfig{{ID: core.NewConfigID(), Selector: "Transfer()"}},
		}},
	}
}

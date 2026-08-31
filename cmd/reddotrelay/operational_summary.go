package main

import (
	"net/http"

	"reddotrelay/internal/observability"
	"reddotrelay/internal/store/sqlite"
)

func handleOperationalSummary(store *sqlite.Store, metrics *observability.Metrics, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pending, delivered, dead, err := store.DeliveryStatusCounts(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "load operational summary")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"deliveries": map[string]int64{"pendingRetrying": pending, "delivered": delivered, "dead": dead},
		"counters":   metrics.Summary(),
	})
}

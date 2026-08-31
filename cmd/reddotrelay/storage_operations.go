package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"reddotrelay/internal/config"
	retentionservice "reddotrelay/internal/retention"
	"reddotrelay/internal/store/sqlite"
)

type engineOperations struct {
	store           *sqlite.Store
	retention       *retentionservice.Service
	retentionConfig config.RetentionConfig
	logger          *slog.Logger
	dynamic         *dynamicRetentionSettings
}

func newEngineOperations(store *sqlite.Store, settings config.RetentionConfig, loggers ...*slog.Logger) *engineOperations {
	if persisted, ok, err := store.RetentionSettings(context.Background()); err == nil && ok {
		settings = persisted
	}
	var logger *slog.Logger
	if len(loggers) > 0 {
		logger = loggers[0]
	}
	return &engineOperations{store: store, retention: retentionservice.New(store), retentionConfig: settings, logger: logger, dynamic: &dynamicRetentionSettings{value: settings}}
}

func handleBuildInfo(environment config.EnvironmentConfig, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimSpace(environment.Name)
	if name == "" {
		name = "Local Engine"
	}
	writeJSON(writer, http.StatusOK, map[string]string{"version": version, "commit": commit, "buildDate": buildDate, "environmentName": name})
}

func handleStorageStatus(store *sqlite.Store, writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	status, err := store.StorageStatus(request.Context())
	if err != nil {
		writeAPIError(writer, http.StatusInternalServerError, "storage status unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, status)
}

type retentionRequest struct {
	OlderThan string `json:"olderThan"`
	BatchSize int    `json:"batchSize"`
	MaxEvents int64  `json:"maxEvents"`
	Confirm   bool   `json:"confirm"`
}

func decodeRetentionRequest(writer http.ResponseWriter, request *http.Request) (retentionRequest, time.Time, bool) {
	var body retentionRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid retention request")
		return body, time.Time{}, false
	}
	age, err := time.ParseDuration(strings.TrimSpace(body.OlderThan))
	if err != nil || age <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "olderThan must be a positive duration")
		return body, time.Time{}, false
	}
	if body.BatchSize == 0 {
		body.BatchSize = 500
	}
	if body.BatchSize < 1 || body.BatchSize > 10000 || body.MaxEvents < 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid retention limits")
		return body, time.Time{}, false
	}
	return body, time.Now().UTC().Add(-age), true
}

func handleRetention(operations *engineOperations, writer http.ResponseWriter, request *http.Request) {
	if operations == nil {
		writeAPIError(writer, http.StatusServiceUnavailable, "retention operations unavailable")
		return
	}
	if request.Method == http.MethodGet {
		settings := operations.dynamic.get()
		runtime := operations.retention.Status()
		var nextRun *time.Time
		if settings.DeliveredFor > 0 && runtime.LastRun != nil {
			next := runtime.LastRun.Completed.Add(settings.PollInterval)
			nextRun = &next
		}
		writeJSON(writer, http.StatusOK, map[string]any{"automatic": settings.DeliveredFor > 0, "deliveredFor": settings.DeliveredFor.String(), "pollInterval": settings.PollInterval.String(), "batchSize": settings.BatchSize, "nextRunAt": nextRun, "runtime": runtime})
		return
	}
	if request.Method != http.MethodPost || !requireAdmin(writer, request) {
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
			writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		}
		return
	}
	if strings.HasSuffix(request.URL.Path, "/config") {
		var input struct {
			Enabled      bool   `json:"enabled"`
			DeliveredFor string `json:"deliveredFor"`
			PollInterval string `json:"pollInterval"`
			BatchSize    int    `json:"batchSize"`
			Confirm      bool   `json:"confirm"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&input) != nil || !input.Confirm {
			writeAPIError(writer, http.StatusBadRequest, "valid configuration and confirm must be provided")
			return
		}
		settings := config.RetentionConfig{}
		if input.Enabled {
			if strings.TrimSpace(input.DeliveredFor) == "0s" || strings.TrimSpace(input.DeliveredFor) == "" {
				input.DeliveredFor = "720h"
			}
			age, err := time.ParseDuration(strings.TrimSpace(input.DeliveredFor))
			if err != nil || age <= 0 {
				writeAPIError(writer, http.StatusBadRequest, "deliveredFor must be positive")
				return
			}
			poll := time.Hour
			if strings.TrimSpace(input.PollInterval) != "" {
				poll, _ = time.ParseDuration(input.PollInterval)
			}
			if poll <= 0 {
				writeAPIError(writer, http.StatusBadRequest, "pollInterval must be positive")
				return
			}
			batch := input.BatchSize
			if batch == 0 {
				batch = 1000
			}
			if batch < 1 || batch > 10000 {
				writeAPIError(writer, http.StatusBadRequest, "batchSize is invalid")
				return
			}
			settings = config.RetentionConfig{DeliveredFor: age, PollInterval: poll, BatchSize: batch}
		}
		operations.dynamic.set(settings)
		if err := operations.store.SaveRetentionSettings(request.Context(), settings); err != nil {
			writeAPIError(writer, http.StatusInternalServerError, "retention configuration could not be saved")
			return
		}
		operations.retentionConfig = settings
		if operations.logger != nil {
			operations.logger.Info("automatic retention configuration changed", "enabled", settings.DeliveredFor > 0)
		}
		writeJSON(writer, http.StatusOK, map[string]any{"automatic": settings.DeliveredFor > 0, "deliveredFor": settings.DeliveredFor.String(), "pollInterval": settings.PollInterval.String(), "batchSize": settings.BatchSize})
		return
	}
	body, cutoff, ok := decodeRetentionRequest(writer, request)
	if !ok {
		return
	}
	if strings.HasSuffix(request.URL.Path, "/preview") {
		result, err := operations.retention.Preview(request.Context(), cutoff)
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, "retention preview failed")
			return
		}
		writeJSON(writer, http.StatusOK, result)
		return
	}
	if !strings.HasSuffix(request.URL.Path, "/prune") {
		writeAPIError(writer, http.StatusNotFound, "resource not found")
		return
	}
	if !body.Confirm {
		writeAPIError(writer, http.StatusBadRequest, "confirm must be true")
		return
	}
	result, err := operations.retention.Prune(request.Context(), cutoff, body.BatchSize, body.MaxEvents, 25*time.Millisecond, nil)
	if errors.Is(err, retentionservice.ErrRunning) {
		writeAPIError(writer, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		if operations.logger != nil {
			operations.logger.Error("retention prune failed", "error", err)
		}
		writeAPIError(writer, http.StatusInternalServerError, "retention prune failed")
		return
	}
	if operations.logger != nil {
		operations.logger.Info("retention cleanup completed", "events", result.Deleted, "eligible", result.Eligible)
	}
	writeJSON(writer, http.StatusOK, result)
}

func handleStorageOptimize(store *sqlite.Store, writer http.ResponseWriter, request *http.Request, loggers ...*slog.Logger) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAPIError(writer, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !requireAdmin(writer, request) {
		return
	}
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1024)).Decode(&body); err != nil || !body.Confirm {
		writeAPIError(writer, http.StatusBadRequest, "confirm must be true")
		return
	}
	if err := store.Optimize(request.Context()); err != nil {
		if len(loggers) > 0 && loggers[0] != nil {
			loggers[0].Error("SQLite optimize failed", "error", err)
		}
		writeAPIError(writer, http.StatusInternalServerError, "database optimize failed")
		return
	}
	if len(loggers) > 0 && loggers[0] != nil {
		loggers[0].Info("SQLite optimized")
	}
	writer.Header().Set("Content-Length", strconv.Itoa(0))
	writer.WriteHeader(http.StatusNoContent)
}

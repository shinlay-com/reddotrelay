package api

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIContractStructureAndRoutes(t *testing.T) {
	document := loadContract(t)
	if document["openapi"] != "3.1.0" {
		t.Fatalf("openapi = %#v", document["openapi"])
	}
	if document["jsonSchemaDialect"] != "https://json-schema.org/draft/2020-12/schema" {
		t.Fatalf("JSON Schema dialect missing")
	}
	paths := object(t, document["paths"], "paths")
	got := make([]string, 0)
	for path, raw := range paths {
		item := object(t, raw, path)
		for _, method := range []string{"get", "post", "patch", "delete", "put"} {
			if _, ok := item[method]; ok {
				got = append(got, strings.ToUpper(method)+" "+path)
			}
		}
	}
	sort.Strings(got)
	want := []string{
		"GET /api/v1/api-keys", "POST /api/v1/api-keys", "POST /api/v1/api-keys/{keyId}/{operation}",
		"GET /api/v1/build-info", "GET /api/v1/storage/status", "GET /api/v1/retention/status",
		"DELETE /api/v1/ui-session",
		"DELETE /api/v1/rpc-listeners/{rpcListenerId}",
		"DELETE /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}",
		"DELETE /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/events/{eventId}",
		"DELETE /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/events/{eventId}/webhooks/{webhookId}",
		"DELETE /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/webhooks/{webhookId}",
		"DELETE /api/v1/rpc-listeners/{rpcListenerId}/webhooks/{webhookId}",
		"DELETE /api/v1/rpc-listeners/webhooks/{webhookId}",
		"GET /api/v1/rpc-listener-audit", "GET /api/v1/rpc-listener-status",
		"GET /api/v1/operational-events",
		"GET /api/v1/events", "GET /api/v1/events/{eventId}/deliveries",
		"GET /api/v1/delivery-requeue-audit",
		"GET /api/v1/scanner-progress",
		"GET /api/v1/rpc-listener-export",
		"GET /api/v1/rpc-listeners", "GET /api/v1/rpc-listeners/{rpcListenerId}",
		"GET /api/v1/ui-session",
		"GET /api/v1/ui-setup", "POST /api/v1/ui-setup",
		"GET /api/v1/users", "POST /api/v1/users", "POST /api/v1/users/{userId}/{operation}",
		"GET /healthz", "GET /metrics", "GET /readyz",
		"PATCH /api/v1/rpc-listeners/{rpcListenerId}",
		"PATCH /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}",
		"PATCH /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/events/{eventId}",
		"PATCH /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/events/{eventId}/webhooks/{webhookId}",
		"PATCH /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/webhooks/{webhookId}",
		"PATCH /api/v1/rpc-listeners/{rpcListenerId}/webhooks/{webhookId}",
		"PATCH /api/v1/rpc-listeners/webhooks/{webhookId}",
		"POST /api/v1/rpc-listeners",
		"POST /api/v1/connection-tests/rpc",
		"POST /api/v1/connection-tests/webhook",
		"POST /api/v1/deliveries/{deliveryId}/requeue",
		"POST /api/v1/rpc-listeners/{rpcListenerId}/pause",
		"POST /api/v1/rpc-listeners/{rpcListenerId}/resume",
		"POST /api/v1/rpc-listeners/{rpcListenerId}/contracts",
		"POST /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/events",
		"POST /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/events/{eventId}/webhooks",
		"POST /api/v1/rpc-listeners/{rpcListenerId}/contracts/{contractId}/webhooks",
		"POST /api/v1/rpc-listeners/{rpcListenerId}/webhooks",
		"POST /api/v1/rpc-listeners/webhooks",
		"POST /api/v1/ui-session",
		"POST /api/v1/storage/optimize", "POST /api/v1/retention/preview", "POST /api/v1/retention/prune",
		"POST /api/v1/retention/config",
		"PUT /api/v1/rpc-listener-import",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("documented operations differ\ngot:  %v\nwant: %v", got, want)
	}
}

func TestOpenAPIMutationsRequireRevisionAndReferencesResolve(t *testing.T) {
	document := loadContract(t)
	walkReferences(t, document, document)
	paths := object(t, document["paths"], "paths")
	for path, raw := range paths {
		item := object(t, raw, path)
		for _, method := range []string{"post", "patch", "delete", "put"} {
			rawOperation, ok := item[method]
			if !ok {
				continue
			}
			if path == "/api/v1/ui-session" || path == "/api/v1/ui-setup" ||
				strings.HasPrefix(path, "/api/v1/users") || strings.HasPrefix(path, "/api/v1/api-keys") ||
				strings.HasPrefix(path, "/api/v1/connection-tests/") || strings.HasPrefix(path, "/api/v1/deliveries/") ||
				strings.HasPrefix(path, "/api/v1/retention/") || path == "/api/v1/storage/optimize" {
				continue
			}
			operation := object(t, rawOperation, method+" "+path)
			parameters, _ := operation["parameters"].([]any)
			found := false
			for _, rawParameter := range parameters {
				parameter := object(t, rawParameter, "parameter")
				if parameter["$ref"] == "#/components/parameters/IfMatch" || parameter["name"] == "If-Match" {
					found = true
				}
			}
			if !found {
				t.Errorf("%s %s does not require If-Match", strings.ToUpper(method), path)
			}
		}
	}
}

func TestOpenAPIPublicOperationsOverrideBearerSecurity(t *testing.T) {
	document := loadContract(t)
	paths := object(t, document["paths"], "paths")
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		operation := object(t, object(t, paths[path], path)["get"], "GET "+path)
		security, ok := operation["security"].([]any)
		if !ok || len(security) != 0 {
			t.Errorf("GET %s must explicitly be public", path)
		}
	}
	operation := object(t, object(t, paths["/api/v1/ui-session"], "/api/v1/ui-session")["post"], "POST /api/v1/ui-session")
	security, ok := operation["security"].([]any)
	if !ok || len(security) != 0 {
		t.Error("POST /api/v1/ui-session must explicitly be public")
	}
	for _, method := range []string{"get", "post"} {
		operation := object(t, object(t, paths["/api/v1/ui-setup"], "/api/v1/ui-setup")[method], strings.ToUpper(method)+" /api/v1/ui-setup")
		security, ok := operation["security"].([]any)
		if !ok || len(security) != 0 {
			t.Errorf("%s /api/v1/ui-setup must explicitly be public", strings.ToUpper(method))
		}
	}
	buildInfo := object(t, object(t, paths["/api/v1/build-info"], "/api/v1/build-info")["get"], "GET /api/v1/build-info")
	if security, ok := buildInfo["security"].([]any); ok && len(security) == 0 {
		t.Error("GET /api/v1/build-info must inherit authenticated API security")
	}
}

func loadContract(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse OpenAPI YAML: %v", err)
	}
	return document
}

func object(t *testing.T, value any, name string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", name, value)
	}
	return result
}

func walkReferences(t *testing.T, root, value any) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		if reference, ok := typed["$ref"].(string); ok {
			if !strings.HasPrefix(reference, "#/") {
				t.Errorf("external reference %q is not allowed", reference)
			} else if !referenceExists(root, reference) {
				t.Errorf("unresolved reference %q", reference)
			}
		}
		for _, child := range typed {
			walkReferences(t, root, child)
		}
	case []any:
		for _, child := range typed {
			walkReferences(t, root, child)
		}
	}
}

func referenceExists(root any, reference string) bool {
	current := root
	for _, token := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = object[strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")]
		if !ok {
			return false
		}
	}
	return true
}

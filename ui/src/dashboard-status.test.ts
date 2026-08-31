import assert from "node:assert/strict";
import test from "node:test";
import { deriveEngineStatus } from "./dashboard-status.ts";

const healthy = { healthOK: true, readinessOK: true, pendingDeliveries: 0, deadDeliveries: 0, scannerFailure: false, runtimeFailure: false };

test("dashboard status follows health, readiness, and operational precedence", () => {
  assert.equal(deriveEngineStatus(healthy), "Healthy");
  assert.equal(deriveEngineStatus({ ...healthy, pendingDeliveries: 1 }), "Degraded");
  assert.equal(deriveEngineStatus({ ...healthy, scannerFailure: true }), "Degraded");
  assert.equal(deriveEngineStatus({ ...healthy, readinessOK: false, pendingDeliveries: 1 }), "Starting");
  assert.equal(deriveEngineStatus({ ...healthy, healthOK: false, readinessOK: false }), "Unavailable");
});

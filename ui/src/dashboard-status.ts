export type EngineStatus = "Healthy" | "Starting" | "Degraded" | "Unavailable";

export function deriveEngineStatus(input: {
  healthOK: boolean;
  readinessOK: boolean;
  pendingDeliveries: number;
  deadDeliveries: number;
  scannerFailure: boolean;
  runtimeFailure: boolean;
}): EngineStatus {
  if (!input.healthOK) return "Unavailable";
  if (!input.readinessOK) return "Starting";
  if (input.pendingDeliveries > 0 || input.deadDeliveries > 0 || input.scannerFailure || input.runtimeFailure) return "Degraded";
  return "Healthy";
}

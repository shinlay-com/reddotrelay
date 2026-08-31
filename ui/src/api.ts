import type { Session } from "./app";

export class APIError extends Error {
  constructor(message: string, readonly status: number) { super(message); }
}

export async function responseError(response: Response) {
  let message = `Request failed (${response.status}).`;
  try { message = (await response.json() as { error?: string }).error ?? message; } catch { /* no JSON error */ }
  if (response.status === 412) message = "Configuration changed on the server. Keep this form open, refresh the dashboard, review the latest values, and save again.";
  return new APIError(message, response.status);
}

export async function mutate<T = unknown>(session: Session, revision: number, path: string, method: string, body?: unknown): Promise<T | undefined> {
  const headers: Record<string, string> = { Accept: "application/json", "If-Match": `"revision-${revision}"`, "X-CSRF-Token": session.csrfToken };
  if (body !== undefined) headers["Content-Type"] = method === "PATCH" ? "application/merge-patch+json" : "application/json";
  const response = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  if (!response.ok) throw await responseError(response);
  if (response.status === 204) return undefined;
  return response.json() as Promise<T>;
}

export async function action<T>(session: Session, path: string, body: unknown): Promise<T> {
  const response = await fetch(path, { method: "POST", headers: { Accept: "application/json", "Content-Type": "application/json", "X-CSRF-Token": session.csrfToken }, body: JSON.stringify(body) });
  if (!response.ok) throw await responseError(response);
  if (response.status === 204) return undefined as T;
  return response.json() as Promise<T>;
}

export async function read<T>(path: string): Promise<T> {
  const response = await fetch(path, { headers: { Accept: "application/json" } });
  if (!response.ok) throw await responseError(response);
  return response.json() as Promise<T>;
}

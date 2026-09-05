import { useState } from "preact/hooks";
import type { ConfigSnapshot, Session, RPCListener } from "./app";
import { mutate } from "./api";
import { ListenerEditor } from "./listener-editor";
import { WebhookManager } from "./webhook-editor";

export function ConfigurationPanel({ session, snapshot, onChanged }: { session: Session; snapshot: ConfigSnapshot; onChanged: () => void }) {
  const [creating, setCreating] = useState(false);
  const [transferring, setTransferring] = useState(false);
  return <section class="configuration-tools listeners-management">
    <div class="section-title"><div><p class="eyebrow">Engine configuration</p><h1>RPC listeners</h1><p class="section-description">Connect RPC endpoints, configure global delivery routing, and manage advanced configuration.</p></div>{session.role === "admin" && <div class="toolbar"><button class="button button--secondary" onClick={() => setTransferring(!transferring)}>Advanced JSON</button><button class="button" onClick={() => setCreating(!creating)}>Add RPC listener</button></div>}</div>
    {session.role !== "admin" && <section class="panel permission"><strong>Read-only access</strong><span>This API key can inspect configuration but cannot change it.</span></section>}
    {session.role === "admin" && creating && <ListenerEditor session={session} revision={snapshot.revision} onCancel={() => setCreating(false)} onChanged={() => { setCreating(false); onChanged(); }} />}
    {session.role === "admin" && transferring && <ConfigurationTransfer session={session} revision={snapshot.revision} onChanged={onChanged} />}
    <section class="panel global-webhooks"><WebhookManager session={session} revision={snapshot.revision} title="Global webhooks" basePath="/api/v1/rpc-listeners/webhooks" webhooks={snapshot.globalWebhooks} onChanged={onChanged} /></section>
  </section>;
}

export function ConfigurationTransfer({ session, revision, onChanged }: { session: Session; revision: number; onChanged: () => void }) {
  const [document, setDocument] = useState(""); const [busy, setBusy] = useState(false); const [message, setMessage] = useState(""); const [error, setError] = useState("");
  async function load() { setBusy(true); setError(""); setMessage(""); try { const response = await fetch("/api/v1/rpc-listener-export", { headers: { Accept: "application/json" } }); if (!response.ok) throw new Error(`Export failed (${response.status}).`); setDocument(JSON.stringify(await response.json(), null, 2)); setMessage("Current secret-safe configuration loaded."); } catch (caught) { setError(caught instanceof Error ? caught.message : "Export failed."); } finally { setBusy(false); } }
  async function replace() { setError(""); setMessage(""); let parsed: unknown; try { parsed = JSON.parse(document); } catch { setError("Configuration must be valid JSON."); return; } if (!window.confirm("Replace the complete durable configuration atomically? This cannot be undone from the UI.")) return; setBusy(true); try { await mutate(session, revision, "/api/v1/rpc-listener-import", "PUT", parsed); setMessage("Configuration replaced successfully."); onChanged(); } catch (caught) { setError(caught instanceof Error ? caught.message : "Import failed."); } finally { setBusy(false); } }
  return <section class="panel editor"><div class="editor__heading"><div><h3>Atomic configuration JSON</h3><p>Advanced backup and restore for the complete desired state. Prefer structured editors for ordinary changes.</p></div><span class="role">Revision {revision}</span></div><textarea aria-label="Configuration JSON" value={document} onInput={(event) => setDocument(event.currentTarget.value)} spellcheck={false} placeholder="Load the current configuration before editing." />{message && <p class="success" role="status">{message}</p>}{error && <p class="error" role="alert">{error}</p>}<div class="toolbar"><button class="button button--secondary" disabled={busy} onClick={() => void load()}>Load current</button><button class="button button--danger" disabled={busy || !document.trim()} onClick={() => void replace()}>Replace configuration</button></div></section>;
}

export function ListenerActions({ session, revision, listener, onChanged, onEdit }: { session: Session; revision: number; listener: RPCListener; onChanged: () => void; onEdit: () => void }) {
  const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  if (session.role !== "admin") return <div class="listener-actions"><div class="toolbar"><button class="button button--secondary" disabled title="Administrator access required">Edit RPC listener</button><button class="button button--secondary" disabled title="Administrator access required">{listener.paused ? "Activate" : "Pause"}</button><button class="button button--danger" disabled title="Administrator access required">Delete</button></div></div>;
  async function run(path: string, method: string, confirmation: string) { if (!window.confirm(confirmation)) return; setBusy(true); setError(""); try { await mutate(session, revision, path, method); onChanged(); } catch (caught) { setError(caught instanceof Error ? caught.message : "Configuration change failed."); } finally { setBusy(false); } }
  return <div class="listener-actions">{error && <p class="error" role="alert">{error}</p>}<div class="toolbar"><button class="button button--secondary" disabled={busy} onClick={onEdit}>Edit RPC listener</button><button class="button button--secondary" disabled={busy} onClick={() => void run(`/api/v1/rpc-listeners/${listener.id}/${listener.paused ? "resume" : "pause"}`, "POST", `${listener.paused ? "Activate" : "Pause"} RPC listener “${listener.name}”?`)}>{listener.paused ? "Activate" : "Pause"}</button><button class="button button--danger" disabled={busy} onClick={() => void run(`/api/v1/rpc-listeners/${listener.id}`, "DELETE", `Delete RPC listener “${listener.name}” and its nested configuration?`)}>Delete</button></div></div>;
}

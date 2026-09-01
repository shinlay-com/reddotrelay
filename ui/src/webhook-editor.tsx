import { useState } from "preact/hooks";
import type { Session, WebhookConfig } from "./app";
import { action, mutate } from "./api";

type LocatorMode = "direct" | "reference" | "unchanged";

function WebhookEditor({ session, revision, basePath, webhook, onCancel, onChanged }: {
  session: Session; revision: number; basePath: string; webhook?: WebhookConfig; onCancel: () => void; onChanged: () => void;
}) {
  const [mode, setMode] = useState<LocatorMode>(() => webhook ? (webhook.urlRef ? "reference" : "unchanged") : "direct");
  const [locator, setLocator] = useState(webhook?.urlRef ?? "");
  const [hmac, setHMAC] = useState(webhook?.authentication?.type === "hmac-sha256");
  const [secretRef, setSecretRef] = useState(webhook?.authentication?.secretRef ?? "");
  const [keyID, setKeyID] = useState(webhook?.authentication?.keyId ?? "");
  const [busy, setBusy] = useState(false); const [error, setError] = useState(""); const [result, setResult] = useState("");
  function locatorBody() { return mode === "direct" ? { url: locator } : mode === "reference" ? { urlRef: locator } : {}; }
  function authentication() { return hmac ? { type: "hmac-sha256", secretRef, keyId: keyID } : { type: "", secretRef: "", keyId: "" }; }
  async function test() {
    setBusy(true); setError(""); setResult("");
    try {
      if (mode === "unchanged") throw new Error("Choose Direct URL or Secret reference to test a replacement. The stored direct URL is redacted and cannot be reconstructed by the UI.");
      const response = await action<{ reachable: true; accepted: boolean; statusCode: number }>(session, "/api/v1/connection-tests/webhook", { ...locatorBody(), authentication: authentication() });
      setResult(response.accepted ? `Webhook accepted the test request (HTTP ${response.statusCode}).` : `Webhook was reached but rejected the test request (HTTP ${response.statusCode}).`);
    } catch (caught) { setError(caught instanceof Error ? caught.message : "Webhook test failed."); } finally { setBusy(false); }
  }
  async function submit(event: Event) {
    event.preventDefault();
    if (!window.confirm(webhook ? "Save changes to this webhook destination?" : "Create this webhook destination?")) return;
    setBusy(true); setError(""); setResult("");
    try {
      const body = { ...locatorBody(), authentication: authentication() };
      await mutate(session, revision, webhook ? `${basePath}/${webhook.id}` : basePath, webhook ? "PATCH" : "POST", body);
      if (!webhook) onCancel();
      onChanged();
    } catch (caught) { setError(caught instanceof Error ? caught.message : "Webhook could not be saved."); } finally { setBusy(false); }
  }
  return <form class="editor webhook-editor" onSubmit={(event) => void submit(event)}>
    <div class="editor__heading"><div><h3>{webhook ? "Edit webhook" : "Add webhook"}</h3><p>Use a direct URL only when it has no credentials. Signed URLs and tokens belong in a secret reference.</p></div><button class="icon-button" type="button" aria-label="Close webhook editor" onClick={onCancel}>×</button></div>
    <fieldset class="locator"><legend>Destination</legend><div class="segmented">{webhook && <label><input type="radio" checked={mode === "unchanged"} onChange={() => setMode("unchanged")} /> Keep current</label>}<label><input type="radio" checked={mode === "direct"} onChange={() => { setMode("direct"); setLocator(""); }} /> Direct URL</label><label><input type="radio" checked={mode === "reference"} onChange={() => { setMode("reference"); setLocator(""); }} /> Secret reference</label></div>
      {mode !== "unchanged" && <label>{mode === "direct" ? "Webhook URL" : "Webhook URL secret reference"}<input value={locator} onInput={(event) => setLocator(event.currentTarget.value)} placeholder={mode === "direct" ? "https://example.com/hooks/reddotrelay" : "env://CHAIN_WEBHOOK_URL"} spellcheck={false} required /></label>}
    </fieldset>
    <label class="check"><input type="checkbox" checked={hmac} onChange={(event) => setHMAC(event.currentTarget.checked)} /> Sign requests with HMAC-SHA256</label>
    {hmac && <div class="form-grid"><label>HMAC secret reference<input value={secretRef} onInput={(event) => setSecretRef(event.currentTarget.value)} placeholder="file:///run/secrets/webhook_hmac" spellcheck={false} required /></label><label>Key ID (optional)<input value={keyID} onInput={(event) => setKeyID(event.currentTarget.value)} placeholder="production-2026" /></label></div>}
    {result && <p class="success" role="status">{result}</p>}{error && <p class="error" role="alert">{error}</p>}
    <div class="toolbar"><button class="button button--secondary" type="button" disabled={busy || mode === "unchanged" || !locator.trim() || (hmac && !secretRef.trim())} onClick={() => void test()}>Send test</button><button class="button" disabled={busy}>{busy ? "Working…" : webhook ? "Save webhook" : "Create webhook"}</button><button class="button button--secondary" type="button" onClick={onCancel}>Cancel</button></div>
  </form>;
}

export function WebhookManager({ session, revision, title, basePath, webhooks, inherited, onChanged }: {
  session: Session; revision: number; title: string; basePath: string; webhooks: WebhookConfig[]; inherited?: string; onChanged: () => void;
}) {
  const [creating, setCreating] = useState(false); const [editing, setEditing] = useState<string | null>(null); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  async function remove(webhook: WebhookConfig) {
    if (!window.confirm("Delete this webhook destination?")) return;
    setBusy(true); setError("");
    try { await mutate(session, revision, `${basePath}/${webhook.id}`, "DELETE"); onChanged(); }
    catch (caught) { setError(caught instanceof Error ? caught.message : "Webhook could not be deleted."); } finally { setBusy(false); }
  }
  return <section class="webhook-manager"><div class="nested-heading"><div><strong>{title}</strong><span>{webhooks.length ? `${webhooks.length} direct` : inherited ? `Inherits ${inherited}` : "None configured"}</span></div><button class="button button--secondary" disabled={busy || session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => { setCreating(!creating); setEditing(null); }}>Add webhook</button></div>
    {creating && <WebhookEditor session={session} revision={revision} basePath={basePath} onCancel={() => setCreating(false)} onChanged={onChanged} />}{error && <p class="error" role="alert">{error}</p>}
    {webhooks.map((webhook) => <div class="webhook" key={webhook.id}><div><code>{webhook.urlRef ? webhook.urlRef : webhook.url ?? "Direct URL"}</code>{webhook.authentication?.type === "hmac-sha256" && <span>HMAC{webhook.authentication.keyId ? ` · ${webhook.authentication.keyId}` : ""}</span>}</div><div class="toolbar"><button class="button button--secondary" disabled={busy || session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => { setEditing(editing === webhook.id ? null : webhook.id); setCreating(false); }}>Edit</button><button class="button button--danger" disabled={busy || session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void remove(webhook)}>Delete</button></div>{editing === webhook.id && <WebhookEditor session={session} revision={revision} basePath={basePath} webhook={webhook} onCancel={() => setEditing(null)} onChanged={onChanged} />}</div>)}
  </section>;
}

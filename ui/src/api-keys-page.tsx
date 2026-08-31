import { useEffect, useState } from "preact/hooks";
import type { Session } from "./app";
import { action } from "./api";

type Key = { id: string; name: string; role: "admin" | "read-only"; prefix: string; createdAt: string; lastUsedAt?: string; revokedAt?: string };

export function APIKeysPage({ session }: { session: Session }) {
  const [keys, setKeys] = useState<Key[]>([]);
  const [error, setError] = useState("");
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const [secret, setSecret] = useState("");

  async function load() {
    const response = await fetch("/api/v1/api-keys");
    if (!response.ok) throw new Error("API keys could not be loaded");
    setKeys((await response.json()).apiKeys ?? []);
  }
  useEffect(() => { void load().catch((e) => setError(e.message)); }, []);

  async function create() {
    if (!confirm(`Create ${name} as a read-only API key?`)) return;
    const result = await action<{ secret: string }>(session, "/api/v1/api-keys", { name, confirm: true });
    setSecret(result.secret); setName(""); setAdding(false); await load();
  }
  async function changeRole(key: Key) {
    const role = key.role === "admin" ? "read-only" : "admin";
    const verb = role === "admin" ? "Promote" : "Demote";
    if (!confirm(`${verb} API key ${key.name} to ${role}? Existing clients receive the new permissions immediately.`)) return;
    await action(session, `/api/v1/api-keys/${key.id}/role`, { role, confirm: true });
    await load();
  }
  async function revoke(key: Key) {
    if (!confirm(`Revoke API key ${key.name}? This takes effect immediately.`)) return;
    await action(session, `/api/v1/api-keys/${key.id}/revoke`, { confirm: true });
    await load();
  }

  return <section class="activity api-keys-view">
    <div class="section-title"><div><p class="eyebrow">Automation access</p><h1>API keys</h1><p class="section-description">New API keys start read-only. Promote only integrations that must change Engine state.</p></div><div class="toolbar"><button class="button button--secondary" onClick={() => void load().catch((e) => setError(e.message))}>Refresh</button><button class="button" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => setAdding(!adding)}>{adding ? "Cancel" : "Create API key"}</button></div></div>
    {error && <p class="alert" role="alert">{error}</p>}
    {secret && <section class="panel one-time-secret" role="status"><div><strong>Copy this secret now</strong><span>It will not be shown again.</span><code>{secret}</code></div><button class="button button--secondary" onClick={() => void navigator.clipboard.writeText(secret)}>Copy secret</button><button class="button button--secondary" onClick={() => setSecret("")}>Done</button></section>}
    {adding && <form class="panel key-create" onSubmit={(event) => { event.preventDefault(); setError(""); void create().catch((e) => setError(e.message)); }}><div><h2>Create read-only API key</h2><p>Administrative access can be granted from the key list after creation.</p></div><label>Key name<input required maxLength={100} value={name} onInput={(e) => setName(e.currentTarget.value)} /></label><button class="button" type="submit">Create read-only key</button></form>}
    <div class="panel"><table class="users-table"><thead><tr><th>Name</th><th>Prefix</th><th>Role</th><th>Last used</th><th>Status</th><th>Actions</th></tr></thead><tbody>{keys.map((key) => <tr key={key.id}><td><strong>{key.name}</strong></td><td><code>{key.prefix}…</code></td><td><span class="role">{key.role}</span></td><td>{key.lastUsedAt ? new Date(key.lastUsedAt).toLocaleString() : "Never"}</td><td>{key.revokedAt ? "Revoked" : "Active"}</td><td>{!key.revokedAt && <div class="toolbar"><button class="button button--secondary" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void changeRole(key).catch((e) => setError(e.message))}>{key.role === "admin" ? "Make read-only" : "Promote to admin"}</button><button class="button button--danger" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void revoke(key).catch((e) => setError(e.message))}>Revoke</button></div>}</td></tr>)}</tbody></table></div>
  </section>;
}

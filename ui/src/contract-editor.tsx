import { useState } from "preact/hooks";
import type { ContractConfig, Session, RPCListener } from "./app";
import { mutate } from "./api";
import { WebhookManager } from "./webhook-editor";

type ABIInput = { type?: string; name?: string; inputs?: ABIInput[]; components?: ABIInput[]; anonymous?: boolean };

function canonicalType(input: ABIInput): string {
  const type = input.type ?? "";
  if (!type.startsWith("tuple")) return type;
  const suffix = type.slice("tuple".length);
  return `(${(input.components ?? []).map(canonicalType).join(",")})${suffix}`;
}

function parseABI(document: string): { abi: ABIInput[]; selectors: string[] } {
  let value: unknown;
  try { value = JSON.parse(document); } catch { throw new Error("ABI must be valid JSON."); }
  if (!Array.isArray(value)) throw new Error("ABI must be a JSON array.");
  const abi = value as ABIInput[];
  const selectors = abi.filter((item) => item?.type === "event" && item.anonymous !== true && typeof item.name === "string" && item.name)
    .map((item) => `${item.name}(${(item.inputs ?? []).map(canonicalType).join(",")})`);
  if (new Set(selectors).size !== selectors.length) throw new Error("ABI contains duplicate event signatures.");
  return { abi, selectors };
}

function ContractEditor({ session, revision, listenerID, contract, onCancel, onChanged }: {
  session: Session; revision: number; listenerID: string; contract?: ContractConfig; onCancel: () => void; onChanged: () => void;
}) {
  const [address, setAddress] = useState(contract?.address ?? "");
  const [abiText, setABIText] = useState(() => contract ? JSON.stringify(contract.abi, null, 2) : "");
  const [selected, setSelected] = useState<string[]>(() => contract?.events.map((event) => event.selector) ?? []);
  const [discovered, setDiscovered] = useState<string[]>(() => { try { return parseABI(contract ? JSON.stringify(contract.abi) : "[]").selectors; } catch { return []; } });
  const [busy, setBusy] = useState(false); const [error, setError] = useState("");

  function inspect(text = abiText) {
    setError("");
    try {
      const result = parseABI(text);
      setDiscovered(result.selectors);
      setSelected((current) => current.filter((selector) => result.selectors.includes(selector)));
    } catch (caught) { setDiscovered([]); setError(caught instanceof Error ? caught.message : "ABI could not be inspected."); }
  }
  async function upload(file?: File) {
    if (!file) return;
    const text = await file.text(); setABIText(text); inspect(text);
  }
  async function submit(event: Event) {
    event.preventDefault();
    if (!window.confirm(contract ? `Save changes to contract ${contract.address}?` : `Create contract ${address.trim()}?`)) return;
    setBusy(true); setError("");
    try {
      const parsed = parseABI(abiText);
      if (parsed.selectors.length !== discovered.length || parsed.selectors.some((selector, index) => selector !== discovered[index])) {
        setDiscovered(parsed.selectors);
        setSelected((current) => current.filter((selector) => parsed.selectors.includes(selector)));
        throw new Error("ABI changed since the last inspection. Review the refreshed event list, then save again.");
      }
      const path = `/api/v1/rpc-listeners/${listenerID}/contracts${contract ? `/${contract.id}` : ""}`;
      const body = contract
        ? { address: address.trim(), abi: parsed.abi, eventSelectors: selected }
        : { address: address.trim(), abi: parsed.abi, events: selected.map((selector) => ({ selector, webhooks: [] })), webhooks: [] };
      await mutate(session, revision, path, contract ? "PATCH" : "POST", body); onChanged();
    } catch (caught) { setError(caught instanceof Error ? caught.message : "Contract could not be saved."); } finally { setBusy(false); }
  }
  function toggle(selector: string, checked: boolean) { setSelected(checked ? [...selected, selector] : selected.filter((item) => item !== selector)); }
  return <form class="panel editor contract-editor" onSubmit={(event) => void submit(event)}>
    <div class="editor__heading"><div><h3>{contract ? "Edit contract" : "Add contract"}</h3><p>Paste or upload an ABI, inspect its events, then choose which signatures RedDotRelay should scan.</p></div><button class="icon-button" type="button" aria-label="Close contract editor" onClick={onCancel}>×</button></div>
    <label class="wide-field">Contract address<input value={address} onInput={(event) => setAddress(event.currentTarget.value)} placeholder="0x…" pattern="0x[0-9a-fA-F]{40}" spellcheck={false} required /></label>
    <label class="wide-field">ABI JSON<textarea value={abiText} onInput={(event) => setABIText(event.currentTarget.value)} spellcheck={false} placeholder='[{"type":"event",…}]' required /></label>
    <div class="toolbar"><label class="button button--secondary file-button">Upload ABI<input type="file" accept=".json,application/json" onChange={(event) => void upload(event.currentTarget.files?.[0])} /></label><button class="button button--secondary" type="button" onClick={() => inspect()}>Inspect events</button></div>
    {discovered.length === 0 ? <p class="hint">No non-anonymous events discovered yet.</p> : <fieldset class="event-selector"><legend>Events ({selected.length} selected)</legend>{discovered.map((selector) => <label key={selector}><input type="checkbox" checked={selected.includes(selector)} onChange={(event) => toggle(selector, event.currentTarget.checked)} /><code>{selector}</code></label>)}</fieldset>}
    {error && <p class="error" role="alert">{error}</p>}
    <div class="toolbar"><button class="button" disabled={busy}>{busy ? "Saving…" : contract ? "Save contract" : "Create contract"}</button><button class="button button--secondary" type="button" onClick={onCancel}>Cancel</button></div>
  </form>;
}

export function ContractManager({ session, revision, listener, onChanged }: { session: Session; revision: number; listener: RPCListener; onChanged: () => void }) {
  const [creating, setCreating] = useState(false); const [editing, setEditing] = useState<string | null>(null); const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  async function remove(contract: ContractConfig) {
    if (!window.confirm(`Delete contract ${contract.address} and its nested events and webhooks?`)) return;
    setBusy(true); setError("");
    try { await mutate(session, revision, `/api/v1/rpc-listeners/${listener.id}/contracts/${contract.id}`, "DELETE"); onChanged(); }
    catch (caught) { setError(caught instanceof Error ? caught.message : "Contract could not be deleted."); } finally { setBusy(false); }
  }
  return <section class="contract-manager" aria-label={`Contracts for ${listener.name}`}>
    <div class="nested-heading"><div><strong>Contracts and events</strong><span>{listener.contracts.length} configured</span></div><button class="button button--secondary" disabled={busy || session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => { setCreating(!creating); setEditing(null); }}>Add contract</button></div>
    {creating && <ContractEditor session={session} revision={revision} listenerID={listener.id} onCancel={() => setCreating(false)} onChanged={onChanged} />}
    {error && <p class="error" role="alert">{error}</p>}
    <div class="contract-list">{listener.contracts.map((contract) => <article class="contract" key={contract.id}>
      <div class="contract__heading"><div><code>{contract.address}</code><span>{contract.events.length} event{contract.events.length === 1 ? "" : "s"}</span></div><div class="toolbar"><button class="button button--secondary" disabled={busy || session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => { setEditing(editing === contract.id ? null : contract.id); setCreating(false); }}>Edit</button><button class="button button--danger" disabled={busy || session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void remove(contract)}>Delete</button></div></div>
      {editing === contract.id && <ContractEditor session={session} revision={revision} listenerID={listener.id} contract={contract} onCancel={() => setEditing(null)} onChanged={onChanged} />}
      <WebhookManager session={session} revision={revision} title="Contract webhooks" basePath={`/api/v1/rpc-listeners/${listener.id}/contracts/${contract.id}/webhooks`} webhooks={contract.webhooks} inherited="RPC listener or global webhooks" onChanged={onChanged} />
      {contract.events.length > 0 && <ul class="selector-list">{contract.events.map((event) => <li key={event.id}><code>{event.selector}</code><span>{event.webhooks.length > 0 ? `${event.webhooks.length} direct webhook(s)` : `inherits ${event.webhookSource}`}</span></li>)}</ul>}
      {contract.events.map((event) => <WebhookManager key={`webhooks-${event.id}`} session={session} revision={revision} title={`Event: ${event.selector}`} basePath={`/api/v1/rpc-listeners/${listener.id}/contracts/${contract.id}/events/${event.id}/webhooks`} webhooks={event.webhooks} inherited={`${event.webhookSource} webhooks`} onChanged={onChanged} />)}
    </article>)}</div>
  </section>;
}

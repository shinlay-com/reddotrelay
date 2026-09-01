import { useState } from "preact/hooks";
import type { Session, RPCListener } from "./app";
import { action, mutate } from "./api";

type LocatorMode = "direct" | "reference" | "unchanged";
type RPCListenerDraft = {
  name: string; chainId: string; locatorMode: LocatorMode; locator: string; startBlock: string; batchSize: string;
  pollInterval: string; confirmations: string; reorgDepth: string; rpcRetryAttempts: string;
	  rpcRetryBackoff: string; rpcTimeout: string; caPem: string; serverName: string; insecureSkipVerify: boolean; paused: boolean;
	  authType: string; authUsername: string; authHeaderName: string; authSecret: string; authTokenUrl: string; authTokenApiKey: string;
};

function draftFor(listener?: RPCListener): RPCListenerDraft {
	if (!listener) return { name: "", chainId: "1", locatorMode: "direct", locator: "", startBlock: "0", batchSize: "1000", pollInterval: "5s", confirmations: "12", reorgDepth: "64", rpcRetryAttempts: "3", rpcRetryBackoff: "1s", rpcTimeout: "10s", caPem: "", serverName: "", insecureSkipVerify: false, paused: true, authType: "", authUsername: "", authHeaderName: "", authSecret: "", authTokenUrl: "", authTokenApiKey: "" };
  return {
    name: listener.name, chainId: String(listener.chainId), locatorMode: listener.rpcUrlRef ? "reference" : "unchanged", locator: listener.rpcUrlRef ?? "",
    startBlock: String(listener.startBlock), batchSize: String(listener.batchSize), pollInterval: listener.pollInterval,
    confirmations: String(listener.confirmations), reorgDepth: String(listener.reorgDepth), rpcRetryAttempts: String(listener.rpcRetryAttempts),
    rpcRetryBackoff: listener.rpcRetryBackoff, rpcTimeout: listener.rpcTimeout, caPem: listener.tls?.caPem ?? "", serverName: listener.tls?.serverName ?? "",
		insecureSkipVerify: listener.tls?.insecureSkipVerify ?? false, paused: listener.paused, authType: listener.rpcAuthentication?.type ?? "", authUsername: listener.rpcAuthentication?.username ?? "", authHeaderName: listener.rpcAuthentication?.headerName ?? "", authSecret: "", authTokenUrl: listener.rpcAuthentication?.tokenUrl ?? "", authTokenApiKey: ""
  };
}

function integer(value: string) { return Number.parseInt(value, 10); }

export function ListenerEditor({ session, revision, listener, onCancel, onChanged }: { session: Session; revision: number; listener?: RPCListener; onCancel: () => void; onChanged: () => void }) {
  const [draft, setDraft] = useState(() => draftFor(listener));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState("");
  const [showAdvancedConnection, setShowAdvancedConnection] = useState(() => Boolean(listener?.tls?.caPem || listener?.tls?.insecureSkipVerify));
  const editing = Boolean(listener);
  function update<K extends keyof RPCListenerDraft>(name: K, value: RPCListenerDraft[K]) { setDraft({ ...draft, [name]: value }); }
  function chooseLocatorMode(locatorMode: LocatorMode) { setDraft((current) => ({ ...current, locatorMode, locator: locatorMode === "unchanged" ? current.locator : "" })); }
  function field(name: keyof RPCListenerDraft, label: string, type = "text", min?: string, required = true) {
    return <label>{label}<input type={type} min={min} value={String(draft[name])} onInput={(event) => update(name, event.currentTarget.value as never)} required={required} /></label>;
  }
  function locatorBody() {
    if (draft.locatorMode === "direct") return { rpcUrl: draft.locator };
    if (draft.locatorMode === "reference") return { rpcUrlRef: draft.locator };
    return {};
  }
	function commonBody() {
    return {
      name: draft.name.trim(), chainId: integer(draft.chainId), ...locatorBody(), startBlock: integer(draft.startBlock), batchSize: integer(draft.batchSize),
      pollInterval: draft.pollInterval, confirmations: integer(draft.confirmations), reorgDepth: integer(draft.reorgDepth), rpcRetryAttempts: integer(draft.rpcRetryAttempts),
      rpcRetryBackoff: draft.rpcRetryBackoff, rpcTimeout: draft.rpcTimeout,
		tls: { caPem: draft.caPem, serverName: draft.serverName, insecureSkipVerify: draft.insecureSkipVerify },
		...(draft.authType ? { rpcAuthentication: { type: draft.authType, username: draft.authUsername, headerName: draft.authHeaderName, secret: draft.authSecret, tokenUrl: draft.authTokenUrl, tokenApiKey: draft.authTokenApiKey } } : (listener?.rpcAuthentication?.secretConfigured ? { rpcAuthentication: null } : {}))
    };
  }
  async function testRPC() {
    setBusy(true); setError(""); setResult("");
    try {
      if (draft.locatorMode === "unchanged") throw new Error("Choose Direct URL or Secret reference to test a replacement. The stored direct URL is redacted and cannot be reconstructed by the UI.");
      const body = commonBody();
      const rpcAuthentication = { type: draft.authType, username: draft.authUsername, headerName: draft.authHeaderName, secret: draft.authSecret, tokenUrl: draft.authTokenUrl, tokenApiKey: draft.authTokenApiKey };
      const response = await action<{ reachable: true; chainId: number }>(session, "/api/v1/connection-tests/rpc", { ...locatorBody(), tls: body.tls, ...(draft.authType ? { rpcAuthentication } : {}) });
      setResult(`Connected successfully. RPC chain ID: ${response.chainId}.`);
    } catch (caught) { setError(caught instanceof Error ? caught.message : "RPC test failed."); } finally { setBusy(false); }
  }
  async function submit(event: Event) {
    event.preventDefault();
    if (!window.confirm(editing ? `Save changes to RPC listener “${listener?.name}”?` : `Create RPC listener “${draft.name.trim()}”?`)) return;
    setBusy(true); setError(""); setResult("");
    try {
      const body = commonBody();
      if (editing && listener) await mutate(session, revision, `/api/v1/rpc-listeners/${listener.id}`, "PATCH", body);
      else await mutate(session, revision, "/api/v1/rpc-listeners", "POST", { ...body, paused: draft.paused, contracts: [], webhooks: [] });
      onChanged();
    } catch (caught) { setError(caught instanceof Error ? caught.message : "RPC listener could not be saved."); } finally { setBusy(false); }
  }
  return <form class="panel editor" onSubmit={(event) => void submit(event)}>
    <div class="editor__heading"><div><h3>{editing ? `Edit ${listener?.name}` : "Add RPC listener"}</h3><p>Connect an EVM RPC endpoint, then configure the contracts and events this listener should process. Use a reference for protected endpoint values.</p></div><button class="icon-button" type="button" aria-label="Close RPC listener editor" onClick={onCancel}>×</button></div>
    <fieldset class="locator"><legend>RPC connection</legend><div class="segmented">
      {editing && <label><input type="radio" name={`locator-${listener?.id}`} checked={draft.locatorMode === "unchanged"} onChange={() => chooseLocatorMode("unchanged")} /> Keep current</label>}
      <label><input type="radio" name={`locator-${listener?.id ?? "new"}`} checked={draft.locatorMode === "direct"} onChange={() => chooseLocatorMode("direct")} /> Direct URL</label>
      <label><input type="radio" name={`locator-${listener?.id ?? "new"}`} checked={draft.locatorMode === "reference"} onChange={() => chooseLocatorMode("reference")} /> Secret reference</label>
    </div>{draft.locatorMode !== "unchanged" && <label>{draft.locatorMode === "direct" ? "RPC URL" : "RPC URL secret reference"}<input type="text" value={draft.locator} onInput={(event) => update("locator", event.currentTarget.value)} placeholder={draft.locatorMode === "direct" ? "http://host.docker.internal:8545" : "env://CHAIN_RPC_URL"} spellcheck={false} required /></label>}
      {draft.locatorMode === "reference" && <p class="hint">Examples: env://CHAIN_RPC_URL or file:///run/secrets/rpc_url</p>}
	  <label class="check"><input type="checkbox" checked={showAdvancedConnection} onChange={(event) => setShowAdvancedConnection(event.currentTarget.checked)} /> Show advanced connection options</label>
	  {showAdvancedConnection && <div class="connection-advanced"><label class="wide-field">Custom TLS CA PEM<textarea value={draft.caPem} onInput={(event) => update("caPem", event.currentTarget.value)} spellcheck={false} placeholder="Optional PEM certificate bundle" /></label><label class="check"><input type="checkbox" checked={draft.insecureSkipVerify} onChange={(event) => update("insecureSkipVerify", event.currentTarget.checked)} /> Disable RPC TLS certificate verification (unsafe)</label></div>}
	</fieldset>
	<fieldset class="locator"><legend>RPC authentication</legend><label>Method<select value={draft.authType} onChange={(event) => update("authType", event.currentTarget.value)}><option value="">None / URL credential</option><option value="basic">HTTP Basic</option><option value="bearer">Bearer token</option><option value="header">Custom header</option><option value="engine-jwt">Ethereum Engine JWT</option><option value="provider-jwt">Provider JWT token endpoint</option></select></label>{draft.authType === "basic" && <label>Username<input value={draft.authUsername} onInput={(event) => update("authUsername", event.currentTarget.value)} required /></label>}{draft.authType === "header" && <label>Header name<input value={draft.authHeaderName} onInput={(event) => update("authHeaderName", event.currentTarget.value)} placeholder="X-API-Key" required /></label>}{draft.authType === "provider-jwt" && <><label>Token endpoint URL<input type="url" value={draft.authTokenUrl} onInput={(event) => update("authTokenUrl", event.currentTarget.value)} placeholder="https://provider.example/api/external/generate-access-token" required /></label><label>Token API key<input type="password" value={draft.authTokenApiKey} onInput={(event) => update("authTokenApiKey", event.currentTarget.value)} placeholder={listener?.rpcAuthentication?.tokenApiKeyConfigured ? "Enter only to replace the configured API key" : "x-api-key"} required={!listener?.rpcAuthentication?.tokenApiKeyConfigured} /></label></>}{draft.authType && <label>{draft.authType === "provider-jwt" ? "Precomputed RSA signature" : "Secret"}<input type="password" value={draft.authSecret} onInput={(event) => update("authSecret", event.currentTarget.value)} placeholder={listener?.rpcAuthentication?.secretConfigured ? "Enter only to replace the configured secret" : "Credential secret"} required={!listener?.rpcAuthentication?.secretConfigured} /></label>}<p class="hint">Credentials are encrypted locally using the Engine operator key and are never shown again.</p></fieldset>
    <div class="form-grid">{field("name", "Name")}{field("chainId", "Chain ID", "number", "1")}{field("startBlock", "Start block", "number", "0")}{field("batchSize", "Batch size", "number", "1")}{field("pollInterval", "Poll interval")}{field("confirmations", "Confirmations", "number", "0")}{field("reorgDepth", "Reorg depth", "number", "1")}{field("rpcRetryAttempts", "RPC retry attempts", "number", "1")}{field("rpcRetryBackoff", "RPC retry backoff")}{field("rpcTimeout", "RPC timeout")}{field("serverName", "TLS server name", "text", undefined, false)}</div>
    {!editing && <label class="check"><input type="checkbox" checked={draft.paused} onChange={(event) => update("paused", event.currentTarget.checked)} /> Create as paused draft</label>}
    {result && <p class="success" role="status">{result}</p>}{error && <p class="error" role="alert">{error}</p>}
    <div class="toolbar"><button class="button button--secondary" type="button" disabled={busy || draft.locatorMode === "unchanged" || !draft.locator.trim()} onClick={() => void testRPC()}>Test RPC</button><button class="button" disabled={busy}>{busy ? "Working…" : editing ? "Save RPC listener" : "Create RPC listener"}</button><button class="button button--secondary" type="button" onClick={onCancel}>Cancel</button></div>
  </form>;
}

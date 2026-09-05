import { useEffect, useMemo, useState } from "preact/hooks";
import type { ConfigSnapshot, RuntimeListener, Session, RPCListener } from "./app";
import { ConfigurationTransfer, ListenerActions } from "./configuration";
import { ContractManager } from "./contract-editor";
import { WebhookManager } from "./webhook-editor";
import { ListenerEditor } from "./listener-editor";
import { ScannerSkip } from "./scanner-skip";

type Tab = "overview" | "contracts" | "webhooks" | "settings";
type Filter = "all" | "attention" | "paused";
type Health = "synced" | "catching-up" | "lagging" | "paused" | "idle" | "failed";
type Progress = { rpcListenerId: string; latestBlock: number; confirmedHead: number; checkpoint: number; lagBlocks: number; updatedAt: string; lastFetchMs?: number; averageFetchMs?: number; lastVerifyMs?: number; averageVerifyMs?: number; lastError?: string; lastErrorAt?: string };

function status(listener: RPCListener, runtime: RuntimeListener | undefined, progress: Progress | undefined): Health {
  if (listener.paused) return "paused";
  if (runtime?.state === "idle") return "idle";
  if (progress?.lastError) return "failed";
  // A fresh runtime can be scanning before its first progress poll arrives.
  // Treat it as catching up, not failed, until an actual scanner error exists.
  if (!progress) return runtime?.state === "running" ? "lagging" : "failed";
  if (progress.lagBlocks > 0) return runtime?.state === "running" ? "catching-up" : "lagging";
  return runtime?.state === "running" ? "synced" : "failed";
}

function failureReason(runtime: RuntimeListener | undefined, progress: Progress | undefined) { return progress?.lastError ?? runtime?.lastError; }
function formatFetchDuration(milliseconds: number | undefined) { return milliseconds === undefined ? "—" : `${(milliseconds / 1_000).toLocaleString(undefined, { maximumFractionDigits: 2 })} s`; }
function healthLabel(health: Health) { return health === "catching-up" ? "Catching up" : health; }

export function ListenersPage({ session, snapshot, runtimeListeners, onChanged }: { session: Session; snapshot: ConfigSnapshot; runtimeListeners: RuntimeListener[]; onChanged: () => void }) {
  const [selectedID, setSelectedID] = useState(snapshot.rpcListeners[0]?.id ?? "");
  const [tab, setTab] = useState<Tab>("overview");
  const [filter, setFilter] = useState<Filter>("all");
  const [query, setQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  const [progress, setProgress] = useState<Progress[]>([]);
  const runtimeByID = new Map(runtimeListeners.map((item) => [item.id, item]));
  const progressByID = new Map(progress.map((item) => [item.rpcListenerId, item]));
  const selected = snapshot.rpcListeners.find((item) => item.id === selectedID) ?? snapshot.rpcListeners[0];

  useEffect(() => { if (selected && selected.id !== selectedID) { setSelectedID(selected.id); setEditing(false); } }, [selected?.id]);
  useEffect(() => {
    let active = true;
    async function loadProgress() { try { const response = await fetch("/api/v1/scanner-progress", { headers: { Accept: "application/json" } }); if (response.ok && active) setProgress(((await response.json()) as { rpcListeners?: Progress[] }).rpcListeners ?? []); } catch { /* Configuration remains usable if progress is unavailable. */ } }
    void loadProgress();
    const timer = window.setInterval(() => void loadProgress(), 5_000);
    return () => { active = false; window.clearInterval(timer); };
  }, []);

  const visible = useMemo(() => snapshot.rpcListeners.filter((listener) => {
    const state = status(listener, runtimeByID.get(listener.id), progressByID.get(listener.id));
    const matchesState = filter === "all" || filter === "paused" && state === "paused" || filter === "attention" && (state === "failed" || state === "lagging");
    return matchesState && `${listener.name} ${listener.chainId}`.toLowerCase().includes(query.trim().toLowerCase());
  }), [snapshot.rpcListeners, runtimeListeners, progress, filter, query]);
  const attention = snapshot.rpcListeners.filter((listener) => ["failed", "lagging"].includes(status(listener, runtimeByID.get(listener.id), progressByID.get(listener.id)))).length;
  const paused = snapshot.rpcListeners.filter((listener) => listener.paused).length;
  const selectedProgress = selected ? progressByID.get(selected.id) : undefined;
  const selectedRuntime = selected ? runtimeByID.get(selected.id) : undefined;
  const selectedHealth = selected ? status(selected, selectedRuntime, selectedProgress) : "failed";
  const selectedFailure = failureReason(selectedRuntime, selectedProgress);

  return <section class="listeners-redesign">
    <div class="section-title"><div><p class="eyebrow">Engine configuration</p><h1>RPC listeners</h1><p class="section-description">Connect EVM endpoints and manage their contracts, events, and delivery routes.</p></div><div class="toolbar"><button class="button button--secondary" onClick={onChanged}>Refresh</button><button class="button button--secondary" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => setAdvanced(!advanced)}>Advanced JSON</button><button class="button" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => setCreating(!creating)}>Add RPC listener</button></div></div>
    {session.role !== "admin" && <section class="panel permission"><strong>Read-only access</strong><span>You can inspect configuration, but administrator access is required to make changes.</span></section>}
    {creating && <ListenerEditor session={session} revision={snapshot.revision} onCancel={() => setCreating(false)} onChanged={() => { setCreating(false); onChanged(); }} />}
    {advanced && <ConfigurationTransfer session={session} revision={snapshot.revision} onChanged={onChanged} />}
    <div class="listener-controls"><label><span>⌕</span><input value={query} onInput={(event) => setQuery(event.currentTarget.value)} placeholder="Search listeners or chain ID" /></label><div>{([['all', 'All', snapshot.rpcListeners.length], ['attention', 'Needs attention', attention], ['paused', 'Paused', paused]] as const).map(([id, label, count]) => <button class={filter === id ? "active" : ""} onClick={() => setFilter(id)}>{label}<b>{count}</b></button>)}</div></div>
    <div class="listener-master-detail">
      <div class="listener-master panel">{visible.length === 0 ? <p class="overview-empty">No listeners match this view.</p> : visible.map((listener) => { const listenerProgress = progressByID.get(listener.id); const listenerRuntime = runtimeByID.get(listener.id); const health = status(listener, listenerRuntime, listenerProgress); const reason = failureReason(listenerRuntime, listenerProgress); return <button class={selected?.id === listener.id ? "selected" : ""} onClick={() => { setSelectedID(listener.id); setTab("overview"); setEditing(false); }}><span>{listener.name.slice(0, 1).toUpperCase()}</span><div><strong>{listener.name}</strong><small>Chain {listener.chainId} · {listener.contracts.length} contract{listener.contracts.length === 1 ? "" : "s"}</small><code>{listener.rpcUrlRef ?? listener.rpcUrl ?? "Protected RPC endpoint"}</code>{health === "idle" && <small>No event subscriptions configured</small>}{health === "failed" && reason && <small class="connection-error" title={reason}>{reason}</small>}</div><b class={`progress-status progress-status--${health}`}>{healthLabel(health)}</b><i>›</i></button>; })}</div>
      {selected ? <article class="panel listener-detail">
        <header><div><span>{selected.name.slice(0, 1).toUpperCase()}</span><div><div><h2>{selected.name}</h2><b class={`progress-status progress-status--${selectedHealth}`}>{healthLabel(selectedHealth)}</b></div><p>Chain {selected.chainId} · {selectedHealth === "synced" ? "Scanner is caught up" : selectedHealth === "catching-up" ? "Scanner is actively processing confirmed blocks" : selectedHealth === "lagging" ? "Scanner is behind the confirmed head" : selectedHealth === "paused" ? "Listener is disabled by configuration" : selectedHealth === "idle" ? "No event subscriptions configured" : "Listener needs attention"}</p>{selectedHealth === "failed" && selectedFailure && <p class="connection-error">{selectedFailure}{selectedProgress?.lastErrorAt && <small>{new Date(selectedProgress.lastErrorAt).toLocaleString()}</small>}</p>}</div></div><ListenerActions session={session} revision={snapshot.revision} listener={selected} onChanged={onChanged} onEdit={() => setEditing(true)} /></header>
        <nav>{([['overview','Overview'],['settings','Scanner settings'],['webhooks','Webhooks'],['contracts','Contracts & events']] as const).map(([id,label]) => <button class={tab === id ? "active" : ""} onClick={() => setTab(id)}>{label}{id === "contracts" && <b>{selected.contracts.length}</b>}{id === "webhooks" && <b>{selected.webhooks.length}</b>}</button>)}</nav>
        {editing && <div class="listener-editor-panel"><ListenerEditor session={session} revision={snapshot.revision} listener={selected} onCancel={() => setEditing(false)} onChanged={() => { setEditing(false); onChanged(); }} /></div>}
        {tab === "overview" && <div class="listener-overview">{selectedHealth === "idle" && <section class="listener-idle-guidance"><div><strong>Scanning is waiting for an event subscription</strong><p>Add a contract and select at least one event before this listener can process logs.</p></div><button class="button" type="button" onClick={() => setTab("contracts")}>Configure events</button></section>}<div class="listener-metrics"><div><span>Latest block</span><strong>{selectedProgress?.latestBlock.toLocaleString() ?? "—"}</strong></div><div><span>Checkpoint</span><strong>{selectedProgress?.checkpoint.toLocaleString() ?? "—"}</strong></div><div><span>Lag</span><strong>{selectedProgress ? `${selectedProgress.lagBlocks.toLocaleString()} blocks` : "—"}</strong></div><div><span>Runtime</span><strong>{selectedRuntime?.state ?? "Not reported"}</strong></div><div><span>Last batch fetch</span><strong>{formatFetchDuration(selectedProgress?.lastFetchMs)}</strong></div><div><span>Average batch fetch</span><strong>{formatFetchDuration(selectedProgress?.averageFetchMs)}</strong></div></div><section><h3>Connection</h3><dl><div><dt>RPC endpoint</dt><dd><code>{selected.rpcUrlRef ?? selected.rpcUrl ?? "Protected direct URL"}</code></dd></div><div><dt>Chain ID</dt><dd>{selected.chainId}</dd></div><div><dt>Start block</dt><dd>{selected.startBlock.toLocaleString()}</dd></div><div><dt>Last error</dt><dd class={selectedProgress?.lastError || selectedRuntime?.lastError ? "connection-error" : undefined}>{selectedProgress?.lastError ?? selectedRuntime?.lastError ?? "None"}{selectedProgress?.lastError && selectedProgress.lastErrorAt && <small>{new Date(selectedProgress.lastErrorAt).toLocaleString()}</small>}</dd></div></dl></section><section><h3>Configured resources</h3><div class="listener-resource-links"><button onClick={() => setTab("contracts")}><span>◇</span><div><strong>{selected.contracts.length} contracts</strong><small>{selected.contracts.reduce((sum, contract) => sum + contract.events.length, 0)} selected events</small></div><i>›</i></button><button onClick={() => setTab("webhooks")}><span>→</span><div><strong>{selected.webhooks.length} listener webhooks</strong><small>{snapshot.globalWebhooks.length} global routes</small></div><i>›</i></button></div></section></div>}
        {tab === "overview" && <section class="listener-performance"><h3>Performance</h3><p>Fetch timing covers <code>eth_getLogs</code>; verification timing covers canonical block header checks before persistence.</p><dl><div><dt>Last batch fetch</dt><dd>{formatFetchDuration(selectedProgress?.lastFetchMs)}</dd></div><div><dt>Average batch fetch</dt><dd>{formatFetchDuration(selectedProgress?.averageFetchMs)}</dd></div><div><dt>Last verification</dt><dd>{formatFetchDuration(selectedProgress?.lastVerifyMs)}</dd></div><div><dt>Average verification</dt><dd>{formatFetchDuration(selectedProgress?.averageVerifyMs)}</dd></div></dl></section>}
        {tab === "overview" && <ScannerSkip key={selected.id} session={session} listener={selected} revision={snapshot.revision} runtimeState={selectedRuntime?.state} onChanged={onChanged} />}
        {tab === "contracts" && <ContractManager session={session} revision={snapshot.revision} listener={selected} onChanged={onChanged} />}
        {tab === "webhooks" && <div class="listener-tab-content"><WebhookManager session={session} revision={snapshot.revision} title="RPC listener webhooks" basePath={`/api/v1/rpc-listeners/${selected.id}/webhooks`} webhooks={selected.webhooks} inherited="global webhooks" onChanged={onChanged} /><WebhookManager session={session} revision={snapshot.revision} title="Global webhooks" basePath="/api/v1/rpc-listeners/webhooks" webhooks={snapshot.globalWebhooks} onChanged={onChanged} /></div>}
        {tab === "settings" && <div class="listener-tab-content listener-settings">
          <section>
            <h3>Scanner configuration</h3>
            <p>Current persisted values used by this RPC listener.</p>
            <dl>
              <div><dt>Start block</dt><dd>{selected.startBlock.toLocaleString()}</dd></div>
              <div><dt>Batch size</dt><dd>{selected.batchSize.toLocaleString()}</dd></div>
              <div><dt>Poll interval</dt><dd>{selected.pollInterval}</dd></div>
              <div><dt>Confirmations</dt><dd>{selected.confirmations.toLocaleString()}</dd></div>
              <div><dt>Reorg depth</dt><dd>{selected.reorgDepth.toLocaleString()}</dd></div>
              <div><dt>RPC retry attempts</dt><dd>{selected.rpcRetryAttempts.toLocaleString()}</dd></div>
              <div><dt>RPC retry backoff</dt><dd>{selected.rpcRetryBackoff}</dd></div>
              <div><dt>RPC timeout</dt><dd>{selected.rpcTimeout}</dd></div>
              <div><dt>TLS server name</dt><dd>{selected.tls.serverName || "Default from endpoint"}</dd></div>
              <div><dt>Custom TLS CA</dt><dd>{selected.tls.caPem ? "Configured" : "Not configured"}</dd></div>
              <div><dt>TLS verification</dt><dd>{selected.tls.insecureSkipVerify ? "Disabled (unsafe)" : "Enabled"}</dd></div>
            </dl>
          </section>
        </div>}
      </article> : <div class="panel empty"><h3>No RPC listeners configured</h3><p>Add a listener to begin scanning EVM events.</p></div>}
    </div>
  </section>;
}

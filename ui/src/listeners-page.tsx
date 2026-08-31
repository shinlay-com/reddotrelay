import { useEffect, useMemo, useState } from "preact/hooks";
import type { ConfigSnapshot, RuntimeListener, Session, RPCListener } from "./app";
import { ConfigurationTransfer, ListenerActions } from "./configuration";
import { ContractManager } from "./contract-editor";
import { WebhookManager } from "./webhook-editor";
import { ListenerEditor } from "./listener-editor";

type Tab = "overview" | "contracts" | "webhooks" | "settings";
type Filter = "all" | "attention" | "paused";
type Health = "synced" | "lagging" | "paused" | "failed";
type Progress = { rpcListenerId: string; latestBlock: number; confirmedHead: number; checkpoint: number; lagBlocks: number; updatedAt: string; lastError?: string; lastErrorAt?: string };

function status(listener: RPCListener, runtime: RuntimeListener | undefined, progress: Progress | undefined): Health {
  if (listener.paused) return "paused";
  if (!progress) return "failed";
  if (progress.lagBlocks > 0) return "lagging";
  return runtime?.state === "running" ? "synced" : "failed";
}

export function ListenersPage({ session, snapshot, runtimeListeners, onChanged }: { session: Session; snapshot: ConfigSnapshot; runtimeListeners: RuntimeListener[]; onChanged: () => void }) {
  const [selectedID, setSelectedID] = useState(snapshot.rpcListeners[0]?.id ?? "");
  const [tab, setTab] = useState<Tab>("overview");
  const [filter, setFilter] = useState<Filter>("all");
  const [query, setQuery] = useState("");
  const [creating, setCreating] = useState(false);
  const [advanced, setAdvanced] = useState(false);
  const [progress, setProgress] = useState<Progress[]>([]);
  const runtimeByID = new Map(runtimeListeners.map((item) => [item.id, item]));
  const progressByID = new Map(progress.map((item) => [item.rpcListenerId, item]));
  const selected = snapshot.rpcListeners.find((item) => item.id === selectedID) ?? snapshot.rpcListeners[0];

  useEffect(() => { if (selected && selected.id !== selectedID) setSelectedID(selected.id); }, [selected?.id]);
  useEffect(() => { void fetch("/api/v1/scanner-progress", { headers: { Accept: "application/json" } }).then(async (response) => { if (response.ok) setProgress(((await response.json()) as { rpcListeners?: Progress[] }).rpcListeners ?? []); }).catch(() => undefined); }, [snapshot.revision]);

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

  return <section class="listeners-redesign">
    <div class="section-title"><div><p class="eyebrow">Engine configuration</p><h1>RPC listeners</h1><p class="section-description">Connect EVM endpoints and manage their contracts, events, and delivery routes.</p></div><div class="toolbar"><button class="button button--secondary" onClick={onChanged}>Refresh</button><button class="button button--secondary" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => setAdvanced(!advanced)}>Advanced JSON</button><button class="button" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => setCreating(!creating)}>Add RPC listener</button></div></div>
    {session.role !== "admin" && <section class="panel permission"><strong>Read-only access</strong><span>You can inspect configuration, but administrator access is required to make changes.</span></section>}
    {creating && <ListenerEditor session={session} revision={snapshot.revision} onCancel={() => setCreating(false)} onChanged={() => { setCreating(false); onChanged(); }} />}
    {advanced && <ConfigurationTransfer session={session} revision={snapshot.revision} onChanged={onChanged} />}
    <div class="listener-controls"><label><span>⌕</span><input value={query} onInput={(event) => setQuery(event.currentTarget.value)} placeholder="Search listeners or chain ID" /></label><div>{([['all', 'All', snapshot.rpcListeners.length], ['attention', 'Needs attention', attention], ['paused', 'Paused', paused]] as const).map(([id, label, count]) => <button class={filter === id ? "active" : ""} onClick={() => setFilter(id)}>{label}<b>{count}</b></button>)}</div></div>
    <div class="listener-master-detail">
      <div class="listener-master panel">{visible.length === 0 ? <p class="overview-empty">No listeners match this view.</p> : visible.map((listener) => { const health = status(listener, runtimeByID.get(listener.id), progressByID.get(listener.id)); return <button class={selected?.id === listener.id ? "selected" : ""} onClick={() => { setSelectedID(listener.id); setTab("overview"); }}><span>{listener.name.slice(0, 1).toUpperCase()}</span><div><strong>{listener.name}</strong><small>Chain {listener.chainId} · {listener.contracts.length} contract{listener.contracts.length === 1 ? "" : "s"}</small><code>{listener.rpcUrlRef ?? listener.rpcUrl ?? "Protected RPC endpoint"}</code></div><b class={`progress-status progress-status--${health}`}>{health}</b><i>›</i></button>; })}</div>
      {selected ? <article class="panel listener-detail">
        <header><div><span>{selected.name.slice(0, 1).toUpperCase()}</span><div><div><h2>{selected.name}</h2><b class={`progress-status progress-status--${selectedHealth}`}>{selectedHealth}</b></div><p>Chain {selected.chainId} · {selectedHealth === "synced" ? "Scanner is caught up" : selectedHealth === "lagging" ? "Scanner is behind the confirmed head" : selectedHealth === "paused" ? "Listener is disabled by configuration" : "Listener needs attention"}</p></div></div></header>
        <nav>{([['overview','Overview'],['contracts','Contracts & events'],['webhooks','Webhooks'],['settings','Scanner settings']] as const).map(([id,label]) => <button class={tab === id ? "active" : ""} onClick={() => setTab(id)}>{label}{id === "contracts" && <b>{selected.contracts.length}</b>}{id === "webhooks" && <b>{selected.webhooks.length}</b>}</button>)}</nav>
        {tab === "overview" && <div class="listener-overview"><div class="listener-metrics"><div><span>Latest block</span><strong>{selectedProgress?.latestBlock.toLocaleString() ?? "—"}</strong></div><div><span>Checkpoint</span><strong>{selectedProgress?.checkpoint.toLocaleString() ?? "—"}</strong></div><div><span>Lag</span><strong>{selectedProgress ? `${selectedProgress.lagBlocks.toLocaleString()} blocks` : "—"}</strong></div><div><span>Runtime</span><strong>{selectedRuntime?.state ?? "Not reported"}</strong></div></div><section><h3>Connection</h3><dl><div><dt>RPC endpoint</dt><dd><code>{selected.rpcUrlRef ?? selected.rpcUrl ?? "Protected direct URL"}</code></dd></div><div><dt>Chain ID</dt><dd>{selected.chainId}</dd></div><div><dt>Start block</dt><dd>{selected.startBlock.toLocaleString()}</dd></div><div><dt>Last error</dt><dd class={selectedProgress?.lastError ? "connection-error" : undefined}>{selectedProgress?.lastError ?? selectedRuntime?.lastError ?? "None"}{selectedProgress?.lastErrorAt && <small>{new Date(selectedProgress.lastErrorAt).toLocaleString()}</small>}</dd></div></dl></section><section><h3>Configured resources</h3><div class="listener-resource-links"><button onClick={() => setTab("contracts")}><span>◇</span><div><strong>{selected.contracts.length} contracts</strong><small>{selected.contracts.reduce((sum, contract) => sum + contract.events.length, 0)} selected events</small></div><i>›</i></button><button onClick={() => setTab("webhooks")}><span>→</span><div><strong>{selected.webhooks.length} listener webhooks</strong><small>{snapshot.globalWebhooks.length} global routes</small></div><i>›</i></button></div></section></div>}
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
          <ListenerActions session={session} revision={snapshot.revision} listener={selected} onChanged={onChanged} />
        </div>}
      </article> : <div class="panel empty"><h3>No RPC listeners configured</h3><p>Add a listener to begin scanning EVM events.</p></div>}
    </div>
  </section>;
}

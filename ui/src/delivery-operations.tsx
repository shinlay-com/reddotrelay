import { useEffect, useRef, useState } from "preact/hooks";
import type { RuntimeListener, Session, RPCListener } from "./app";
import { action, read } from "./api";
import { displayValue, middleEllipsis } from "./display";

type DeliveryCounts = { pending: number; delivered: number; dead: number };
type EventEntry = { id: string; chainId: number; transactionHash: string; logIndex: number; blockNumber: number; contractAddress: string; eventName: string; eventSignature: string; decodedData: Record<string, unknown>; observedAt: string; deliveries: DeliveryCounts };
type DeliveryEntry = { id: string; destination: string; status: "pending" | "delivered" | "dead"; attempts: number; totalAttempts: number; nextAttempt: string; lastAttemptAt?: string; lastStatusCode?: number; failureSummary?: string; deliveredAt?: string; authentication?: string; keyId?: string };
type EventPage = { entries: EventEntry[]; nextBefore?: string; deliverySummary: DeliveryCounts };
type DeliveryPage = { entries: DeliveryEntry[] };
type ProgressState = "synced" | "lagging" | "paused" | "idle" | "failed";
type ScannerProgress = { rpcListenerId: string; chainId: number; latestBlock: number; confirmedHead: number; checkpoint: number; lagBlocks: number; updatedAt: string; lastError?: string; lastErrorAt?: string; name?: string; state?: ProgressState; available?: boolean; detail?: string };
type EventFilters = { chainId: string; transactionHash: string; blockNumber: string; contractAddress: string; eventSignature: string; deliveryStatus: string };

const emptyFilters: EventFilters = { chainId: "", transactionHash: "", blockNumber: "", contractAddress: "", eventSignature: "", deliveryStatus: "" };

function localTime(value?: string) { if (!value) return "—"; const date = new Date(value); return Number.isNaN(date.valueOf()) ? value : date.toLocaleString(); }

export function DeliveryOperations({ session, listeners, runtimeListeners }: { session: Session; listeners: RPCListener[]; runtimeListeners: RuntimeListener[] }) {
  const [events, setEvents] = useState<EventEntry[]>([]); const [nextBefore, setNextBefore] = useState<string>(); const [eventCursor, setEventCursor] = useState<string>(); const [eventHistory, setEventHistory] = useState<Array<string | undefined>>([]); const [expanded, setExpanded] = useState<string>();
  const [deliveries, setDeliveries] = useState<DeliveryEntry[]>([]);
  const [pageSize, setPageSize] = useState(50);
  const [progress, setProgress] = useState<ScannerProgress[]>([]);
  const [filters, setFilters] = useState<EventFilters>(emptyFilters);
  const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  const eventRequest = useRef(0);
  const skipNextTextFilterReload = useRef(false);
  const listenerByID = new Map(listeners.map((listener) => [listener.id, listener]));
  const runtimeByID = new Map(runtimeListeners.map((runtime) => [runtime.id, runtime]));
  const progressByID = new Map(progress.map((item) => [item.rpcListenerId, item]));
  const displayedProgress = listeners.map((listener) => {
    const item = progressByID.get(listener.id);
    const runtime = runtimeByID.get(listener.id);
    if (!item) return {
      rpcListenerId: listener.id, chainId: listener.chainId, latestBlock: 0, confirmedHead: 0, checkpoint: 0, lagBlocks: 0, updatedAt: "", name: listener.name,
      state: (listener.paused ? "paused" : runtime?.state === "idle" ? "idle" : "failed") as ProgressState, available: false,
      detail: runtime?.state === "idle" ? "No event subscriptions configured" : undefined
    };
    return {
    ...item,
      name: listenerByID.get(item.rpcListenerId)?.name ?? "RPC listener",
      state: (listener.paused ? "paused" : runtime?.state === "idle" ? "idle" : item.lastError ? "failed" : item.lagBlocks > 0 ? "lagging" : runtime?.state === "running" ? "synced" : "failed") as ProgressState,
      available: true,
      detail: runtime?.state === "idle" ? "No event subscriptions configured" : item.lastError
    };
  });

  async function loadEvents(targetCursor?: string, requestedFilters = filters, requestedPageSize = pageSize) {
    const request = ++eventRequest.current;
    setBusy(true); setError("");
    try {
      const query = new URLSearchParams({ limit: String(requestedPageSize) });
      for (const [key, value] of Object.entries(requestedFilters)) if (value.trim()) query.set(key, value.trim());
      if (targetCursor) query.set("before", targetCursor);
      const page = await read<EventPage>(`/api/v1/events?${query}`);
      if (request === eventRequest.current) { setEvents(page.entries); setNextBefore(page.nextBefore); }
    } catch (caught) { if (request === eventRequest.current) setError(caught instanceof Error ? caught.message : "Event history is unavailable."); } finally { if (request === eventRequest.current) setBusy(false); }
  }
  async function loadProgress() { try { const response = await fetch("/api/v1/scanner-progress", { headers: { Accept: "application/json" } }); if (response.ok) setProgress((await response.json() as { rpcListeners?: ScannerProgress[] }).rpcListeners ?? []); } catch { /* event history remains usable */ } }
  async function loadDeliveries(eventID: string) {
    setBusy(true); setError("");
    try {
      const page = await read<DeliveryPage>(`/api/v1/events/${eventID}/deliveries?limit=200`); setExpanded(eventID); setDeliveries(page.entries);
    } catch (caught) { setError(caught instanceof Error ? caught.message : "Delivery history is unavailable."); } finally { setBusy(false); }
  }
  async function requeue(delivery: DeliveryEntry) {
    if (!window.confirm(`Requeue dead delivery ${delivery.id}? RedDotRelay may deliver the event again.`)) return;
    setBusy(true); setError("");
    try { await action(session, `/api/v1/deliveries/${delivery.id}/requeue`, { confirm: true }); if (expanded) await loadDeliveries(expanded); await loadEvents(eventCursor); }
    catch (caught) { setError(caught instanceof Error ? caught.message : "Delivery could not be requeued."); } finally { setBusy(false); }
  }
  useEffect(() => {
    void loadProgress();
    const timer = window.setInterval(() => void loadProgress(), 5_000);
    return () => window.clearInterval(timer);
  }, []);
  useEffect(() => {
    if (skipNextTextFilterReload.current) { skipNextTextFilterReload.current = false; return; }
    const timer = window.setTimeout(() => { setEventCursor(undefined); setEventHistory([]); setExpanded(undefined); void loadEvents(); }, 300);
    return () => window.clearTimeout(timer);
  }, [filters.transactionHash, filters.blockNumber, filters.contractAddress, filters.eventSignature]);
  function nextEventPage() { if (!nextBefore) return; setEventHistory([...eventHistory, eventCursor]); setEventCursor(nextBefore); setExpanded(undefined); void loadEvents(nextBefore); }
  function previousEventPage() { if (eventHistory.length === 0) return; const previous = eventHistory[eventHistory.length - 1]; setEventHistory(eventHistory.slice(0, -1)); setEventCursor(previous); setExpanded(undefined); void loadEvents(previous); }
  function openDeliveries(eventID: string) { void loadDeliveries(eventID); }
  function toggleDeliveries(eventID: string) { expanded === eventID ? setExpanded(undefined) : openDeliveries(eventID); }
  function resetEventPage() { setEventCursor(undefined); setEventHistory([]); setExpanded(undefined); }
  function applyFilters(nextFilters: EventFilters) {
    skipNextTextFilterReload.current = nextFilters.transactionHash !== filters.transactionHash || nextFilters.blockNumber !== filters.blockNumber || nextFilters.contractAddress !== filters.contractAddress || nextFilters.eventSignature !== filters.eventSignature;
    setFilters(nextFilters); resetEventPage(); void loadEvents(undefined, nextFilters);
  }
  function update(name: keyof EventFilters, value: string) {
    const nextFilters = { ...filters, [name]: value };
    if (name === "deliveryStatus") { applyFilters(nextFilters); return; }
    setFilters(nextFilters); resetEventPage();
  }
  function toggleChain(chainID: number) { applyFilters({ ...filters, chainId: filters.chainId === String(chainID) ? "" : String(chainID) }); }
  function clearFilters() { applyFilters(emptyFilters); }
  function updatePageSize(size: number) { setPageSize(size); resetEventPage(); void loadEvents(undefined, filters, size); }
  const hasFilters = Object.values(filters).some((value) => value.trim());
  async function refresh() { await Promise.all([loadEvents(eventCursor), loadProgress(), expanded ? loadDeliveries(expanded) : Promise.resolve()]); }
  return <section class="delivery-operations"><div class="section-title"><div><p class="eyebrow">Durable operations</p><h1>Events and deliveries</h1></div><button class="button button--secondary" type="button" disabled={busy} onClick={() => void refresh()}>{busy ? "Refreshing…" : "Refresh"}</button></div>
    {displayedProgress.length > 0 && <div class="scanner-progress">{displayedProgress.map((item) => <button type="button" class={`panel scanner-progress__card scanner-progress--${item.state}${filters.chainId === String(item.chainId) ? " selected" : ""}`} key={item.rpcListenerId} onClick={() => toggleChain(item.chainId)} aria-pressed={filters.chainId === String(item.chainId)} title={`Filter events to chain ${item.chainId}`}><div><strong>{item.name}</strong><span>Chain {item.chainId}</span><span class={`progress-status progress-status--${item.state}`}>{item.state}</span>{item.detail && <span class={item.lastError ? "connection-error" : "scanner-status-detail"} title={item.detail}>{item.detail}</span>}</div><dl><div><dt>Latest</dt><dd>{item.available ? item.latestBlock.toLocaleString() : "—"}</dd></div><div><dt>Confirmed</dt><dd>{item.available ? item.confirmedHead.toLocaleString() : "—"}</dd></div><div><dt>Checkpoint</dt><dd>{item.available ? item.checkpoint.toLocaleString() : "—"}</dd></div><div><dt>Lag</dt><dd>{item.available ? `${item.lagBlocks.toLocaleString()} blocks` : "—"}</dd></div></dl></button>)}</div>}
    <form class="panel diagnostic-filters" onSubmit={(event) => event.preventDefault()}>
      <label>Transaction hash<span class="filter-control"><input value={filters.transactionHash} onInput={(event) => update("transactionHash", event.currentTarget.value)} placeholder="0x…" />{filters.transactionHash && <button type="button" class="filter-clear" aria-label="Clear transaction hash" title="Clear" onClick={() => update("transactionHash", "")}>×</button>}</span></label>
      <label>Block<span class="filter-control"><input type="number" min="0" value={filters.blockNumber} onInput={(event) => update("blockNumber", event.currentTarget.value)} />{filters.blockNumber && <button type="button" class="filter-clear" aria-label="Clear block" title="Clear" onClick={() => update("blockNumber", "")}>×</button>}</span></label>
      <label>Contract address<span class="filter-control"><input value={filters.contractAddress} onInput={(event) => update("contractAddress", event.currentTarget.value)} placeholder="0x…" />{filters.contractAddress && <button type="button" class="filter-clear" aria-label="Clear contract address" title="Clear" onClick={() => update("contractAddress", "")}>×</button>}</span></label>
      <label>Event signature<span class="filter-control"><input value={filters.eventSignature} onInput={(event) => update("eventSignature", event.currentTarget.value)} placeholder="Transfer(address,address,uint256)" />{filters.eventSignature && <button type="button" class="filter-clear" aria-label="Clear event signature" title="Clear" onClick={() => update("eventSignature", "")}>×</button>}</span></label>
      <label>Delivery state<span class="filter-control"><select value={filters.deliveryStatus} onChange={(event) => update("deliveryStatus", event.currentTarget.value)}><option value="">Any</option><option value="pending">Pending</option><option value="delivered">Delivered</option><option value="dead">Dead letter</option></select>{filters.deliveryStatus && <button type="button" class="filter-clear" aria-label="Clear delivery state" title="Clear" onClick={() => update("deliveryStatus", "")}>×</button>}</span></label>
      <div class="diagnostic-filter-actions"><button class="button button--secondary" type="button" disabled={busy || !hasFilters} onClick={clearFilters}>Clear all</button></div>
    </form>
    {error && <p class="alert" role="alert">{error}</p>}
    <div class="panel history-list">{events.length === 0 ? <div class="empty"><h3>No matching events</h3><p>Events appear after a configured scanner durably processes a selected log.</p></div> : events.map((entry) => <article class="history-event" key={entry.id}>
      <div class="history-event__summary" onClick={(event) => { if ((event.target as HTMLElement).closest("button") || !window.getSelection()?.isCollapsed) return; toggleDeliveries(entry.id); }}>
        <button type="button" class="history-event__toggle" onClick={() => toggleDeliveries(entry.id)} aria-expanded={expanded === entry.id} aria-label={`${expanded === entry.id ? "Hide" : "Show"} delivery details for ${entry.eventName}`} title={`${expanded === entry.id ? "Hide" : "Show"} delivery details`}>{expanded === entry.id ? "−" : "+"}</button><span class="history-event__identity"><strong>{entry.eventName}</strong><code class="overflow-text selectable" title={entry.eventSignature}>{middleEllipsis(entry.eventSignature)}</code></span><span class="event-position selectable">Chain {entry.chainId} <i aria-hidden="true">·</i> Block {entry.blockNumber.toLocaleString()} <i aria-hidden="true">·</i> Log {entry.logIndex}</span><span class="delivery-counts"><b>{entry.deliveries.pending} pending</b><b>{entry.deliveries.delivered} delivered</b><b class={entry.deliveries.dead ? "dead" : ""}>{entry.deliveries.dead} dead</b></span><time>{localTime(entry.observedAt)}</time>
      </div>
      {expanded === entry.id && <div class="delivery-details"><div class="decoded-event"><h3>Decoded event data</h3><dl>{Object.entries(entry.decodedData ?? {}).map(([name, value]) => { const fullValue = displayValue(value); return <div key={name}><dt>{name}</dt><dd><code class="overflow-text" title={fullValue}>{middleEllipsis(fullValue, 64)}</code></dd></div>; })}</dl></div><div class="event-identity"><span>Transaction <code class="overflow-text" title={entry.transactionHash}>{entry.transactionHash}</code></span><span>Contract <code class="overflow-text" title={entry.contractAddress}>{entry.contractAddress}</code></span></div>{deliveries.length === 0 ? <p class="muted">No delivery destinations persisted.</p> : deliveries.map((delivery) => <article class="delivery-row" key={delivery.id}>
        <div><span class={`badge badge--${delivery.status === "dead" ? "paused" : "active"}`}>{delivery.status}</span><code>{delivery.destination}</code>{delivery.authentication && <small>{delivery.authentication}{delivery.keyId ? ` · ${delivery.keyId}` : ""}</small>}</div>
        <dl><div><dt>Attempts</dt><dd>{delivery.attempts} current / {delivery.totalAttempts} lifetime</dd></div><div><dt>Last attempt</dt><dd>{localTime(delivery.lastAttemptAt)}</dd></div><div><dt>HTTP</dt><dd>{delivery.lastStatusCode ?? "—"}</dd></div><div><dt>Next attempt</dt><dd>{delivery.status === "pending" ? localTime(delivery.nextAttempt) : "—"}</dd></div></dl>
        {delivery.failureSummary && <p class="failure-summary">{delivery.failureSummary}</p>}{delivery.status === "dead" && <button class="button button--secondary" disabled={busy || session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void requeue(delivery)}>Requeue</button>}
      </article>)}</div>}
    </article>)}</div>
    <nav class="pagination" aria-label="Event pages"><label class="page-size-control">Rows per page<select value={pageSize} onChange={(event) => updatePageSize(Number(event.currentTarget.value))}>{[10, 25, 50, 100, 200].map((size) => <option key={size} value={size}>{size}</option>)}</select></label><button class="button button--secondary" disabled={busy || eventHistory.length === 0} onClick={previousEventPage}>Previous</button><span>Page {eventHistory.length + 1}</span><button class="button button--secondary" disabled={busy || !nextBefore} onClick={nextEventPage}>Next</button></nav>
  </section>;
}

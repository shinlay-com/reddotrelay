import type { DashboardData, DashboardSection, RPCListener } from "./app";
import { middleEllipsis } from "./display";
import { deriveEngineStatus } from "./dashboard-status";

type ProgressState = "synced" | "lagging" | "paused" | "failed";

function localTime(value?: string) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
}

function total(listeners: RPCListener[], select: (listener: RPCListener) => number) {
  return listeners.reduce((sum, listener) => sum + select(listener), 0);
}

export function Overview({ data, sessionName, refreshing, onRefresh }: { data: DashboardData; sessionName: string; refreshing: boolean; onRefresh: () => void }) {
  const { events, deliveryCounts, activity, progress } = data;
  const listeners = data.snapshot.rpcListeners;
  const runtimeByID = new Map(data.runtime.rpcListeners.map((runtime) => [runtime.id, runtime]));
  const progressByID = new Map(progress.map((item) => [item.rpcListenerId, item]));
  const stale = (section: DashboardSection) => data.stale.includes(section) ? <span class="stale-indicator" title="The latest refresh failed; showing last successful data.">Stale</span> : null;

  function listenerState(listener: RPCListener): ProgressState {
    if (listener.paused) return "paused";
    const scanner = progressByID.get(listener.id);
    if (!scanner || scanner.lastError) return "failed";
    if (scanner.lagBlocks > 0) return "lagging";
    return runtimeByID.get(listener.id)?.state === "running" ? "synced" : "failed";
  }

  const activeListeners = listeners.filter((listener) => !listener.paused).length;
  const contractCount = total(listeners, (listener) => listener.contracts.length);
  const selectedEvents = total(listeners, (listener) => listener.contracts.reduce((sum, contract) => sum + contract.events.length, 0));
  const deliveryTotal = deliveryCounts.pending + deliveryCounts.delivered + deliveryCounts.dead;
  const deliveryPercent = deliveryTotal === 0 ? 100 : Math.round(deliveryCounts.delivered / deliveryTotal * 100);
  const engineStatus = deriveEngineStatus({ healthOK: data.health === "ok", readinessOK: data.readiness === "ok", pendingDeliveries: deliveryCounts.pending, deadDeliveries: deliveryCounts.dead, scannerFailure: data.recent.scannerErrors > 0 || progress.some((item) => Boolean(item.lastError)), runtimeFailure: data.recent.deliveryFailures > 0 || data.runtime.rpcListeners.some((item) => item.state === "failed") });
  const detail = `Service health: ${data.health === "ok" ? "passing" : "failing"}\nRuntime readiness: ${data.readiness === "ok" ? "ready" : "not ready"}\nRPC listeners: ${activeListeners} active / ${listeners.length} configured\nDelivery backlog: ${deliveryCounts.pending} pending, ${deliveryCounts.dead} dead\nLast checked: ${localTime(data.lastCheckedAt)}`;

  return <section class="overview-view">
    <div class="overview-heading"><div><p class="eyebrow">Engine overview</p><h1>Good afternoon, {sessionName}</h1><p>Monitor blockchain events and webhook delivery from one place.</p></div><button class="button button--secondary" disabled={refreshing} onClick={onRefresh}>{refreshing ? "Refreshing…" : "Refresh"}</button></div>
    <div class="overview-metrics">
      <details class={`panel engine-summary engine-status--${engineStatus.toLowerCase()}`} title={detail}><summary><span>Engine status {stale("health")} {stale("readiness")} {stale("runtime")} {stale("summary")}</span><strong><i class="overview-status-dot" />{engineStatus}</strong><small>Last checked {localTime(data.lastCheckedAt)}</small></summary><div class="engine-summary-detail"><dl><div><dt>Service</dt><dd>{data.health === "ok" ? "Healthy" : "Unavailable"}</dd></div><div><dt>Runtime</dt><dd>{data.readiness === "ok" ? "Ready" : "Starting"}</dd></div><div><dt>RPC listeners</dt><dd>{activeListeners} active · {listeners.length - activeListeners} paused</dd></div><div><dt>Delivery backlog</dt><dd>{deliveryCounts.pending} pending/retrying</dd></div><div><dt>Last checked</dt><dd>{localTime(data.lastCheckedAt)}</dd></div></dl></div></details>
      <article class="panel"><span>Active RPC listeners {stale("configuration")} {stale("progress")}</span><strong>{activeListeners}</strong><small>{listeners.length - activeListeners} paused</small></article>
      <article class="panel"><span>Configured contracts</span><strong>{contractCount}</strong><small>{selectedEvents} selected events</small></article>
      <article class="panel"><span>Recent delivery success {stale("events")}</span><strong>{deliveryPercent}%</strong><small>{deliveryCounts.delivered} delivered · {deliveryCounts.dead} dead</small></article>
    </div>
    <div class="overview-grid">
      <div class="overview-main">
        <article class="panel overview-events"><header><div><h2>Recent events {stale("events")}</h2><p>Latest persisted decoded events</p></div></header><div class="overview-table-wrap"><table><thead><tr><th>Observed</th><th>Event</th><th>Contract</th><th>Block</th><th>Delivery</th></tr></thead><tbody>{events.length === 0 ? <tr><td colSpan={5}>No persisted events yet.</td></tr> : events.map((event) => <tr key={event.id}><td><strong>{localTime(event.observedAt)}</strong></td><td><strong>{event.eventName}</strong><small class="overflow-text" title={event.eventSignature}>{middleEllipsis(event.eventSignature)}</small></td><td><code class="overflow-text" title={event.contractAddress}>{event.contractAddress.slice(0, 8)}…{event.contractAddress.slice(-4)}</code></td><td>{event.blockNumber.toLocaleString()}</td><td><span class={`badge badge--${event.deliveries.dead ? "paused" : "active"}`}>{event.deliveries.dead ? "Dead" : event.deliveries.pending ? "Pending" : "Delivered"}</span></td></tr>)}</tbody></table></div></article>
        <article class="panel overview-activity"><header><h2>Operational activity {stale("activity")}</h2><p>Latest runtime events</p></header>{activity.length === 0 ? <p class="overview-empty">No runtime activity recorded.</p> : activity.map((event) => <div key={event.sequence}><i /><p><strong>{event.message}</strong><small>{event.component}</small></p><time>{localTime(event.timestamp)}</time></div>)}</article>
      </div>
      <aside class="overview-side">
        <article class="panel overview-health"><header><h2>Delivery health {stale("events")}</h2><p>All persisted deliveries</p></header><div><span class="overview-donut" style={{ "--delivery-percent": `${deliveryPercent * 3.6}deg` }}><b>{deliveryPercent}%</b><small>success</small></span><dl><div><dt>Delivered</dt><dd>{deliveryCounts.delivered}</dd></div><div><dt>Pending</dt><dd>{deliveryCounts.pending}</dd></div><div><dt>Dead</dt><dd>{deliveryCounts.dead}</dd></div></dl></div></article>
        <article class="panel overview-listeners"><header><h2>RPC listeners {stale("configuration")} {stale("progress")}</h2><p>Configured EVM endpoints</p></header>{listeners.length === 0 ? <p class="overview-empty">No RPC listeners configured.</p> : listeners.slice(0, 4).map((listener) => { const state = listenerState(listener); return <div class="overview-listener" key={listener.id}><span>{listener.name.slice(0, 1).toUpperCase()}</span><div><strong>{listener.name}</strong><small>Chain {listener.chainId} · {listener.contracts.length} contracts</small></div><b class={`progress-status progress-status--${state}`}>{state}</b></div>; })}</article>
      </aside>
    </div>
  </section>;
}

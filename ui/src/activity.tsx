import { useEffect, useState } from "preact/hooks";
import type { Session } from "./app";

type OperationalEvent = { sequence: number; timestamp: string; level: "info" | "warn" | "error"; component: "server" | "scanner" | "delivery"; message: string; attributes: Record<string, unknown> };
type AuditEvent = { id: string; actorName: string; actorRole: string; action: string; resourceKind: string; resourceId: string; previousRevision: number; newRevision: number; createdAt: string };
type RequeueAudit = { id: string; actorName: string; actorRole: string; deliveryId: string; eventId: string; previousAttempts: number; createdAt: string };
type Page<T> = { entries: T[]; nextBefore?: string };
type Feed = "runtime" | "audit" | "requeue";

function displayTime(value: string) {
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
}

export function ActivityPanel({ session }: { session: Session }) {
  const [feed, setFeed] = useState<Feed>("runtime");
  const [events, setEvents] = useState<OperationalEvent[]>([]);
  const [audits, setAudits] = useState<AuditEvent[]>([]);
  const [requeues, setRequeues] = useState<RequeueAudit[]>([]);
  const [nextBefore, setNextBefore] = useState<string>();
  const [cursor, setCursor] = useState<string>();
  const [cursorHistory, setCursorHistory] = useState<Array<string | undefined>>([]);
  const [pageSize, setPageSize] = useState(50);
  const [level, setLevel] = useState("");
  const [component, setComponent] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function load(targetCursor?: string) {
    setBusy(true); setError("");
    try {
      const parameters = new URLSearchParams({ limit: String(pageSize) });
      if (targetCursor) parameters.set("before", targetCursor);
      if (feed === "runtime" && level) parameters.set("level", level);
      if (feed === "runtime" && component) parameters.set("component", component);
      const path = feed === "runtime" ? "/api/v1/operational-events" : feed === "audit" ? "/api/v1/rpc-listener-audit" : "/api/v1/delivery-requeue-audit";
      const response = await fetch(`${path}?${parameters}`, { headers: { Accept: "application/json" } });
      if (!response.ok) throw new Error(`Activity request failed (${response.status}).`);
      if (feed === "runtime") {
        const page = await response.json() as Page<OperationalEvent>;
        setEvents(page.entries); setNextBefore(page.nextBefore);
      } else if (feed === "audit") {
        const page = await response.json() as Page<AuditEvent>;
        setAudits(page.entries); setNextBefore(page.nextBefore);
      } else {
        const page = await response.json() as Page<RequeueAudit>;
        setRequeues(page.entries); setNextBefore(page.nextBefore);
      }
    } catch (caught) { setError(caught instanceof Error ? caught.message : "Activity could not be loaded."); } finally { setBusy(false); }
  }

  useEffect(() => { setNextBefore(undefined); setCursor(undefined); setCursorHistory([]); void load(); }, [feed, level, component, pageSize]);

  function choose(next: Feed) { setFeed(next); setError(""); setNextBefore(undefined); }
  function nextPage() { if (!nextBefore) return; setCursorHistory([...cursorHistory, cursor]); setCursor(nextBefore); void load(nextBefore); }
  function previousPage() { if (cursorHistory.length === 0) return; const previous = cursorHistory[cursorHistory.length - 1]; setCursorHistory(cursorHistory.slice(0, -1)); setCursor(previous); void load(previous); }
  const current = feed === "runtime" ? events : feed === "audit" ? audits : requeues;
  return <section class="activity">
    <div class="section-title"><div><p class="eyebrow">Observability</p><h1>Activity</h1></div><button class="button button--secondary" disabled={busy} onClick={() => void load(cursor)}>{busy ? "Loading…" : "Refresh"}</button></div>
    <div class="activity-tabs" role="tablist"><button role="tab" aria-selected={feed === "runtime"} onClick={() => choose("runtime")}>Runtime events</button><button role="tab" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} aria-selected={feed === "audit"} onClick={() => choose("audit")}>Configuration audit</button><button role="tab" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} aria-selected={feed === "requeue"} onClick={() => choose("requeue")}>Requeue audit</button></div>
    {feed === "runtime" && <div class="filters"><label>Level<select value={level} onChange={(event) => setLevel(event.currentTarget.value)}><option value="">All levels</option><option value="info">Info</option><option value="warn">Warning</option><option value="error">Error</option></select></label><label>Component<select value={component} onChange={(event) => setComponent(event.currentTarget.value)}><option value="">All components</option><option value="server">Server</option><option value="scanner">Scanner</option><option value="delivery">Delivery</option></select></label></div>}
    {error && <p class="alert" role="alert">{error}</p>}
    <div class="panel activity-list" role="tabpanel">
      {!busy && current.length === 0 ? <div class="empty"><h3>No activity found</h3><p>{feed === "runtime" ? "Events are held in memory and reset when this instance restarts." : "No durable audit entries match this view."}</p></div> : feed === "runtime" ? events.map((event) => <article class="event" key={event.sequence}><time>{displayTime(event.timestamp)}</time><span class={`event__level event__level--${event.level}`}>{event.level}</span><span class="event__component">{event.component}</span><div><strong>{event.message}</strong>{Object.keys(event.attributes).length > 0 && <p>{Object.entries(event.attributes).map(([key, value]) => `${key}=${String(value)}`).join(" · ")}</p>}</div></article>) : feed === "audit" ? audits.map((event) => <article class="event" key={event.id}><time>{displayTime(event.createdAt)}</time><span class="event__level event__level--info">audit</span><span class="event__component">{event.resourceKind}</span><div><strong>{event.actorName} {event.action} {event.resourceKind}</strong><p>revision {event.previousRevision} → {event.newRevision} · {event.resourceId}</p></div></article>) : requeues.map((event) => <article class="event" key={event.id}><time>{displayTime(event.createdAt)}</time><span class="event__level event__level--warn">requeue</span><span class="event__component">delivery</span><div><strong>{event.actorName} requeued a dead delivery</strong><p>{event.deliveryId} · event {event.eventId} · previous attempts {event.previousAttempts}</p></div></article>)}
    </div>
    <nav class="pagination" aria-label="Activity pages"><label class="page-size-control">Rows per page<select value={pageSize} onChange={(event) => setPageSize(Number(event.currentTarget.value))}>{[10, 25, 50, 100, 200].map((size) => <option key={size} value={size}>{size}</option>)}</select></label><button class="button button--secondary" disabled={busy || cursorHistory.length === 0} onClick={previousPage}>Previous</button><span>Page {cursorHistory.length + 1}</span><button class="button button--secondary" disabled={busy || !nextBefore} onClick={nextPage}>Next</button></nav>
  </section>;
}

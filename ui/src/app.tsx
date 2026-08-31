import { render } from "preact";
import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import { DeliveryOperations } from "./delivery-operations";
import { ActivityPanel } from "./activity";
import { Overview } from "./overview";
import { ListenersPage } from "./listeners-page";
import { StoragePage } from "./storage-page";
import { UsersPage } from "./users-page";
import { APIKeysPage } from "./api-keys-page";
import { deriveEngineStatus } from "./dashboard-status";
import "./styles.css";

type ServiceState = "checking" | "ok" | "unavailable";
export type Session = { name: string; role: "admin" | "read-only"; csrfToken: string; expiresAt: string };
export type WebhookConfig = { id: string; url?: string; urlRef?: string; authentication?: { type: string; secretRef: string; keyId?: string } };
export type EventConfig = { id: string; selector: string; webhooks: WebhookConfig[]; effectiveWebhooks: WebhookConfig[]; webhookSource: string };
export type ContractConfig = { id: string; address: string; abi: unknown; webhooks: WebhookConfig[]; events: EventConfig[] };
export type RPCListener = {
  id: string;
  name: string;
  paused: boolean;
  chainId: number;
  rpcUrl?: string;
  rpcUrlRef?: string;
	 rpcAuthentication?: { type: string; username?: string; headerName?: string; secretConfigured: boolean; tokenUrl?: string; tokenApiKeyConfigured?: boolean };
  startBlock: number;
  batchSize: number;
  confirmations: number;
  pollInterval: string;
  reorgDepth: number;
  rpcRetryAttempts: number;
  rpcRetryBackoff: string;
  rpcTimeout: string;
  tls: { caPem?: string; serverName?: string; insecureSkipVerify: boolean };
  contracts: ContractConfig[];
  webhooks: WebhookConfig[];
};
export type ConfigSnapshot = { revision: number; updatedAt: string; globalWebhooks: WebhookConfig[]; rpcListeners: RPCListener[] };
export type RuntimeListener = { id: string; chainId: number; state: string; attempts: number; nextRetryAt?: string; lastError?: string };
type RuntimeStatus = {
  desiredRevision: number;
  initialReconcileComplete: boolean;
  lastReconciledAt?: string;
  ready: boolean;
  rpcListeners: RuntimeListener[];
};
export type DeliveryCounts = { pending: number; delivered: number; dead: number };
export type RecentEvent = { id: string; blockNumber: number; eventName: string; eventSignature: string; contractAddress: string; observedAt: string; deliveries: DeliveryCounts };
export type OperationalEvent = { sequence: number; timestamp: string; component: string; message: string };
export type ScannerProgress = { rpcListenerId: string; lagBlocks: number; lastError?: string };
export type OperationalSummary = { deliveries: { pendingRetrying: number; delivered: number; dead: number }; counters: { eventsProcessedTotal: number; scannerErrorsTotal: number; deliveryFailuresTotal: number } };
export type DashboardSection = "health" | "readiness" | "configuration" | "runtime" | "events" | "activity" | "progress" | "summary";
export type DashboardData = {
  health: ServiceState;
  readiness: ServiceState;
  snapshot: ConfigSnapshot;
  runtime: RuntimeStatus;
  events: RecentEvent[];
  deliveryCounts: DeliveryCounts;
  activity: OperationalEvent[];
  progress: ScannerProgress[];
  summary: OperationalSummary;
  recent: { eventsProcessed: number; scannerErrors: number; deliveryFailures: number };
  stale: DashboardSection[];
  lastCheckedAt: string;
};

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, { signal, headers: { Accept: "application/json" } });
  if (response.status === 401) throw new Error("session-expired");
  if (!response.ok) throw new Error(`request-failed-${response.status}`);
  return response.json() as Promise<T>;
}

function formatTime(value?: string) {
  if (!value) return "Not yet";
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? value : date.toLocaleString();
}

function App() {
  const [health, setHealth] = useState<ServiceState>("checking");
  const [session, setSession] = useState<Session | null | undefined>(undefined);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [setupRequired, setSetupRequired] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [dashboard, setDashboard] = useState<DashboardData | null>(null);
  const [dashboardError, setDashboardError] = useState("");
  const [refreshing, setRefreshing] = useState(false);
  const refreshInFlight = useRef(false);

  const loadDashboard = useCallback(async (signal?: AbortSignal) => {
    if (refreshInFlight.current) return;
    refreshInFlight.current = true;
    setRefreshing(true);
    setDashboardError("");
    try {
      const results = await Promise.allSettled([
        fetch("/healthz", { signal, headers: { Accept: "application/json" } }),
        fetch("/readyz", { signal, headers: { Accept: "application/json" } }),
        getJSON<ConfigSnapshot>("/api/v1/rpc-listeners", signal),
        getJSON<RuntimeStatus>("/api/v1/rpc-listener-status", signal),
        getJSON<{ entries?: RecentEvent[]; deliverySummary?: DeliveryCounts }>("/api/v1/events?limit=5", signal),
        getJSON<{ entries?: OperationalEvent[] }>("/api/v1/operational-events?limit=3", signal),
        getJSON<{ rpcListeners?: ScannerProgress[] }>("/api/v1/scanner-progress", signal),
        getJSON<OperationalSummary>("/api/v1/operational-summary", signal)
      ]);
      if (signal?.aborted) return;
      const sessionExpired = results.some((result) => result.status === "rejected" && result.reason instanceof Error && result.reason.message === "session-expired");
      if (sessionExpired) {
        setSession(null);
        setDashboard(null);
        setError("Your session expired. Sign in again.");
        return;
      }
      const sectionNames: DashboardSection[] = ["health", "readiness", "configuration", "runtime", "events", "activity", "progress", "summary"];
      const stale = sectionNames.filter((_, index) => results[index].status === "rejected");
      setDashboard((previous) => {
        const healthResult = results[0];
        const readinessResult = results[1];
        const configResult = results[2];
        const runtimeResult = results[3];
        const eventsResult = results[4];
        const activityResult = results[5];
        const progressResult = results[6];
        const summaryResult = results[7];
        if (!previous && (configResult.status === "rejected" || runtimeResult.status === "rejected" || summaryResult.status === "rejected")) return null;
        const eventPage = eventsResult.status === "fulfilled" ? eventsResult.value : undefined;
        const summary = summaryResult.status === "fulfilled" ? summaryResult.value : previous!.summary;
        const previousCounters = previous?.summary.counters ?? summary.counters;
        const next: DashboardData = {
          health: healthResult.status === "fulfilled" ? (healthResult.value.ok ? "ok" : "unavailable") : "unavailable",
          readiness: readinessResult.status === "fulfilled" ? (readinessResult.value.ok ? "ok" : "unavailable") : previous?.readiness ?? "unavailable",
          snapshot: configResult.status === "fulfilled" ? configResult.value : previous!.snapshot,
          runtime: runtimeResult.status === "fulfilled" ? runtimeResult.value : previous!.runtime,
          events: eventPage?.entries ?? previous?.events ?? [],
          deliveryCounts: { pending: summary.deliveries.pendingRetrying, delivered: summary.deliveries.delivered, dead: summary.deliveries.dead },
          activity: activityResult.status === "fulfilled" ? activityResult.value.entries ?? [] : previous?.activity ?? [],
          progress: progressResult.status === "fulfilled" ? progressResult.value.rpcListeners ?? [] : previous?.progress ?? [],
          summary,
          recent: {
            eventsProcessed: Math.max(0, summary.counters.eventsProcessedTotal - previousCounters.eventsProcessedTotal),
            scannerErrors: Math.max(0, summary.counters.scannerErrorsTotal - previousCounters.scannerErrorsTotal),
            deliveryFailures: Math.max(0, summary.counters.deliveryFailuresTotal - previousCounters.deliveryFailuresTotal)
          },
          stale,
          lastCheckedAt: new Date().toISOString()
        };
        setHealth(next.health);
        return next;
      });
      if (stale.length > 0) setDashboardError(`Some dashboard data is stale: ${stale.join(", ")}.`);
    } finally {
      refreshInFlight.current = false;
      if (!signal?.aborted) setRefreshing(false);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    fetch("/healthz", { signal: controller.signal, headers: { Accept: "application/json" } })
      .then((response) => setHealth(response.ok ? "ok" : "unavailable"))
      .catch(() => setHealth("unavailable"));
    getJSON<Session>("/api/v1/ui-session", controller.signal)
      .then((value) => setSession(value))
      .catch(() => setSession(null));
    getJSON<{required:boolean}>("/api/v1/ui-setup", controller.signal).then(value=>setSetupRequired(value.required)).catch(()=>undefined);
    return () => controller.abort();
  }, []);

  async function login() {
    setBusy(true);
    setError("");
    try {
      const response = await fetch("/api/v1/ui-session", {
        method: "POST",
        headers: { Accept: "application/json", "Content-Type": "application/json" },
        body: JSON.stringify({ username, password })
      });
      if (!response.ok) {
        setError(response.status === 401 ? "The username or password is invalid." : response.status === 429 ? "Too many failed attempts. Try again later." : "Sign-in failed. Try again.");
        return;
      }
      setSession(await response.json() as Session);
    } catch {
      setError("RedDotRelay is unavailable.");
    } finally {
      setBusy(false);
    }
  }

  async function logout() {
    if (!session) return;
    setBusy(true);
    try {
      await fetch("/api/v1/ui-session", { method: "DELETE", headers: { "X-CSRF-Token": session.csrfToken } });
    } finally {
      setSession(null);
      setDashboard(null);
      setBusy(false);
    }
  }

  async function setup() {
    setBusy(true); setError("");
    try {
      const response=await fetch("/api/v1/ui-setup",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({username,password})});
      if(!response.ok){const result=await response.json().catch(()=>({error:"Setup failed."}));setError(result.error??"Setup failed.");return}
      setSetupRequired(false); await login();
    } catch { setError("RedDotRelay is unavailable."); } finally { setBusy(false); }
  }

  if (session) {
    return <Dashboard session={session} data={dashboard} error={dashboardError} refreshing={refreshing} onRefresh={() => void loadDashboard()} onLogout={() => void logout()} busy={busy} />;
  }

  return (
    <main class="shell shell--centered">
      <section class="hero" aria-labelledby="title">
        <Brand />
        <p class="summary">Configure EVM RPC listeners and review delivery operations from one secure console.</p>
        <StatusPill label="Service" state={health} />
        {session === undefined ? <p class="notice" role="status">Checking your session…</p> : (
          <form class="login" onSubmit={(event) => { event.preventDefault(); void (setupRequired ? setup() : login()); }}>
            <div><label for="username">{setupRequired ? "Create the first administrator" : "Username"}</label><p class="hint">{setupRequired ? "This one-time setup creates the local Engine administrator." : "Sign in with your local Engine account."}</p></div>
            <input id="username" name="username" value={username} onInput={(event) => setUsername(event.currentTarget.value)} placeholder="admin" autocomplete="username" required autofocus />
            <input id="password" name="password" type="password" value={password} onInput={(event) => setPassword(event.currentTarget.value)} placeholder={setupRequired ? "At least 12 characters" : "Password"} autocomplete={setupRequired ? "new-password" : "current-password"} required />
            {error && <p class="error" role="alert">{error}</p>}
            <button class="button" type="submit" disabled={busy || username.trim() === "" || password === ""}>{busy ? "Please wait…" : setupRequired ? "Create administrator" : "Sign in"}</button>
          </form>
        )}
      </section>
    </main>
  );
}

function Brand() {
  return <header class="brand"><div class="mark" aria-hidden="true">RDR</div><div><p class="eyebrow">Operations console</p><h1 id="title">RedDotRelay</h1></div></header>;
}

function StatusPill({ label, state }: { label: string; state: ServiceState }) {
  return <span class={`health health--${state}`}><span class="health__dot" />{label} {state === "checking" ? "checking" : state}</span>;
}

function Dashboard({ session, data, error, refreshing, onRefresh, onLogout, busy }: {
  session: Session; data: DashboardData | null; error: string; refreshing: boolean; onRefresh: () => void; onLogout: () => void; busy: boolean;
}) {
  const [navigationCollapsed, setNavigationCollapsed] = useState(false);
  const [activeSection, setActiveSection] = useState("overview");
  const [buildInfo, setBuildInfo] = useState<{version:string;commit:string;buildDate:string;environmentName?:string}>();
  useEffect(()=>{void fetch('/api/v1/build-info',{headers:{Accept:'application/json'}}).then(async response=>{if(response.ok)setBuildInfo(await response.json())}).catch(()=>undefined)},[]);
  const runtimeListeners = data?.runtime.rpcListeners ?? [];
  const listeners = data?.snapshot.rpcListeners ?? [];
  const listenerCount = listeners.length;
  const contractCount = listeners.reduce((total, item) => total + (item.contracts ?? []).length, 0);
  const eventCount = listeners.reduce((total, item) => total + (item.contracts ?? []).reduce((sum, contract) => sum + (contract.events ?? []).length, 0), 0);
  const navigation = [
    { id: "overview", icon: "⌂", label: "Overview" },
    { id: "deliveries", icon: "⇢", label: "Events & deliveries" },
    { id: "listeners", icon: "⌁", label: "RPC listeners", count: listenerCount },
    { id: "activity", icon: "◷", label: "Activity" }
    ,{ id: "storage", icon: "▣", label: "Storage & retention" }
    ,{ id: "users", icon: "◎", label: "Users" }
    ,{ id: "api-keys", icon: "⌘", label: "API keys" }
  ];
  function selectSection(id: string) {
    setActiveSection(id);
    window.scrollTo({ top: 0, behavior: "smooth" });
  }

  const configurationView = activeSection === "overview" || activeSection === "listeners";
  useEffect(() => { if (configurationView && !data && !refreshing && !error) onRefresh(); }, [configurationView, data, refreshing, error, onRefresh]);
  useEffect(() => {
    if (activeSection !== "overview") return;
    const interval = window.setInterval(onRefresh, 30_000);
    return () => window.clearInterval(interval);
  }, [activeSection, onRefresh]);
  const status = !data ? "Unavailable" : deriveEngineStatus({ healthOK: data.health === "ok", readinessOK: data.readiness === "ok", pendingDeliveries: data.deliveryCounts.pending, deadDeliveries: data.deliveryCounts.dead, scannerFailure: data.recent.scannerErrors > 0 || data.progress.some((item) => Boolean(item.lastError)), runtimeFailure: data.recent.deliveryFailures > 0 || data.runtime.rpcListeners.some((item) => item.state === "failed") });
  return (
    <div class={`application-frame${navigationCollapsed ? " navigation-collapsed" : ""}`}>
      <header class="application-topbar">
        <div class="application-brand-group">
          <button class="navigation-toggle" type="button" aria-label={navigationCollapsed ? "Expand sidebar" : "Collapse sidebar"} aria-expanded={!navigationCollapsed} title={navigationCollapsed ? "Expand sidebar" : "Collapse sidebar"} onClick={() => setNavigationCollapsed(!navigationCollapsed)}>≡</button>
          <a class="application-brand" href="#overview" onClick={(event) => { event.preventDefault(); selectSection("overview"); }}><span>R</span><strong>RedDotRelay</strong></a>
        </div>
        <div class="engine-identity"><span class="engine-identity__dot" /><span><small>Connected environment</small>{buildInfo?.environmentName ?? "Local Engine"}</span></div>
        {buildInfo&&<span class="engine-version" tabindex={0} title={`Version ${buildInfo.version}\nCommit ${buildInfo.commit}\nBuilt ${buildInfo.buildDate}`}>{buildInfo.version}</span>}
        <div class="application-breadcrumb"><span>›</span>{navigation.find((item) => item.id === activeSection)?.label ?? "Overview"}</div>
        <div class="account"><div><span class="muted">Signed in as</span><strong>{session.name}</strong><span class="role">{session.role}</span></div><button class="button button--secondary" disabled={busy} onClick={onLogout}>Sign out</button></div>
      </header>
      <aside class="application-sidebar">
        <nav aria-label="Main navigation">{navigation.map((item) => <a key={item.id} href={`#${item.id}`} class={`application-nav-item${activeSection === item.id ? " active" : ""}`} aria-current={activeSection === item.id ? "page" : undefined} title={navigationCollapsed ? item.label : undefined} onClick={(event) => { event.preventDefault(); selectSection(item.id); }}><span aria-hidden="true">{item.icon}</span><strong>{item.label}</strong>{item.count !== undefined && <b>{item.count}</b>}</a>)}</nav>
        <details class={`application-engine-status engine-status--${status.toLowerCase()}`}>
          <summary title="Show Engine status details"><span class="engine-identity__dot" /><div><strong>Engine status: {status}</strong><small>{data ? `Checked ${formatTime(data.lastCheckedAt)}` : "Waiting for dashboard data"}</small></div></summary>
          {data && <div class="engine-status-detail">
            <dl>
              <div><dt>Service</dt><dd>{data.health === "ok" ? "Healthy" : "Unavailable"}</dd></div>
              <div><dt>Runtime</dt><dd>{data.readiness === "ok" ? "Ready" : "Starting"}</dd></div>
              <div><dt>RPC listeners</dt><dd>{data.snapshot.rpcListeners.filter((item) => !item.paused).length} active · {data.snapshot.rpcListeners.filter((item) => item.paused).length} paused</dd></div>
              <div><dt>Delivery backlog</dt><dd>{data.deliveryCounts.pending} pending/retrying</dd></div>
              <div><dt>Last checked</dt><dd>{formatTime(data.lastCheckedAt)}</dd></div>
            </dl>
            <small>{data.recent.eventsProcessed} events processed · {data.recent.deliveryFailures} delivery failures · {data.recent.scannerErrors} scanner errors since the previous check</small>
          </div>}
        </details>
      </aside>
      <main class="console">
      {error && <p class="alert" role="alert">{error}</p>}
      {configurationView && !data ? <section class="panel loading" role="status">{refreshing ? "Loading view…" : "View data is unavailable."}</section> : activeSection === "overview" && data ? <Overview data={data} sessionName={session.name} onRefresh={onRefresh} refreshing={refreshing} /> : activeSection === "listeners" && data ? <ListenersPage session={session} snapshot={data.snapshot} runtimeListeners={runtimeListeners} onChanged={onRefresh} /> : activeSection === "deliveries" ? <DeliveryOperations session={session} listeners={listeners} runtimeListeners={runtimeListeners} /> : activeSection === "activity" ? <ActivityPanel session={session} /> : activeSection === "storage" ? <StoragePage session={session}/> : activeSection === "users" ? <UsersPage session={session}/> : activeSection === "api-keys" ? <APIKeysPage session={session}/> : null}
      </main>
    </div>
  );
}

const root = document.getElementById("app");
if (!root) throw new Error("UI root element is missing");
render(<App />, root);

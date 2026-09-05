import { useEffect, useState } from "preact/hooks";
import type { RPCListener, Session } from "./app";
import { mutate, read } from "./api";

type Preview = { token: string; configurationRevision: number; previousBlock: number | null; fromBlock: number; toBlock: number; blocks: number; confirmation: string; expiresAt: string };
type Audit = { sequence: number; fromBlock: number; toBlock: number; actorName: string; createdAt: string };
type Page = { entries: Audit[]; nextBefore?: number };

export function ScannerSkip({ session, listener, revision, runtimeState, onChanged }: { session: Session; listener: RPCListener; revision: number; runtimeState?: string; onChanged: () => void }) {
  const [preview, setPreview] = useState<Preview>();
  const [confirmation, setConfirmation] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [success, setSuccess] = useState("");
  const [audit, setAudit] = useState<Page>();
  const path = `/api/v1/rpc-listeners/${listener.id}`;
  useEffect(() => { setPreview(undefined); setConfirmation(""); }, [revision, listener.paused]);
  if (session.role !== "admin") return null;
  const ready = listener.paused && runtimeState === "paused";
  async function loadAudit(before?: number) {
    setBusy(true); setError("");
    try { setAudit(await read<Page>(`${path}/skip-audit?limit=10${before ? `&before=${before}` : ""}`)); }
    catch (e) { setError((e as Error).message); } finally { setBusy(false); }
  }
  async function execute(confirm: boolean) {
    setBusy(true); setError(""); setSuccess("");
    try {
      if (confirm && preview) {
        const result = await mutate<Audit>(session, preview.configurationRevision, `${path}/skip-to-head`, "POST", { token: preview.token, confirmation });
        setPreview(undefined); setConfirmation(""); setAudit(undefined);
        setSuccess(`Skipped blocks ${result!.fromBlock.toLocaleString()}–${result!.toBlock.toLocaleString()}. Listener enabled; scanning resumes after this checkpoint.`);
        onChanged();
      } else {
        setPreview(await mutate<Preview>(session, revision, `${path}/skip-to-head/preview`, "POST", {})); setConfirmation("");
      }
    } catch (e) { setError((e as Error).message); setPreview(undefined); }
    finally { setBusy(false); }
  }
  return <section class="scanner-skip">
    <h3>Skip to confirmed head</h3>
    <p>Advance the live checkpoint without fetching the skipped events. Recover missing events later through Backfills, while your RPC still retains that history. Existing events and deliveries stay unchanged.</p>
    <p>Pause this listener first and finish or cancel active backfills. Confirming enables the listener again. Reorg recovery may still revisit earlier blocks.</p>
    {!ready && <p role="status">Pause the listener using the header button and wait for its runtime to stop before previewing.</p>}
    <button class="button button--secondary" disabled={!ready || busy} onClick={() => void execute(false)}>Preview skip to confirmed head</button>
    {preview && <div class="scanner-skip-confirm">
      <p>Checkpoint: {preview.previousBlock?.toLocaleString() ?? "Not started"}. Skip <strong>{preview.fromBlock.toLocaleString()}–{preview.toBlock.toLocaleString()}</strong> ({preview.blocks.toLocaleString()} blocks).</p>
      <p>This preview expires at {new Date(preview.expiresAt).toLocaleTimeString()}. Skipped missing events require a later Backfill.</p>
      <label>Type <strong>{preview.confirmation}</strong> to confirm<input aria-label="Skip confirmation" value={confirmation} onInput={e => setConfirmation(e.currentTarget.value)} autoComplete="off" /></label>
      <div class="toolbar"><button class="button" disabled={busy || !ready || confirmation !== preview.confirmation || Date.now() >= Date.parse(preview.expiresAt)} onClick={() => void execute(true)}>Skip and resume scanning</button><button class="button button--secondary" disabled={busy} onClick={() => setPreview(undefined)}>Cancel</button></div>
    </div>}
    {error && <p role="alert">{error}</p>}{success && <p role="status">{success}</p>}
    <details><summary onClick={() => { if (!audit) void loadAudit(); }}>Skip audit history</summary>
      {audit && <><button class="button button--secondary" disabled={busy} onClick={() => void loadAudit()}>Refresh audit</button>{audit.entries.length ? <ul>{audit.entries.map(a => <li key={a.sequence}>{new Date(a.createdAt).toLocaleString()} · {a.actorName} · skipped {a.fromBlock.toLocaleString()}–{a.toBlock.toLocaleString()}</li>)}</ul> : <p>No skips recorded.</p>}{audit.nextBefore && <button class="button button--secondary" disabled={busy} onClick={() => void loadAudit(audit.nextBefore)}>Older entries</button>}</>}
    </details>
  </section>;
}

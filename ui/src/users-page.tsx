import { useEffect, useState } from "preact/hooks";
import type { Session } from "./app";
import { action } from "./api";

type User = { id: string; username: string; role: "admin" | "read-only"; enabled: boolean; lastLoginAt?: string };

export function UsersPage({ session }: { session: Session }) {
  const [users, setUsers] = useState<User[]>([]);
  const [error, setError] = useState("");
  const [adding, setAdding] = useState(false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  async function load() {
    const response = await fetch("/api/v1/users");
    if (!response.ok) throw new Error("Users could not be loaded");
    setUsers((await response.json()).users ?? []);
  }
  useEffect(() => { void load().catch((e) => setError(e.message)); }, []);

  async function create() {
    if (!confirm(`Create ${username} as a read-only user?`)) return;
    await action(session, "/api/v1/users", { username, password, confirm: true });
    setUsername(""); setPassword(""); setAdding(false); await load();
  }
  async function toggle(user: User) {
    if (!confirm(`${user.enabled ? "Disable" : "Enable"} ${user.username}?`)) return;
    await action(session, `/api/v1/users/${user.id}/enabled`, { enabled: !user.enabled, confirm: true });
    await load();
  }
  async function changeRole(user: User) {
    const role = user.role === "admin" ? "read-only" : "admin";
    const verb = role === "admin" ? "Promote" : "Demote";
    if (!confirm(`${verb} ${user.username} to ${role}?`)) return;
    await action(session, `/api/v1/users/${user.id}/role`, { role, confirm: true });
    await load();
  }
  async function reset(user: User) {
    const next = prompt(`New password for ${user.username} (at least 12 characters)`);
    if (!next || !confirm(`Reset password for ${user.username}?`)) return;
    await action(session, `/api/v1/users/${user.id}/password`, { password: next, confirm: true });
  }

  return <section class="activity users-view">
    <div class="section-title"><div><p class="eyebrow">Engine access</p><h1>Users</h1><p class="section-description">New users start read-only. Promote them separately only when administration access is required.</p></div><div class="toolbar"><button class="button button--secondary" onClick={() => void load().catch((e) => setError(e.message))}>Refresh</button><button class="button" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => setAdding(!adding)}>{adding ? "Cancel" : "Add user"}</button></div></div>
    {error && <p class="alert" role="alert">{error}</p>}
    {adding && <form class="panel user-create" onSubmit={(event) => { event.preventDefault(); setError(""); void create().catch((e) => setError(e.message)); }}>
      <div><h2>Create read-only user</h2><p>Administrative access can be granted from the user list after creation.</p></div>
      <label>Username<input required minLength={3} maxLength={64} autocomplete="username" value={username} onInput={(e) => setUsername(e.currentTarget.value)} /></label>
      <label>Temporary password<input required minLength={12} type="password" autocomplete="new-password" value={password} onInput={(e) => setPassword(e.currentTarget.value)} /></label>
      <button class="button" type="submit">Create read-only user</button>
    </form>}
    <div class="panel"><table class="users-table"><thead><tr><th>User</th><th>Role</th><th>Status</th><th>Last login</th><th>Actions</th></tr></thead><tbody>{users.map((user) => <tr key={user.id}><td><strong>{user.username}</strong></td><td><span class="role">{user.role}</span></td><td>{user.enabled ? "Active" : "Disabled"}</td><td>{user.lastLoginAt ? new Date(user.lastLoginAt).toLocaleString() : "Never"}</td><td><div class="toolbar"><button class="button button--secondary" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void changeRole(user).catch((e) => setError(e.message))}>{user.role === "admin" ? "Make read-only" : "Promote to admin"}</button><button class="button button--secondary" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void toggle(user).catch((e) => setError(e.message))}>{user.enabled ? "Disable" : "Enable"}</button><button class="button button--secondary" disabled={session.role !== "admin"} title={session.role !== "admin" ? "Administrator access required" : undefined} onClick={() => void reset(user).catch((e) => setError(e.message))}>Reset password</button></div></td></tr>)}</tbody></table></div>
  </section>;
}

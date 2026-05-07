import { useEffect, useState } from "react";
import type { Session } from "../lib/api";

interface Props {
  sessions: Session[];
  activeId?: string;
  onSelect: (id: string) => void;
  onRefresh: (id: string) => void;
  onDelete: (id: string) => void;
}

function formatTTL(expiresAt: string, now: number): string {
  const ms = new Date(expiresAt).getTime() - now;
  if (ms <= 0) return "expired";
  const s = Math.floor(ms / 1000);
  const m = Math.floor(s / 60);
  if (m > 0) return `${m}m ${s % 60}s`;
  return `${s}s`;
}

export function SessionList({
  sessions,
  activeId,
  onSelect,
  onRefresh,
  onDelete,
}: Props) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  if (sessions.length === 0) {
    return (
      <aside className="session-list session-list--empty">
        <p>No sessions yet. Pick an image and start one.</p>
      </aside>
    );
  }

  return (
    <aside className="session-list">
      <h2>Sessions</h2>
      <ul>
        {sessions.map((s) => {
          const active = s.id === activeId;
          return (
            <li
              key={s.id}
              className={
                active
                  ? "session-list__item session-list__item--active"
                  : "session-list__item"
              }
            >
              <button
                type="button"
                className="session-list__select"
                onClick={() => onSelect(s.id)}
              >
                <span className="session-list__image">{s.image}</span>
                <span className="session-list__id">{s.id.slice(0, 8)}</span>
                <span
                  className="session-list__state"
                  data-state={s.container_state}
                >
                  {s.container_state}
                </span>
                <span className="session-list__ttl">
                  TTL {formatTTL(s.expires_at, now)}
                </span>
              </button>
              <div className="session-list__actions">
                <button
                  type="button"
                  onClick={() => onRefresh(s.id)}
                  aria-label="Refresh TTL"
                >
                  refresh
                </button>
                <button
                  type="button"
                  onClick={() => onDelete(s.id)}
                  aria-label="Delete session"
                >
                  delete
                </button>
              </div>
            </li>
          );
        })}
      </ul>
    </aside>
  );
}

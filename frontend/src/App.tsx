import { useCallback, useEffect, useMemo, useState } from "react";
import { ImagePicker } from "./components/ImagePicker";
import { SessionList } from "./components/SessionList";
import { Terminal } from "./components/Terminal";
import { api, ApiClientError, type Session } from "./lib/api";
import "./App.css";

const API_BASE = import.meta.env.VITE_API_BASE ?? "";

function App() {
  const [images, setImages] = useState<string[]>([]);
  const [sessions, setSessions] = useState<Session[]>([]);
  const [activeId, setActiveId] = useState<string | undefined>();
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState<string | undefined>();

  const refreshSessions = useCallback(async () => {
    try {
      const res = await api.listSessions();
      setSessions(res.sessions);
    } catch (e) {
      setError(e instanceof Error ? e.message : "failed to load sessions");
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    void api
      .images()
      .then((r) => {
        if (!cancelled) setImages(r.images);
      })
      .catch((e) => {
        if (!cancelled)
          setError(e instanceof Error ? e.message : "failed to load images");
      });
    const tick = () => {
      if (!cancelled) void refreshSessions();
    };
    tick();
    const t = setInterval(tick, 5000);
    return () => {
      cancelled = true;
      clearInterval(t);
    };
  }, [refreshSessions]);

  const startSession = useCallback(
    async (image: string, ttlSeconds: number) => {
      setError(undefined);
      setCreating(true);
      try {
        const created = await api.createSession({
          image,
          ttl_seconds: ttlSeconds,
        });
        setActiveId(created.id);
        await refreshSessions();
      } catch (e) {
        const msg =
          e instanceof ApiClientError
            ? `${e.code}: ${e.message}`
            : (e as Error).message;
        setError(msg);
      } finally {
        setCreating(false);
      }
    },
    [refreshSessions],
  );

  const refresh = useCallback(
    async (id: string) => {
      await api.refreshSession(id);
      await refreshSessions();
    },
    [refreshSessions],
  );

  const remove = useCallback(
    async (id: string) => {
      await api.deleteSession(id);
      if (activeId === id) setActiveId(undefined);
      await refreshSessions();
    },
    [activeId, refreshSessions],
  );

  const active = useMemo(
    () => sessions.find((s) => s.id === activeId),
    [sessions, activeId],
  );

  return (
    <div className="app">
      <header className="app__header">
        <h1>devenv-manager</h1>
        <p className="app__subtitle">Container-backed development sessions</p>
      </header>

      <div className="app__layout">
        <div className="app__sidebar">
          <ImagePicker images={images} onStart={startSession} busy={creating} />
          <SessionList
            sessions={sessions}
            activeId={activeId}
            onSelect={setActiveId}
            onRefresh={(id) => void refresh(id)}
            onDelete={(id) => void remove(id)}
          />
        </div>

        <main className="app__main">
          {error && (
            <div role="alert" className="error">
              {error}
            </div>
          )}
          {active ? (
            <Terminal
              key={active.id}
              sessionId={active.id}
              wsPath={`/v1/sessions/${active.id}/ws`}
              apiBase={API_BASE}
              onClosed={() => void refreshSessions()}
            />
          ) : (
            <div className="empty">
              Select or start a session to open a terminal.
            </div>
          )}
        </main>
      </div>
    </div>
  );
}

export default App;

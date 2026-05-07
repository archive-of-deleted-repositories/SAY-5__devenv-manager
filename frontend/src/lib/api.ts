// Typed REST client for the devenv-manager backend. The base URL defaults
// to /api in production (so the frontend is served behind the same origin)
// and to http://localhost:8080 during dev.

export interface CreateSessionRequest {
  image: string;
  ttl_seconds?: number;
  working_dir?: string;
  env?: Record<string, string>;
  command?: string[];
}

export interface CreateSessionResponse {
  id: string;
  container_id: string;
  ws_url: string;
  expires_at: string;
  image: string;
}

export interface Session {
  id: string;
  image: string;
  container_id: string;
  container_state: string;
  created_at: string;
  expires_at: string;
  working_dir?: string;
}

export interface ListSessionsResponse {
  sessions: Session[];
  next_cursor?: string;
}

export interface ImagesResponse {
  images: string[];
}

export interface ApiError {
  code: string;
  message: string;
  retryable: boolean;
  request_id?: string;
}

export class ApiClientError extends Error {
  readonly code: string;
  readonly retryable: boolean;
  readonly status: number;
  constructor(status: number, body: ApiError) {
    super(body.message);
    this.code = body.code;
    this.retryable = body.retryable;
    this.status = status;
  }
}

const DEFAULT_BASE = (() => {
  if (typeof window === "undefined") return "http://localhost:8080";
  // In dev, Vite proxies /api to the Go server; in production we expect a
  // reverse proxy to do the same.
  return import.meta.env.VITE_API_BASE ?? "";
})();

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  base = DEFAULT_BASE,
): Promise<T> {
  const res = await fetch(base + path, {
    method,
    headers: body ? { "content-type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    let detail: ApiError = {
      code: "unknown",
      message: `HTTP ${res.status}`,
      retryable: false,
    };
    try {
      detail = (await res.json()) as ApiError;
    } catch {
      // ignore parse failure; fall through with default
    }
    throw new ApiClientError(res.status, detail);
  }
  if (res.status === 204) {
    return undefined as T;
  }
  return (await res.json()) as T;
}

export const api = {
  health: () =>
    request<{ status: string; docker_version?: string }>("GET", "/healthz"),
  images: () => request<ImagesResponse>("GET", "/v1/images"),
  listSessions: () => request<ListSessionsResponse>("GET", "/v1/sessions"),
  createSession: (req: CreateSessionRequest) =>
    request<CreateSessionResponse>("POST", "/v1/sessions", req),
  refreshSession: (id: string) =>
    request<{ id: string; expires_at: string }>(
      "POST",
      `/v1/sessions/${encodeURIComponent(id)}/refresh`,
    ),
  deleteSession: (id: string) =>
    request<void>("DELETE", `/v1/sessions/${encodeURIComponent(id)}`),
};

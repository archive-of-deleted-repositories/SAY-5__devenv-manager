// WebSocket protocol client. Mirrors the Go-side frame definitions in
// pkg/api/types.go. Stays decoupled from xterm so it can be unit-tested
// without a DOM.

export type FrameType =
  | "input"
  | "output"
  | "resize"
  | "ping"
  | "pong"
  | "close";

export interface OutputFrame {
  type: "output";
  data: string; // base64
  stream?: "stdout" | "stderr";
}

export interface InputFrame {
  type: "input";
  data: string; // base64
}

export interface ResizeFrame {
  type: "resize";
  cols: number;
  rows: number;
}

export interface PingFrame {
  type: "ping";
}

export interface PongFrame {
  type: "pong";
}

export interface CloseFrame {
  type: "close";
  reason?: string;
}

export type Frame =
  | OutputFrame
  | InputFrame
  | ResizeFrame
  | PingFrame
  | PongFrame
  | CloseFrame;

export function encodeBytes(input: string | Uint8Array): string {
  const bytes =
    typeof input === "string" ? new TextEncoder().encode(input) : input;
  let s = "";
  for (let i = 0; i < bytes.byteLength; i++) {
    s += String.fromCharCode(bytes[i]);
  }
  if (typeof btoa !== "undefined") return btoa(s);
  // Node fallback used by vitest in jsdom; same result.
  return Buffer.from(bytes).toString("base64");
}

export function decodeBytes(b64: string): Uint8Array {
  if (typeof atob !== "undefined") {
    const s = atob(b64);
    const out = new Uint8Array(s.length);
    for (let i = 0; i < s.length; i++) out[i] = s.charCodeAt(i);
    return out;
  }
  return new Uint8Array(Buffer.from(b64, "base64"));
}

export interface TerminalSocketHandlers {
  onOutput?: (data: Uint8Array, stream: "stdout" | "stderr") => void;
  onClose?: (ev: CloseEvent) => void;
  onError?: (ev: Event) => void;
  onOpen?: () => void;
}

/** Wraps a WebSocket with the typed frame protocol. */
export class TerminalSocket {
  private ws: WebSocket;
  private pingTimer?: ReturnType<typeof setInterval>;
  private handlers: TerminalSocketHandlers;

  constructor(url: string, handlers: TerminalSocketHandlers = {}) {
    this.handlers = handlers;
    this.ws = new WebSocket(url);
    this.ws.addEventListener("open", () => {
      this.handlers.onOpen?.();
      // Refresh TTL every 25s so the 60s lease never lapses on an active session.
      this.pingTimer = setInterval(() => this.send({ type: "ping" }), 25_000);
    });
    this.ws.addEventListener("message", (ev) => {
      try {
        const frame = JSON.parse(ev.data as string) as Frame;
        if (frame.type === "output") {
          const bytes = decodeBytes(frame.data);
          this.handlers.onOutput?.(bytes, frame.stream ?? "stdout");
        }
      } catch {
        // Drop malformed frames; backend invariant guarantees JSON.
      }
    });
    this.ws.addEventListener("close", (ev) => {
      if (this.pingTimer) clearInterval(this.pingTimer);
      this.handlers.onClose?.(ev);
    });
    this.ws.addEventListener("error", (ev) => this.handlers.onError?.(ev));
  }

  send(frame: Frame): void {
    if (this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(frame));
    }
  }

  sendInput(data: string | Uint8Array): void {
    this.send({ type: "input", data: encodeBytes(data) });
  }

  resize(cols: number, rows: number): void {
    this.send({ type: "resize", cols, rows });
  }

  close(): void {
    if (this.pingTimer) clearInterval(this.pingTimer);
    if (
      this.ws.readyState === WebSocket.OPEN ||
      this.ws.readyState === WebSocket.CONNECTING
    ) {
      this.ws.close();
    }
  }
}

export function buildWSURL(httpBase: string, path: string): string {
  const proto = httpBase.startsWith("https") ? "wss" : "ws";
  const host = httpBase.replace(/^https?:\/\//, "");
  return `${proto}://${host}${path}`;
}

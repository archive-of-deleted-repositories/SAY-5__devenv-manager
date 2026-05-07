import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import {
  TerminalSocket,
  encodeBytes,
  decodeBytes,
  buildWSURL,
} from "../lib/ws";

class FakeWebSocket {
  static OPEN = 1;
  static CONNECTING = 0;
  static CLOSED = 3;
  readyState = FakeWebSocket.CONNECTING;
  sent: string[] = [];
  listeners: Record<string, ((ev: unknown) => void)[]> = {};
  url: string;
  constructor(url: string) {
    this.url = url;
    setTimeout(() => {
      this.readyState = FakeWebSocket.OPEN;
      this.fire("open", {});
    }, 0);
  }
  addEventListener(name: string, fn: (ev: unknown) => void) {
    (this.listeners[name] ??= []).push(fn);
  }
  send(data: string) {
    this.sent.push(data);
  }
  close() {
    this.readyState = FakeWebSocket.CLOSED;
    this.fire("close", { code: 1000, reason: "normal" });
  }
  fire(name: string, ev: unknown) {
    (this.listeners[name] ?? []).forEach((fn) => fn(ev));
  }
}

beforeEach(() => {
  vi.useFakeTimers();
  // @ts-expect-error: jsdom provides WebSocket but we want a controllable stub
  globalThis.WebSocket = FakeWebSocket;
});

afterEach(() => {
  vi.useRealTimers();
});

describe("encode/decode", () => {
  it("round-trips ASCII", () => {
    const enc = encodeBytes("hello");
    const dec = new TextDecoder().decode(decodeBytes(enc));
    expect(dec).toBe("hello");
  });
  it("round-trips bytes", () => {
    const bytes = new Uint8Array([0xff, 0x00, 0x7f, 0x80]);
    const enc = encodeBytes(bytes);
    expect(Array.from(decodeBytes(enc))).toEqual([0xff, 0x00, 0x7f, 0x80]);
  });
});

describe("buildWSURL", () => {
  it("uses ws for http", () => {
    expect(buildWSURL("http://localhost:8080", "/v1/sessions/abc/ws")).toBe(
      "ws://localhost:8080/v1/sessions/abc/ws",
    );
  });
  it("uses wss for https", () => {
    expect(buildWSURL("https://example.com", "/v1/sessions/abc/ws")).toBe(
      "wss://example.com/v1/sessions/abc/ws",
    );
  });
});

describe("TerminalSocket", () => {
  it("sends input frames as JSON with base64", async () => {
    const onOpen = vi.fn();
    const sock = new TerminalSocket("ws://localhost/x", { onOpen });
    await vi.runOnlyPendingTimersAsync();
    expect(onOpen).toHaveBeenCalled();
    // @ts-expect-error reaching into private for assertion
    const ws = sock["ws"] as FakeWebSocket;
    sock.sendInput("hi");
    expect(ws.sent).toHaveLength(1);
    const parsed = JSON.parse(ws.sent[0]) as { type: string; data: string };
    expect(parsed.type).toBe("input");
    expect(Array.from(decodeBytes(parsed.data))).toEqual([0x68, 0x69]);
    sock.close();
  });

  it("sends ping every 25 seconds", async () => {
    const sock = new TerminalSocket("ws://localhost/x", {});
    await vi.runOnlyPendingTimersAsync();
    // @ts-expect-error: reaching into private for assertion
    const ws = sock["ws"] as FakeWebSocket;
    expect(ws.sent.filter((s) => s.includes('"ping"'))).toHaveLength(0);
    await vi.advanceTimersByTimeAsync(25_000);
    expect(ws.sent.filter((s) => s.includes('"ping"'))).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(25_000);
    expect(ws.sent.filter((s) => s.includes('"ping"'))).toHaveLength(2);
    sock.close();
  });

  it("decodes output frames into bytes for the handler", async () => {
    const got: Uint8Array[] = [];
    const sock = new TerminalSocket("ws://localhost/x", {
      onOutput: (b) => got.push(b),
    });
    await vi.runOnlyPendingTimersAsync();
    // @ts-expect-error reaching into private for test
    const ws = sock["ws"] as FakeWebSocket;
    const frame = JSON.stringify({
      type: "output",
      data: encodeBytes("hello"),
    });
    ws.fire("message", { data: frame });
    expect(got).toHaveLength(1);
    expect(new TextDecoder().decode(got[0])).toBe("hello");
    sock.close();
  });
});

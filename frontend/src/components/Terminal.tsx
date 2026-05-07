import { useEffect, useRef } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";

import { TerminalSocket, buildWSURL } from "../lib/ws";

interface Props {
  sessionId: string;
  wsPath: string;
  apiBase: string;
  onClosed?: () => void;
}

/** xterm.js-backed terminal wired to a WebSocket relay. */
export function Terminal({ sessionId, wsPath, apiBase, onClosed }: Props) {
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!containerRef.current) return;
    const term = new XTerm({
      cursorBlink: true,
      convertEol: true,
      fontFamily: '"Fira Code", "JetBrains Mono", ui-monospace, monospace',
      fontSize: 13,
      theme: { background: "#0d1117", foreground: "#c9d1d9" },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(containerRef.current);
    fit.fit();

    const decoder = new TextDecoder();
    const socket = new TerminalSocket(
      buildWSURL(apiBase || window.location.origin, wsPath),
      {
        onOpen: () => {
          term.write("\x1b[1;32mConnected.\x1b[0m\r\n");
          socket.resize(term.cols, term.rows);
        },
        onOutput: (bytes) => term.write(decoder.decode(bytes)),
        onClose: () => {
          term.write("\r\n\x1b[1;33mSession closed.\x1b[0m\r\n");
          onClosed?.();
        },
        onError: () => term.write("\r\n\x1b[1;31mWebSocket error.\x1b[0m\r\n"),
      },
    );

    const dataDisp = term.onData((data) => socket.sendInput(data));
    const resizeDisp = term.onResize(({ cols, rows }) =>
      socket.resize(cols, rows),
    );

    const onWindowResize = () => fit.fit();
    window.addEventListener("resize", onWindowResize);

    // Test hook: expose a stable name so e2e specs can drive the input
    // path without depending on xterm's keyboard handling, which is
    // notoriously unreliable through synthesized events. This is gated
    // behind a top-level flag that is never set in production builds.
    if (
      typeof window !== "undefined" &&
      (window as unknown as { __DEVENV_E2E_HOOKS?: boolean }).__DEVENV_E2E_HOOKS
    ) {
      (
        window as unknown as {
          __devenvTerminalSendInput?: (s: string) => void;
        }
      ).__devenvTerminalSendInput = (s: string) => socket.sendInput(s);
    }

    return () => {
      window.removeEventListener("resize", onWindowResize);
      dataDisp.dispose();
      resizeDisp.dispose();
      socket.close();
      term.dispose();
      if (
        typeof window !== "undefined" &&
        (window as unknown as { __devenvTerminalSendInput?: unknown })
          .__devenvTerminalSendInput
      ) {
        delete (window as unknown as { __devenvTerminalSendInput?: unknown })
          .__devenvTerminalSendInput;
      }
    };
  }, [sessionId, wsPath, apiBase, onClosed]);

  return (
    <div className="terminal" data-testid="terminal-host" ref={containerRef} />
  );
}

import { createStream } from "@/stream/core";

import type { ShowMessage, StreamMessage } from "@/stream/core";

const WORKER_NAME = "sortie-change-stream";

interface Connection {
  disconnect(): void;
  show(dispatchId: string | null): void;
}

function connectFallback(onMessage: (m: StreamMessage) => void, shown: string | null): Connection {
  let live = true;
  const stream = createStream((message) => {
    if (live) {
      onMessage(message);
    }
  });
  const show = (dispatchId: string | null) => {
    stream.watch(dispatchId === null ? [] : [dispatchId]);
    const held = dispatchId === null ? undefined : stream.snapshot(dispatchId);
    if (held !== undefined) {
      onMessage(held);
    }
  };
  show(shown);
  stream.open();
  return {
    disconnect: () => {
      live = false;
    },
    show,
  };
}

function connectWorker(
  onMessage: (m: StreamMessage) => void,
  lastShown: () => string | null,
): Connection {
  const worker = new SharedWorker(new URL("./worker.ts", import.meta.url), {
    name: WORKER_NAME,
  });
  let fallback: Connection | undefined;
  worker.port.onmessage = (event: MessageEvent<StreamMessage>) => onMessage(event.data);
  worker.onerror = () => {
    worker.port.close();
    fallback ??= connectFallback(onMessage, lastShown());
  };
  worker.port.start();
  return {
    disconnect: () => {
      worker.port.onmessage = null;
      worker.port.close();
      fallback?.disconnect();
    },
    show: (dispatchId) => {
      if (fallback === undefined) {
        worker.port.postMessage({ kind: "show", dispatchId } satisfies ShowMessage);
      } else {
        fallback.show(dispatchId);
      }
    },
  };
}

/** Calls `onMessage` with the current status at once, then with every message. A shared worker holds the single connection of the browser; where one cannot be built the tab runs the stream itself. `show` names the run whose events the tab receives, or null for none. */
export function connectChangeStream(onMessage: (m: StreamMessage) => void): Connection {
  let shown: string | null = null;
  onMessage({ kind: "status", status: "connecting" });
  let connection: Connection;
  if (typeof SharedWorker !== "function") {
    connection = connectFallback(onMessage, shown);
  } else {
    try {
      connection = connectWorker(onMessage, () => shown);
    } catch {
      connection = connectFallback(onMessage, shown);
    }
  }
  return {
    disconnect: () => connection.disconnect(),
    show: (dispatchId) => {
      shown = dispatchId;
      connection.show(dispatchId);
    },
  };
}

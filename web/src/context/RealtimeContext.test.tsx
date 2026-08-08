import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, render } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { RealtimeProvider } from "./RealtimeContext";

class FakeWebSocket {
  static instances: FakeWebSocket[] = [];

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  onclose: ((event: CloseEvent) => void) | null = null;

  constructor(readonly url: string) {
    FakeWebSocket.instances.push(this);
  }

  close() {
    this.onclose?.(new CloseEvent("close"));
  }
}

afterEach(() => {
  FakeWebSocket.instances = [];
  vi.unstubAllGlobals();
});

describe("实时节点事件", () => {
  it("节点状态变化时同步刷新实例节点投影", () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <RealtimeProvider><span>ready</span></RealtimeProvider>
      </QueryClientProvider>,
    );

    expect(FakeWebSocket.instances).toHaveLength(1);
    act(() => {
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({ id: 1, type: "node.offline", resource_type: "node", resource_id: "node-a", created_at: "2026-08-07T00:00:00Z" }),
      } as MessageEvent);
    });

    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["nodes"] });
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["instances"] });
  });

  it("任务更新时刷新运行环境投影", () => {
    vi.stubGlobal("WebSocket", FakeWebSocket);
    const client = new QueryClient();
    const invalidate = vi.spyOn(client, "invalidateQueries");
    render(
      <QueryClientProvider client={client}>
        <RealtimeProvider><span>ready</span></RealtimeProvider>
      </QueryClientProvider>,
    );

    act(() => {
      FakeWebSocket.instances[0].onmessage?.({
        data: JSON.stringify({ id: 2, type: "job.updated", resource_type: "job", resource_id: "job-a", created_at: "2026-08-09T00:00:00Z" }),
      } as MessageEvent);
    });

    expect(invalidate).toHaveBeenCalledWith({ queryKey: ["environments"] });
  });
});

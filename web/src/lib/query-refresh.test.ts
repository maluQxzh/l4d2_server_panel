import { QueryClient, QueryObserver } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";
import { refreshQueries } from "./query-refresh";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

describe("写入或事件后的查询刷新", () => {
  it.each([
    ["maps", "node-a"], ["nodes"], ["jobs"], ["environments"],
    ["instances", "instance-a", "packages"], ["instances", "instance-a", "backups"],
  ])("首次 %j 请求未返回时也读取写入后的新数据，丢弃迟到旧响应", async (...key) => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const old = deferred<string[]>();
    const current = deferred<string[]>();
    const fetch = vi.fn().mockReturnValueOnce(old.promise).mockReturnValueOnce(current.promise);
    const observer = new QueryObserver(client, { queryKey: key, queryFn: fetch });
    const unsubscribe = observer.subscribe(() => {});
    expect(fetch).toHaveBeenCalledTimes(1);
    const refresh = refreshQueries(client, { queryKey: [key[0]] });
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(2));
    current.resolve(["new"]);
    await refresh;
    old.resolve(["old"]);
    await old.promise;
    expect(client.getQueryData(key)).toEqual(["new"]);
    unsubscribe(); client.clear();
  });

  it("没有查看权限的禁用查询和未挂载查询只失效，不主动请求", async () => {
    const client = new QueryClient();
    const fetch = vi.fn();
    const observer = new QueryObserver(client, { queryKey: ["maps", "denied"], queryFn: fetch, enabled: false });
    const unsubscribe = observer.subscribe(() => {});
    client.setQueryData(["maps", "inactive"], ["old"]);
    await refreshQueries(client, { queryKey: ["maps"] });
    expect(fetch).not.toHaveBeenCalled();
    expect(client.getQueryState(["maps", "inactive"])?.isInvalidated).toBe(true);
    unsubscribe(); client.clear();
  });
});

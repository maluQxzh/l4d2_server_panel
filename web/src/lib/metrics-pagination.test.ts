import { afterEach, expect, it, vi } from "vitest";
import { api } from "./api";
afterEach(() => vi.unstubAllGlobals());

it("指标自动读取后续页并固定首次时间范围", async () => {
  const fetch=vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ items:[{ players:1 }],from:"2026-10-01T00:00:00Z",to:"2026-10-08T00:00:00Z",resolution:"1m",next_cursor:"tail" })))
    .mockResolvedValueOnce(new Response(JSON.stringify({ items:[{ players:2 }],from:"2026-10-01T00:00:00Z",to:"2026-10-08T00:00:00Z",resolution:"1m",next_cursor:"" })));
  vi.stubGlobal("fetch",fetch);
  const result=await api.metrics({ instanceId:"instance" });
  expect(result.items.map((item)=>item.players)).toEqual([1,2]);
  const url=new URL(fetch.mock.calls[1][0],"https://example.test");
  expect(url.searchParams.get("cursor")).toBe("tail");
  expect(url.searchParams.get("to")).toBe("2026-10-08T00:00:00Z");
});

it("重复游标报错，不返回看似完整的部分指标", async () => {
  vi.stubGlobal("fetch",vi.fn(async()=>new Response(JSON.stringify({items:[],from:"a",to:"b",next_cursor:"same"}))));
  await expect(api.metrics({instanceId:"instance"})).rejects.toMatchObject({code:"invalid_metrics_cursor"});
});

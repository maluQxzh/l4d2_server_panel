import { afterEach, expect, it, vi } from "vitest";
import { api } from "./api";
afterEach(() => vi.unstubAllGlobals());
it("读取后续地图页并保留节点筛选", async () => {
  const fetch = vi.fn().mockResolvedValueOnce(new Response(JSON.stringify({ items: [{ id: "a" }], next_cursor: "next" }))).mockResolvedValueOnce(new Response(JSON.stringify({ items: [{ id: "b" }] })));
  vi.stubGlobal("fetch", fetch);
  expect((await api.maps("node")).items.map((map) => map.id)).toEqual(["a", "b"]);
  expect(fetch.mock.calls[1][0]).toContain("node_id=node&cursor=next");
});

import { lazy, Suspense } from "react";
import { render, screen } from "@testing-library/react";
import { expect, it, vi } from "vitest";
import { PageErrorBoundary } from "./PageErrorBoundary";

it("懒加载失败显示可恢复界面，不显示原始错误信息", async () => {
  vi.spyOn(console,"error").mockImplementation(()=>{});
  const Broken=lazy(async()=>{throw new Error("private-chunk-url");});
  render(<PageErrorBoundary resetKey="/jobs"><Suspense fallback="loading"><Broken /></Suspense></PageErrorBoundary>);
  expect(await screen.findByRole("alert")).toHaveTextContent("页面暂时无法显示");
  expect(screen.getByRole("button",{name:"重新加载"})).toBeEnabled();
  expect(screen.queryByText("private-chunk-url")).not.toBeInTheDocument();
});

it("组件渲染异常被捕获，切换路径后可恢复", () => {
  vi.spyOn(console,"error").mockImplementation(()=>{});
  function Broken(): never {throw new Error("render failed");}
  const view=render(<PageErrorBoundary resetKey="/broken"><Broken /></PageErrorBoundary>);
  expect(screen.getByRole("alert")).toBeInTheDocument();
  view.rerender(<PageErrorBoundary resetKey="/"><span>正常页面</span></PageErrorBoundary>);
  expect(screen.getByText("正常页面")).toBeInTheDocument();
});

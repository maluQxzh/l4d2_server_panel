import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Button, EmptyState, Modal, ProgressBar, StatusBadge } from "./ui";

describe("基础界面组件", () => {
  it("显示可理解的中文状态", () => {
    render(<StatusBadge status="running" pulse />);
    expect(screen.getByText("运行中")).toBeInTheDocument();
  });

  it("限制越界进度并提供可访问名称", () => {
    render(<ProgressBar value={140} />);
    expect(screen.getByLabelText("进度 100%")).toBeInTheDocument();
  });

  it("显示空状态说明", () => {
    render(<EmptyState title="暂无节点" description="先接入一个节点" />);
    expect(screen.getByRole("heading", { name: "暂无节点" })).toBeInTheDocument();
    expect(screen.getByText("先接入一个节点")).toBeInTheDocument();
  });

  it("加载中的按钮不可重复提交", () => {
    render(<Button loading>提交</Button>);
    expect(screen.getByRole("button", { name: "提交" })).toBeDisabled();
  });

  it("为模态窗口关联标题说明并支持 Escape 关闭", () => {
    const close = vi.fn();
    render(<Modal open title="安全确认" description="操作不可撤销" onClose={close}><button>继续</button></Modal>);
    const dialog = screen.getByRole("dialog", { name: "安全确认", description: "操作不可撤销" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(close).toHaveBeenCalledOnce();
  });
});

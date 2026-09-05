import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { FileUploadInput } from "./FileUploadInput";

function transfer(files: File[], directory = false) {
  return { files, types: ["Files"], items: files.map(() => ({ kind: "file", webkitGetAsEntry: () => ({ isDirectory: directory }) })), dropEffect: "none" };
}

describe("统一文件上传入口", () => {
  it("点击选择和拖拽调用相同处理函数，支持再次选择同一文件", async () => {
    const choose = vi.fn();
    render(<FileUploadInput label="上传文件" accept=".zip" maxBytes={100} onFile={choose} />);
    const file = new File(["archive"], "插件.ZIP", { type: "" });
    const zone = screen.getByTestId("file-dropzone");
    fireEvent.dragEnter(zone, { dataTransfer: transfer([file]) });
    expect(zone).toHaveClass("file-dropzone--active");
    fireEvent.drop(zone, { dataTransfer: transfer([file]) });
    await screen.findByText("已选择：插件.ZIP");
    expect(choose).toHaveBeenCalledWith(file);
    expect(zone).not.toHaveClass("file-dropzone--active");
    fireEvent.change(screen.getByLabelText("上传文件"), { target: { files: [file] } });
    await waitFor(() => expect(choose).toHaveBeenCalledTimes(2));
  });

  it.each([
    { title: "多个文件", files: [new File(["x"], "a.zip"), new File(["x"], "b.zip")], message: /每次只能选择一个/ },
    { title: "类型不匹配", files: [new File(["x"], "a.exe", { type: "application/zip" })], message: /不支持此文件类型/ },
    { title: "超限", files: [new File(["x".repeat(11)], "a.zip")], message: /不能超过/ },
    { title: "空文件", files: [new File([], "a.zip")], message: /文件不能为空/ },
  ])("拒绝$title，不进入上传处理", async ({ files, message }) => {
    const choose = vi.fn();
    render(<FileUploadInput label="上传文件" accept=".zip,application/zip" minBytes={1} maxBytes={10} onFile={choose} />);
    fireEvent.drop(screen.getByTestId("file-dropzone"), { dataTransfer: transfer(files) });
    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(choose).not.toHaveBeenCalled();
  });

  it("拒绝文件夹，忽略上传期间的拖拽和点击", () => {
    const choose = vi.fn();
    const props = { label: "上传文件", maxBytes: 100, onFile: choose };
    const view = render(<FileUploadInput {...props} />);
    const file = new File([], "folder");
    fireEvent.drop(screen.getByTestId("file-dropzone"), { dataTransfer: transfer([file], true) });
    expect(screen.getByRole("alert")).toHaveTextContent("不支持拖入文件夹");
    view.rerender(<FileUploadInput {...props} disabled />);
    fireEvent.dragEnter(screen.getByTestId("file-dropzone"), { dataTransfer: transfer([file]) });
    fireEvent.drop(screen.getByTestId("file-dropzone"), { dataTransfer: transfer([file]) });
    expect(choose).not.toHaveBeenCalled();
    expect(screen.getByLabelText("上传文件")).toBeDisabled();
    expect(screen.getByTestId("file-dropzone")).not.toHaveClass("file-dropzone--active");
  });

  it("异步读取完成前不能替换文件，读取错误允许重试", async () => {
    let reject!: (error: Error) => void;
    const choose = vi.fn(() => new Promise<void>((_, no) => { reject = no; }));
    render(<FileUploadInput label="上传文件" maxBytes={100} onFile={choose} />);
    const file = new File(["x"], "a.cfg");
    fireEvent.drop(screen.getByTestId("file-dropzone"), { dataTransfer: transfer([file]) });
    expect(screen.getByLabelText("上传文件")).toBeDisabled();
    fireEvent.drop(screen.getByTestId("file-dropzone"), { dataTransfer: transfer([file]) });
    expect(choose).toHaveBeenCalledTimes(1);
    reject(new Error("无法读取文件"));
    await screen.findByText("无法读取文件");
    expect(screen.getByLabelText("上传文件")).toBeEnabled();
  });

  it("文件掉在区域外不会打开本机文件，文本拖动不被拦截，卸载后清理监听", () => {
    const view = render(<FileUploadInput label="上传文件" maxBytes={100} onFile={vi.fn()} />);
    const dispatch = (types: string[]) => {
      const event = new Event("drop", { bubbles: true, cancelable: true });
      Object.defineProperty(event, "dataTransfer", { value: { types } });
      document.dispatchEvent(event);
      return event.defaultPrevented;
    };
    expect(dispatch(["Files"])).toBe(true);
    expect(dispatch(["text/plain"])).toBe(false);
    view.unmount();
    expect(dispatch(["Files"])).toBe(false);
  });

  it("拖过内部元素不会闪烁；仅提供 MIME 的类型限制同样有效", async () => {
    render(<FileUploadInput label="上传图片" accept="image/*" maxBytes={100} onFile={vi.fn()} />);
    const zone = screen.getByTestId("file-dropzone");
    const file = new File(["x"], "a", { type: "image/png" });
    fireEvent.dragEnter(zone, { dataTransfer: transfer([file]) });
    fireEvent.dragEnter(screen.getByText(/拖拽|松开/), { dataTransfer: transfer([file]) });
    fireEvent.dragLeave(zone);
    expect(zone).toHaveClass("file-dropzone--active");
    fireEvent.drop(zone, { dataTransfer: transfer([file]) });
    await screen.findByText("已选择：a");
  });
});

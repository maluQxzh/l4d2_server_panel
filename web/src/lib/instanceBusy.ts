import { ApiError } from "./api";

// Only retry a definite rejection caused by the instance write barrier. A
// generic 409, a lost response or a server error may require user intervention.
// The caller retains one idempotency key and an immutable request per item.
export async function submitWhenInstanceReady<T>(submit: () => Promise<T>, stopped: () => boolean, waiting: (message: string) => void): Promise<T> {
  const deadline = Date.now() + 10 * 60_000;
  let delay = 750;
  while (!stopped()) {
    try {
      return await submit();
    } catch (error) {
      if (stopped()) break;
      if (!(error instanceof ApiError) || error.status !== 409 || error.code !== "conflict" || error.reason !== "instance_busy") throw error;
      if (Date.now() >= deadline) throw new Error("等待实例空闲超时，已停止后续操作；请检查任务中心后重试");
      waiting(`等待实例空闲：${error.message}`);
      await new Promise((resolve) => setTimeout(resolve, Math.min(delay, deadline - Date.now())));
      delay = Math.min(delay * 2, 3000);
      if (Date.now() >= deadline) throw new Error("等待实例空闲超时，已停止后续操作；请检查任务中心后重试");
    }
  }
  throw new Error("已停止后续操作；尚未提交的操作不会执行");
}

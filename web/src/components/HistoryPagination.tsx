import { useState } from "react";
import { Button } from "./ui";

export function useHistoryCursor(scope = "") {
  const [state, setState] = useState({ scope, cursors: [""] });
  const cursors = state.scope === scope ? state.cursors : [""];
  return {
    cursor: cursors[cursors.length - 1], page: cursors.length,
    previous: () => setState({ scope, cursors: cursors.slice(0, -1) }),
    next: (cursor: string) => setState({ scope, cursors: [...cursors, cursor] }),
  };
}

export function HistoryPagination({ pager, next, loading }: { pager: ReturnType<typeof useHistoryCursor>; next?: string; loading: boolean }) {
  return <div className="form-actions" aria-label="历史记录分页">
    <Button variant="secondary" disabled={pager.page === 1 || loading} onClick={pager.previous}>上一页</Button>
    <span>第 {pager.page} 页</span>
    <Button variant="secondary" disabled={!next || loading} onClick={() => next && pager.next(next)}>下一页</Button>
  </div>;
}

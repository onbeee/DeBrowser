import { useEffect, useMemo, useRef, useState } from "react";
import { Check, ChevronDown, Search } from "lucide-react";
import type { NodeView } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";

// 代理节点选择器：代理配置页与浏览器实例页共用的同一个
// 组件。菜单顶部是固定搜索框（按节点名不区分大小写部分
// 匹配），下面是固定的「未设置」，再下面是可滚动的节点
// 列表（最大高度固定，节点再多也不会无限拉长）。菜单中
// 只显示节点名称——不显示代理源名称或任何分组信息；两个
// 来源有同名节点时按节点各自成行，选择即绑定对应节点。
export default function ProxyNodePicker({
  nodes,
  value,
  onChange,
  disabled,
  ariaLabel,
}: {
  nodes: NodeView[];
  // 当前绑定的节点 ID；"" 表示未设置。
  value: string;
  onChange: (nodeId: string) => void;
  disabled?: boolean;
  ariaLabel?: string;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const rootRef = useRef<HTMLDivElement>(null);

  const selected = nodes.find((n) => n.id === value);
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return nodes;
    return nodes.filter((n) => n.name.toLowerCase().includes(q));
  }, [nodes, query]);

  // 点击组件外或按 Escape 关闭菜单。
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (rootRef.current && !rootRef.current.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open ]);

  const choose = (nodeId: string) => {
    setOpen(false);
    setQuery("");
    onChange(nodeId);
  };

  const rowCls =
    "flex w-full items-center gap-2 px-3 py-2 text-left text-sm text-(--text-main) transition hover:bg-(--hover-bg)";

  return (
    <div ref={rootRef} className="relative w-full">
      <button
        type="button"
        disabled={disabled}
        aria-label={ariaLabel}
        className="flex w-full items-center gap-2 rounded-md border border-(--border) bg-(--input-bg) px-2.5 py-1.5 text-left text-sm text-(--text-main) outline-none transition hover:bg-(--hover-bg) disabled:opacity-50"
        onClick={() => setOpen((v) => !v)}
      >
        <span className="min-w-0 flex-1 truncate">{selected ? selected.name : "未设置"}</span>
        <ChevronDown size={14} className="shrink-0 text-(--text-faint)" />
      </button>
      {open && (
        <div className="absolute right-0 z-30 mt-1 w-64 max-w-[80vw] overflow-hidden rounded-md border border-(--border-strong) bg-(--panel-bg) shadow-xl">
          <div className="border-b border-(--border) p-2">
            <div className="relative">
              <Search
                size={14}
                className="pointer-events-none absolute left-2 top-1/2 -translate-y-1/2 text-(--text-faint)"
              />
              <input
                autoFocus
                className="w-full rounded-md border border-(--border) bg-(--input-bg) py-1.5 pl-7 pr-2 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)"
                placeholder="搜索节点名称"
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
            </div>
          </div>
          <button type="button" className={rowCls} onClick={() => choose("")}>
            <span className="min-w-0 flex-1 truncate">未设置</span>
            {value === "" && <Check size={14} className="shrink-0 text-(--accent)" />}
          </button>
          <div className="max-h-60 overflow-y-auto border-t border-(--border) py-1">
            {filtered.length === 0 ? (
              <p className="px-3 py-2 text-sm text-(--text-faint)">无匹配结果</p>
            ) : (
              filtered.map((n) => (
                <button key={n.id} type="button" className={rowCls} onClick={() => choose(n.id)}>
                  <span className="min-w-0 flex-1 truncate">{n.name}</span>
                  {value === n.id && <Check size={14} className="shrink-0 text-(--accent)" />}
                </button>
              ))
            )}
          </div>
        </div>
      )}
    </div>
  );
}

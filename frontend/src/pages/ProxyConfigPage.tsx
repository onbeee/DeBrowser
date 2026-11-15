import { Network } from "lucide-react";

/**
 * 代理配置入口（产品负责人决定）：当前仅保留占位页面。
 * 代理服务器管理、订阅导入与 Mihomo 核心均不属于当前实现范围，
 * 本页面不调用任何代理服务，也不展示节点、代理组或配置数据。
 */
export default function ProxyConfigPage() {
  return (
    <div className="flex h-full flex-col items-center justify-center px-8 text-center">
      <span className="flex h-14 w-14 items-center justify-center rounded-2xl bg-(--hover-bg) text-(--text-faint)">
        <Network size={26} />
      </span>
      <p className="mt-4 max-w-md text-base leading-relaxed text-(--text-dim)">
        代理配置功能暂未开放
      </p>
    </div>
  );
}

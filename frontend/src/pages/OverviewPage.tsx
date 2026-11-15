import { CheckCircle2, Database, FolderCog, MonitorCog, Timer } from "lucide-react";
import { useAppStore } from "../stores/useAppStore";

function formatUptime(totalSeconds: number): string {
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes <= 0) return `${seconds} 秒`;
  return `${minutes} 分 ${seconds} 秒`;
}

export default function OverviewPage() {
  const status = useAppStore((state) => state.status);
  const statusError = useAppStore((state) => state.statusError);
  const dbOk = status?.database === "ok";

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-8">
      <div>
        <h2 className="text-xl font-semibold">基础架构运行中</h2>
        <p className="mt-1.5 max-w-2xl text-sm leading-relaxed text-(--text-dim)">
          当前版本已交付：应用基础架构与便携式目录、SQLite 数据层、实例元数据管理与 profile
          管理基础（Stage 3）、结构化日志、操作审计与诊断导出（Stage 5B，现统一归入「数据管理」），以及应用状态实时连接。代理配置功能暂未开放；当前开发重点是浏览器内核管理与实例真实启动（Stage
          9）。尚未交付的功能这里不展示任何占位数据。
        </p>
      </div>

      {statusError && (
        <div className="rounded-lg border border-(--danger-border) bg-(--danger-bg) px-4 py-3 text-sm text-(--danger)">
          无法连接 Go 后端：{statusError}
        </div>
      )}

      <div className="grid grid-cols-2 gap-4">
        <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
          <div className="flex items-center gap-2 text-xs tracking-wider text-(--text-faint)">
            <CheckCircle2 size={14} /> 应用
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? `${status.name} v${status.version}` : "加载中…"}
          </div>
          <div className="mt-0.5 text-xs text-(--text-faint)">Wails v3 · Go 后端 · React 前端</div>
        </div>

        <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
          <div className="flex items-center gap-2 text-xs tracking-wider text-(--text-faint)">
            <MonitorCog size={14} /> 平台
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? `${status.platform} / ${status.arch}` : "加载中…"}
          </div>
          <div className="mt-0.5 text-xs text-(--text-faint)">目标平台：Windows 11 x64</div>
        </div>

        <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
          <div className="flex items-center gap-2 text-xs tracking-wider text-(--text-faint)">
            <Database size={14} /> 数据库
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? (dbOk ? "SQLite · 健康" : "SQLite · 异常") : "加载中…"}
          </div>
          <div className="mt-0.5 text-xs text-(--text-faint)">
            {status
              ? dbOk
                ? "WAL 模式 · 外键已启用 · 迁移已应用"
                : `状态异常：${status.database}`
              : "正在检查…"}
          </div>
        </div>

        <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
          <div className="flex items-center gap-2 text-xs tracking-wider text-(--text-faint)">
            <Timer size={14} /> 本次会话
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? `已运行 ${formatUptime(status.uptimeSeconds)}` : "加载中…"}
          </div>
          <div className="mt-0.5 flex items-center gap-1.5 text-xs text-(--text-faint)">
            <FolderCog size={12} />
            <span className="truncate" title={status?.dataDir}>
              {status ? status.dataDir : "正在解析数据目录…"}
            </span>
          </div>
        </div>
      </div>

      <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
        <h3 className="text-sm font-semibold">当前已包含</h3>
        <ul className="mt-2 list-inside list-disc space-y-1 text-sm leading-relaxed text-(--text-dim)">
          <li>
            模块化单体结构：Go 后端位于 <code className="text-(--text-main)">internal/</code>，React
            前端位于 <code className="text-(--text-main)">frontend/</code>
          </li>
          <li>便携式目录架构：全部数据跟随可执行文件所在目录，不写入 %APPDATA%</li>
          <li>配置与目录统一管理、结构化 JSON 日志</li>
          <li>带迁移机制的 SQLite 数据层（实例、书签等业务表；历史数据表保持兼容）</li>
          <li>实例元数据管理与 profile 管理基础（Stage 3）</li>
          <li>凭据 Vault 底层兼容：既有凭据文件与引用保持不变，不作为独立产品功能开放</li>
          <li>日志、操作审计与诊断统一归入「数据管理」</li>
          <li>侧边栏固定八项；「代理配置」为暂未开放占位页面</li>
          <li>
            类型化的 Wails 绑定与 <code className="text-(--text-main)">app:status</code> 事件通道
          </li>
          <li>Windows x64 CI：格式、测试、vet、类型检查、前端与桌面应用构建</li>
        </ul>
      </div>
    </div>
  );
}

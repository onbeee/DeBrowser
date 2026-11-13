import { useState } from "react";
import { ArrowLeft, FolderInput, Loader2, UserPlus } from "lucide-react";
import { WelcomeService } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";

// 首次启动选择页（Stage 5A）。本页运行在欢迎模式：正式数据目录尚
// 不存在，后端只注册了 WelcomeService。「导入数据」只显示 Stage 5B
// 提示，不打开文件选择器，也不创建任何数据。
type View = "choose" | "import" | "busy" | "failed" | "done";

export default function WelcomeApp() {
  const [view, setView] = useState<View>("choose");
  const [error, setError] = useState("");

  const startNewUser = async () => {
    setView("busy");
    setError("");
    try {
      const message = await WelcomeService.StartNewUser();
      if (message) {
        setError(message);
        setView("failed");
      } else {
        // 初始化成功：后端会启动正式主程序并关闭本窗口。若窗口未
        // 关闭（例如正式进程启动失败），完成视图会给出手动指引。
        setView("done");
      }
    } catch (e) {
      setError(`初始化调用失败：${String(e)}`);
      setView("failed");
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-(--app-bg) p-8 text-(--text-main)">
      <div className="w-full max-w-md">
        <div className="mb-8 text-center">
          <h1 className="text-2xl font-semibold">欢迎使用 DeBrowser</h1>
          <p className="mt-2 text-sm text-(--text-dim)">
            第一次使用，请选择如何开始。
          </p>
        </div>

        {view === "choose" && (
          <div className="space-y-3">
            <button
              type="button"
              onClick={startNewUser}
              className="flex w-full items-center gap-4 rounded-lg border border-(--border) bg-(--panel-bg) p-4 text-left transition-colors hover:bg-(--hover-bg)"
            >
              <UserPlus className="h-6 w-6 shrink-0 text-(--accent)" />
              <span>
                <span className="block font-medium">新用户</span>
                <span className="mt-0.5 block text-sm text-(--text-dim)">
                  在这台电脑上从零开始，创建全新的数据目录。
                </span>
              </span>
            </button>
            <button
              type="button"
              onClick={() => setView("import")}
              className="flex w-full items-center gap-4 rounded-lg border border-(--border) bg-(--panel-bg) p-4 text-left transition-colors hover:bg-(--hover-bg)"
            >
              <FolderInput className="h-6 w-6 shrink-0 text-(--accent)" />
              <span>
                <span className="block font-medium">导入数据</span>
                <span className="mt-0.5 block text-sm text-(--text-dim)">
                  从备份文件恢复已有的数据。
                </span>
              </span>
            </button>
          </div>
        )}

        {view === "import" && (
          <div>
            <div className="rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3 text-sm leading-relaxed">
              当前版本尚未开放备份导入功能，相关能力将在 Stage 5B 实现。
            </div>
            <button
              type="button"
              onClick={() => setView("choose")}
              className="mt-4 inline-flex items-center gap-1.5 rounded-md border border-(--border) bg-(--panel-bg) px-3 py-1.5 text-sm transition-colors hover:bg-(--hover-bg)"
            >
              <ArrowLeft className="h-4 w-4" />
              返回
            </button>
          </div>
        )}

        {view === "busy" && (
          <div className="flex items-center justify-center gap-3 rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-6 text-sm text-(--text-dim)">
            <Loader2 className="h-5 w-5 animate-spin" />
            正在初始化，请稍候…
          </div>
        )}

        {view === "failed" && (
          <div>
            <div className="rounded-lg border border-(--danger-border) bg-(--danger-bg) px-4 py-3 text-sm leading-relaxed text-(--danger)">
              {error}
            </div>
            <div className="mt-4 flex gap-2">
              <button
                type="button"
                onClick={startNewUser}
                className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-(--accent-contrast) transition-opacity hover:opacity-90"
              >
                重新尝试
              </button>
              <button
                type="button"
                onClick={() => setView("choose")}
                className="inline-flex items-center gap-1.5 rounded-md border border-(--border) bg-(--panel-bg) px-3 py-1.5 text-sm transition-colors hover:bg-(--hover-bg)"
              >
                <ArrowLeft className="h-4 w-4" />
                返回选择
              </button>
            </div>
          </div>
        )}

        {view === "done" && (
          <div className="rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-6 text-center text-sm leading-relaxed text-(--text-dim)">
            初始化完成，正在打开主界面…
            <span className="mt-2 block text-(--text-faint)">
              若主界面没有打开，请关闭本窗口后重新双击 DeBrowser.exe。
            </span>
          </div>
        )}
      </div>
    </div>
  );
}

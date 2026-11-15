import {
  DiagnosticsService,
  type ClientEventInput,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";

/**
 * 统一记录的前端接入：每次用户操作生成一个 operation_id，
 * 随业务调用一起传给后端，使前端请求、后端步骤、执行结果与
 * 用户看到的提示共用同一个关联 ID。记录类调用一律静默失败——
 * 记录系统的问题不能影响正常业务操作。
 */
export function newOperationId(): string {
  const c = globalThis.crypto as Crypto | undefined;
  if (c && typeof c.randomUUID === "function") {
    return c.randomUUID();
  }
  return `op-${Date.now().toString(36)}-${Math.floor(Math.random() * 1e9).toString(36)}`;
}

function send(input: ClientEventInput): void {
  DiagnosticsService.RecordClientEvent(input).catch(() => {
    // 记录失败不打扰用户，也不影响业务。
  });
}

/** 记录一次操作在界面上的最终反馈（用户实际看到的提示）。 */
export function recordOperationFeedback(
  operationId: string,
  ok: boolean,
  userMessage: string,
  module: string,
  action: string,
): void {
  send({
    category: "operation",
    eventType: "client_result",
    module,
    action,
    targetType: "",
    targetId: "",
    operationId,
    status: ok ? "success" : "failed",
    userMessage,
    metadata: {},
  });
}

/** 记录页面打开/离开（运行记录）。 */
export function recordPageNavigation(action: "打开页面" | "离开页面", page: string): void {
  send({
    category: "runtime",
    eventType: "client",
    module: "界面",
    action,
    targetType: "page",
    targetId: page,
    operationId: "",
    status: "info",
    userMessage: "",
    metadata: { page },
  });
}

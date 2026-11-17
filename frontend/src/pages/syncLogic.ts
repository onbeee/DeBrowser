// 多开同步页面的纯逻辑助手：参与选择校验与状态文案。
// 与页面组件分离，便于单元测试（tests/syncLogic.test.ts）。

export interface SyncInstanceLike {
  id: string;
  status: string;
  cdpAvailable: boolean;
  cdpReason: string;
}

// startBlockReason 返回阻止开始同步的原因（可开始时返回 null）。
// 规则与后端 StartSync 的前置检查一致：至少两个参与实例、必须
// 指定主实例、每个参与实例都已确认运行且可建立 CDP 连接。
export function startBlockReason(
  selected: ReadonlySet<string>,
  masterId: string,
  instances: SyncInstanceLike[],
): string | null {
  if (selected.size < 2) {
    return "至少需要选择两个实例才能开始同步";
  }
  if (!masterId || !selected.has(masterId)) {
    return "请从参与实例中指定一个主实例";
  }
  const byId = new Map(instances.map((i) => [i.id, i]));
  for (const id of selected) {
    const inst = byId.get(id);
    if (!inst) {
      return "所选实例不存在";
    }
    if (inst.status !== "running") {
      return "所有参与实例必须处于已确认运行状态";
    }
    if (!inst.cdpAvailable) {
      return inst.cdpReason || "所选实例暂不支持同步";
    }
  }
  return null;
}

// followerIds 返回除主实例外的参与实例 id 列表（保持选择顺序
// 不重要，后端按集合处理）。
export function followerIds(
  selected: ReadonlySet<string>,
  masterId: string,
): string[] {
  return [...selected].filter((id) => id !== masterId);
}

// roleText 返回实例在当前会话中的角色文案。
export function roleText(role: string, sessionState: string): string {
  if (sessionState === "idle" || role === "") {
    return "未参与";
  }
  if (role === "master") {
    return "主实例";
  }
  return sessionState === "paused" ? "已暂停" : "跟随中";
}

// sessionStateText 返回会话状态文案。
export function sessionStateText(state: string): string {
  switch (state) {
    case "active":
      return "同步中";
    case "paused":
      return "已暂停";
    default:
      return "未开始";
  }
}

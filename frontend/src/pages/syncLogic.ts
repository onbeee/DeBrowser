// Pure helpers for the Stage 10 v2 sync page. Kept free of React
// and bindings so they can be unit-tested directly.

export interface SyncOptionLike {
  id: string;
  running: boolean;
  cdpSupported: boolean;
}

export interface FollowerStateLike {
  id: string;
  status: string; // "active" | "failed"
  reason?: string;
}

/** Instances that can take part in a sync session. */
export function eligibleInstances<T extends SyncOptionLike>(options: T[]): T[] {
  return options.filter((o) => o.running && o.cdpSupported);
}

/** Instances that run but cannot take part (old launch, no CDP). */
export function unsupportedInstances<T extends SyncOptionLike>(options: T[]): T[] {
  return options.filter((o) => o.running && !o.cdpSupported);
}

/** Start is possible with a master and at least one follower. */
export function canStart(masterId: string, followerIds: string[]): boolean {
  return masterId !== "" && followerIds.length > 0 && !followerIds.includes(masterId);
}

/** Toggle one follower in the selection, keeping pick order. */
export function toggleFollower(selected: string[], id: string, masterId: string): string[] {
  if (id === masterId) return selected;
  return selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id];
}

/** One-line summary of follower health for the session header. */
export function followerSummary(followers: FollowerStateLike[]): string {
  if (followers.length === 0) return "";
  const failed = followers.filter((f) => f.status === "failed").length;
  const suspended = followers.filter((f) => f.status === "suspended").length;
  const active = followers.length - failed - suspended;
  if (failed === 0 && suspended === 0) return `全部 ${followers.length} 个跟随实例正常`;
  const parts = [`${active} 个正常`];
  if (suspended > 0) parts.push(`${suspended} 个等待标签映射`);
  if (failed > 0) parts.push(`${failed} 个失败`);
  return parts.join(" · ");
}

/** Support levels shown on the page. These describe the actual
 * mechanisms in use — semantic operations are reported as what
 * they are, and anything without a reliable channel is named as
 * unsupported instead of being simulated. */
export interface SupportLevel {
  level: "reliable" | "conditional" | "unsupported";
  label: string;
  detail: string;
}

export function supportLevels(): SupportLevel[] {
  return [
    {
      level: "reliable",
      label: "网页内容操作",
      detail: "点击、移动、滚动、拖动、键盘与快捷键通过各实例自己的调试连接执行，跟随实例互不影响。",
    },
    {
      level: "reliable",
      label: "标签与页面导航",
      detail: "新建、关闭标签与页面导航（含地址栏提交后的导航）通过浏览器语义命令执行；前进、后退按各实例自己的历史记录执行。",
    },
    {
      level: "reliable",
      label: "窗口行为与排列",
      detail: "移动、缩放、最大化、最小化、还原与自动排列使用 Windows 原生窗口管理，不抢占焦点、不移动真实鼠标。",
    },
    {
      level: "conditional",
      label: "标签切换跟随",
      detail: "依赖浏览器通过 UI Automation 暴露标签栏，且标签标题能与调试目标一一对应（标题重复时无法可靠对应）；无法确认对应关系时不执行切换，也不会激活可能错误的标签，新建与关闭仍可同步。",
    },
    {
      level: "conditional",
      label: "浏览器菜单打开与关闭",
      detail: "主实例菜单的打开、关闭状态经 UI Automation 语义命令在跟随实例上执行，依赖菜单按钮实际暴露展开状态，逐实例反馈执行结果；菜单项的点击本身无法可靠捕获，其产生的效果（新建标签、导航、窗口变化）经各自通道同步。",
    },
    {
      level: "conditional",
      label: "书签栏、扩展弹窗、右键菜单与对话框",
      detail: "依赖具体浏览器与版本实际暴露的 UI Automation 控件；无法确认对应控件的操作不会执行，也不会用模拟输入代替。书签栏点击产生的页面导航已由导航通道同步。",
    },
    {
      level: "unsupported",
      label: "中文输入法组合输入",
      detail: "网页内的拼音等组合输入过程无法以按键事件可靠传递；地址栏输入由各浏览器自身的输入法处理，不会被同步到其他实例的网页中。",
    },
  ];
}

/** Capability summary line for one instance's UIA probe. */
export function capsSummary(caps: {
  available: boolean;
  addressBar: boolean;
  tabStrip: boolean;
  menuButton: boolean;
}): string {
  if (!caps.available) return "界面控件检测不可用";
  const parts: string[] = [];
  parts.push(caps.tabStrip ? "标签栏可检测" : "标签栏不可检测");
  parts.push(caps.addressBar ? "地址栏可识别" : "地址栏不可识别");
  parts.push(caps.menuButton ? "菜单按钮可识别" : "菜单按钮不可识别");
  return parts.join(" · ");
}

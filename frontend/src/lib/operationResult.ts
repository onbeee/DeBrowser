// 批量 / 多实例操作的结果分类：把真实执行计数统一映射为
// 通知类型，保证用户看到的结果与实际执行一致。
//
// 规则（全局统一）：
// - 全部成功 → success
// - 部分成功、部分失败 → warning
// - 全部失败（实际执行的项目无一成功） → error
// - 全部跳过、没有实际执行 → info
// - 没有任何项目 → null（不产生通知）
import type { NotificationKind } from "./notifications.ts";

/** 按成功 / 失败 / 跳过计数分类。total 为 0 时返回 null。 */
export function classifyCounts(
  success: number,
  failed: number,
  skipped: number,
): NotificationKind | null {
  const total = success + failed + skipped;
  if (total <= 0) return null;
  if (failed === 0) return success > 0 ? "success" : "info";
  if (success === 0 && skipped === 0) return "error";
  return "warning";
}

/**
 * 书签同步结果分类：必须同时看实例与书签两个口径——
 * 实例成功但书签写入失败（failedBookmarks > 0）不是成功。
 * 成功证据 = 成功实例数 + 新增书签数；失败证据 = 失败
 * 实例数 + 失败书签数。
 */
export function classifySyncResult(r: {
  successInstances: number;
  failedInstances: number;
  skippedInstances: number;
  addedBookmarks: number;
  skippedBookmarks: number;
  failedBookmarks: number;
}): NotificationKind {
  const failed = r.failedInstances + r.failedBookmarks;
  const succeeded = r.successInstances + r.addedBookmarks;
  if (failed === 0) return succeeded > 0 ? "success" : "info";
  return succeeded > 0 ? "warning" : "error";
}

/**
 * 书签聚合应用（编辑 / 删除）结果分类：updatedInstances 与
 * affectedNodes 是成功口径，failedInstances 与 failedNodes
 * 是失败口径，skippedInstances 是跳过口径。三者皆为 0
 * （没有执行任何项目）时返回 null，不产生通知。
 */
export function classifyApplyResult(r: {
  updatedInstances: number;
  failedInstances: number;
  skippedInstances: number;
  affectedNodes: number;
  failedNodes: number;
}): NotificationKind | null {
  const failed = r.failedInstances + r.failedNodes;
  const succeeded = r.updatedInstances + r.affectedNodes;
  const total = succeeded + failed + r.skippedInstances;
  if (total <= 0) return null;
  if (failed === 0) return succeeded > 0 ? "success" : "info";
  return succeeded > 0 ? "warning" : "error";
}

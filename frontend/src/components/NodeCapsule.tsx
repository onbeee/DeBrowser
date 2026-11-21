// NodeCapsule is THE single visual rule for showing a proxy
// node's name together with its health state, shared by the
// instance list, the instance create/edit form and the proxy
// page's binding overview: the node name sits in a rounded
// capsule whose colors carry the state — green healthy, red
// present-but-failing, yellow gone from its source, neutral
// not-yet-checked. The capsule contains the name only; there
// is no separate status text or icon anywhere.
//
// Colors come from the theme tokens (--ok / --danger / --warn
// and their backgrounds), so all three themes stay readable.

export type NodeHealthState = "ok" | "down" | "missing" | "unknown";

const CAPSULE_CLS: Record<NodeHealthState, string> = {
  ok: "bg-(--ok-bg) text-(--ok)",
  down: "bg-(--danger-bg) text-(--danger)",
  missing: "bg-(--warn-bg) text-(--warn)",
  unknown: "bg-(--badge-bg) text-(--text-dim)",
};

// asNodeState normalizes a backend state string for display.
export function asNodeState(state: string): NodeHealthState {
  return state === "ok" || state === "down" || state === "missing" ? state : "unknown";
}

// nodeStateOfNode derives the display state of one node-list
// entry from the core's health data: unchecked is neutral,
// never green.
export function nodeStateOfNode(node: { alive: boolean; checked: boolean }): NodeHealthState {
  if (!node.checked) return "unknown";
  return node.alive ? "ok" : "down";
}

export function NodeCapsule({ name, state }: { name: string; state: NodeHealthState }) {
  return (
    <span
      title={name}
      className={`inline-block max-w-44 truncate rounded-full px-2.5 py-0.5 align-middle text-xs font-medium ${CAPSULE_CLS[state]}`}
    >
      {name}
    </span>
  );
}

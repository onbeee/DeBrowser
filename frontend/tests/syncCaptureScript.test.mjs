// Executes the REAL observer script from
// internal/capture/script.go in a vm with a minimal fake DOM
// and asserts the event contract the Go side consumes.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const goSource = readFileSync(
  new URL("../../internal/capture/script.go", import.meta.url),
  "utf8",
);
const match = goSource.match(/const Script = `([\s\S]*?)`/);
assert.ok(match, "Script constant not found in script.go");
const scriptText = match[1];

function loadScript({ top = true } = {}) {
  const listeners = {};
  const reported = [];
  const win = {
    top: null,
    self: null,
    addEventListener: (type, fn) => {
      (listeners[type] ??= []).push(fn);
    },
    __debrowserSyncReport: (json) => reported.push(JSON.parse(json)),
  };
  win.self = win;
  win.top = top ? win : {};
  const doc = {
    visibilityState: "visible",
    addEventListener: (type, fn) => {
      (listeners["doc:" + type] ??= []).push(fn);
    },
  };
  const context = vm.createContext({ window: win, document: doc });
  vm.runInContext(scriptText, context);
  reported.length = 0; // drain the install-time visibility report
  const fire = (type, ev) => {
    for (const fn of listeners[type] ?? []) fn(ev);
  };
  return { reported, fire };
}

const keyEv = (over = {}) => ({
  key: "a",
  code: "KeyA",
  location: 0,
  repeat: false,
  isComposing: false,
  ctrlKey: false,
  altKey: false,
  shiftKey: false,
  metaKey: false,
  ...over,
});

test("ordinary keys are reported with modifiers", () => {
  const { reported, fire } = loadScript();
  fire("keydown", keyEv({ key: "A", shiftKey: true }));
  assert.equal(reported.length, 1);
  assert.equal(reported[0].kind, "keydown");
  assert.equal(reported[0].shift, true);
  assert.equal(reported[0].key, "A");
});

test("dead keys are not reported as keys", () => {
  const { reported, fire } = loadScript();
  fire("keydown", keyEv({ key: "Dead", code: "Quote" }));
  assert.equal(reported.length, 0);
});

test("the script never reads any clipboard; Ctrl+V is an ordinary key event", () => {
  assert.ok(!goSource.includes("clipboardData"), "script must not touch clipboardData");
  assert.ok(!goSource.includes("insertfrompaste"), "no paste text events exist");
  const { reported, fire } = loadScript();
  fire("keydown", keyEv({ key: "v", code: "KeyV", ctrlKey: true }));
  fire("keyup", keyEv({ key: "v", code: "KeyV", ctrlKey: true }));
  assert.deepEqual(reported.map((e) => e.kind), ["keydown", "keyup"],
    "paste shortcut travels as plain keys; the follower page's own clipboard decides");
});

test("input increments and composition results are reported; composing input is not", () => {
  const { reported, fire } = loadScript();
  fire("input", { isComposing: false, inputType: "insertText", data: "x" });
  fire("input", { isComposing: true, inputType: "insertText", data: "拼" });
  fire("compositionend", { data: "拼音" });
  assert.deepEqual(
    reported.map((r) => [r.kind, r.text]),
    [
      ["input", "x"],
      ["compositionend", "拼音"],
    ],
  );
});

test("pointer and wheel events carry viewport coordinates", () => {
  const { reported, fire } = loadScript();
  fire("pointerdown", { clientX: 10, clientY: 20, button: 0, buttons: 1 });
  fire("wheel", { clientX: 5, clientY: 6, deltaX: 0, deltaY: 120 });
  assert.equal(reported[0].kind, "pointerdown");
  assert.equal(reported[0].x, 10);
  assert.equal(reported[1].kind, "wheel");
  assert.equal(reported[1].deltaY, 120);
});

test("subframes never install the observer", () => {
  const { reported, fire } = loadScript({ top: false });
  fire("keydown", keyEv());
  fire("pointerdown", { clientX: 1, clientY: 2, button: 0, buttons: 1 });
  assert.equal(reported.length, 0);
});

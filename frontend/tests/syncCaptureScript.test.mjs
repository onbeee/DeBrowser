// Unit tests for the Stage 10 page observer script
// (internal/service/sync_capture_script.js), executed in a Node
// vm against a minimal fake DOM. These are SCRIPT unit tests:
// they prove the script's event filtering, top-level-only and
// text-reporting rules, but they are NOT a real browser test —
// real Chromium behaviour (CDP key-event semantics, IME) remains
// a Windows 11 manual acceptance item.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import vm from "node:vm";

const source = readFileSync(
  new URL("../../internal/service/sync_capture_script.js", import.meta.url),
  "utf8",
);

function makeWorld({ top = true } = {}) {
  const reports = [];
  const listeners = { window: {}, document: {} };
  const win = {
    addEventListener(type, fn) {
      (listeners.window[type] ??= []).push(fn);
    },
  };
  win.self = win;
  win.top = top ? win : { notSelf: true };
  const doc = {
    visibilityState: "visible",
    addEventListener(type, fn) {
      (listeners.document[type] ??= []).push(fn);
    },
  };
  win.__debrowserSyncReport = (payload) => reports.push(JSON.parse(payload));
  const sandbox = {
    window: win,
    document: doc,
    location: { href: "https://a.example/" },
    setTimeout: () => 0,
  };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  const fire = (target, type, ev) => {
    for (const fn of listeners[target][type] ?? []) fn(ev);
  };
  // Drop the install-time visibility report; tests start clean.
  reports.length = 0;
  return { reports, fire, win };
}

const keyEv = (over = {}) => ({
  key: "a",
  code: "KeyA",
  location: 0,
  repeat: false,
  isComposing: false,
  altKey: false,
  ctrlKey: false,
  metaKey: false,
  shiftKey: false,
  ...over,
});

test("plain character and space keys are reported as key events", () => {
  const { reports, fire } = makeWorld();
  fire("window", "keydown", keyEv());
  fire("window", "keyup", keyEv());
  fire("window", "keydown", keyEv({ key: " ", code: "Space" }));
  const keys = reports.filter((r) => r.kind === "keydown" || r.kind === "keyup");
  assert.equal(keys.length, 3);
  assert.equal(keys[0].key, "a");
  assert.equal(keys[0].code, "KeyA");
  assert.equal(keys[1].kind, "keyup");
  assert.equal(keys[2].key, " ");
  assert.equal(keys[2].code, "Space");
});

test("key events keep real key semantics: modifiers, location, repeat", () => {
  const { reports, fire } = makeWorld();
  fire("window", "keydown", keyEv({ key: "A", shiftKey: true, repeat: true, location: 1 }));
  fire("window", "keydown", keyEv({ key: "F5", code: "F5" }));
  assert.equal(reports[0].shift, true);
  assert.equal(reports[0].repeat, true);
  assert.equal(reports[0].location, 1);
  assert.equal(reports[1].key, "F5");
});

test("typed text arrives once as a text increment, beside its key event", () => {
  const { reports, fire } = makeWorld();
  fire("window", "keydown", keyEv());
  fire("window", "input", { inputType: "insertText", data: "a", isComposing: false });
  const texts = reports.filter((r) => r.kind === "text");
  assert.equal(texts.length, 1);
  assert.equal(texts[0].text, "a");
  assert.equal(texts[0].source, "input");
});

test("Ctrl+V and Shift+Insert are not reported; paste follows the text path only", () => {
  const { reports, fire } = makeWorld();
  fire("window", "keydown", keyEv({ key: "v", code: "KeyV", ctrlKey: true }));
  fire("window", "keyup", keyEv({ key: "v", code: "KeyV", ctrlKey: true }));
  fire("window", "keydown", keyEv({ key: "Insert", code: "Insert", shiftKey: true }));
  assert.equal(reports.filter((r) => r.kind === "keydown" || r.kind === "keyup").length, 0);
  fire("window", "input", { inputType: "insertFromPaste", data: "粘贴", isComposing: false });
  const texts = reports.filter((r) => r.kind === "text");
  assert.equal(texts.length, 1);
  assert.equal(texts[0].source, "paste");
  // Other shortcuts are still key events.
  fire("window", "keydown", keyEv({ key: "a", code: "KeyA", ctrlKey: true }));
  assert.equal(reports.filter((r) => r.kind === "keydown").length, 1);
});

test("composition commit is reported once; its twin input event is suppressed", () => {
  const { reports, fire } = makeWorld();
  fire("window", "input", { inputType: "insertCompositionText", data: "你", isComposing: true });
  fire("window", "compositionend", { data: "你好" });
  fire("window", "input", { inputType: "insertText", data: "你好", isComposing: false });
  const texts = reports.filter((r) => r.kind === "text");
  assert.equal(texts.length, 1);
  assert.equal(texts[0].text, "你好");
  assert.equal(texts[0].source, "composition");
  // A fresh keystroke ends the twin window: the same text typed
  // again afterwards is genuine input and is reported.
  fire("window", "keydown", keyEv());
  fire("window", "input", { inputType: "insertText", data: "你好", isComposing: false });
  assert.equal(reports.filter((r) => r.kind === "text").length, 2);
});

test("pointer and wheel events are reported with viewport coordinates", () => {
  const { reports, fire } = makeWorld();
  fire("window", "pointerdown", { clientX: 10, clientY: 20, button: 0, buttons: 1, detail: 1 });
  fire("window", "wheel", { clientX: 10, clientY: 20, deltaX: 0, deltaY: 120 });
  assert.equal(reports[0].kind, "pointerdown");
  assert.equal(reports[0].x, 10);
  assert.equal(reports[1].kind, "wheel");
  assert.equal(reports[1].deltaY, 120);
});

test("in a subframe the script stays completely inert", () => {
  const { reports, fire, win } = makeWorld({ top: false });
  assert.equal(win.__debrowserSyncInstalled, undefined);
  fire("window", "keydown", keyEv());
  fire("window", "input", { inputType: "insertText", data: "x", isComposing: false });
  fire("window", "pointerdown", { clientX: 1, clientY: 2, button: 0, buttons: 1, detail: 1 });
  assert.equal(reports.length, 0);
});

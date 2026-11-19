// DeBrowser Stage 10 page observer (isolated world "debrowser-sync").
//
// Installed on the TOP-LEVEL page of a synced browser through CDP
// (Page.addScriptToEvaluateOnNewDocument for future documents,
// Runtime.evaluate in an isolated world for the current one). It
// only OBSERVES input and reports it through the CDP Runtime
// binding; it never calls preventDefault, never stops propagation,
// and never reads form values, page text, cookies, storage or
// history. Text is reported only as the increment the user just
// committed (an input event's data, or an IME composition's final
// result) — never as a field's full value.
//
// Iframes are out of scope for the first version: the script runs
// in a subframe's document too (addScriptToEvaluateOnNewDocument
// applies per frame), but there it stays completely inert — it
// installs no listeners and reports nothing, so iframe input is
// never mistaken for top-level page input.
(function () {
  "use strict";
  if (window.top !== window.self) return; // subframe: stay inert
  if (window.__debrowserSyncInstalled) return;
  window.__debrowserSyncInstalled = true;

  var BINDING = "__debrowserSyncReport";
  function report(obj) {
    try {
      var fn = window[BINDING];
      if (typeof fn === "function") fn(JSON.stringify(obj));
    } catch (e) {
      /* the binding may not be attached yet; events are dropped */
    }
  }

  function mods(e) {
    return {
      alt: !!e.altKey,
      ctrl: !!e.ctrlKey,
      meta: !!e.metaKey,
      shift: !!e.shiftKey,
    };
  }

  // --- visibility: how the sync session learns which page target
  // is the active tab of this browser window ---
  function reportVisibility() {
    report({
      kind: "visibility",
      state: document.visibilityState,
      href: location.href,
    });
  }
  document.addEventListener("visibilitychange", reportVisibility, true);
  reportVisibility();
  // The binding is attached to this isolated context a moment
  // after the script first runs on a new document; re-report once
  // shortly after install so the initial state is not lost.
  setTimeout(reportVisibility, 250);
  window.addEventListener("pageshow", reportVisibility, true);

  // --- pointer ---
  function pointer(kind, e) {
    var m = mods(e);
    report({
      kind: kind,
      x: e.clientX,
      y: e.clientY,
      button: e.button,
      buttons: e.buttons,
      detail: e.detail || 0,
      alt: m.alt,
      ctrl: m.ctrl,
      meta: m.meta,
      shift: m.shift,
    });
  }
  window.addEventListener("pointerdown", function (e) { pointer("pointerdown", e); }, true);
  window.addEventListener("pointerup", function (e) { pointer("pointerup", e); }, true);
  window.addEventListener("pointermove", function (e) { pointer("pointermove", e); }, true);
  window.addEventListener("wheel", function (e) {
    var m = mods(e);
    report({
      kind: "wheel",
      x: e.clientX,
      y: e.clientY,
      deltaX: e.deltaX,
      deltaY: e.deltaY,
      alt: m.alt,
      ctrl: m.ctrl,
      meta: m.meta,
      shift: m.shift,
    });
  }, { capture: true, passive: true });

  // --- keyboard ---
  // EVERY keydown/keyup is reported with its real key, code,
  // location, repeat flag and modifiers — including plain
  // character keys, which games, canvas apps and page-level
  // shortcut handlers listen for. Reporting a key event cannot
  // duplicate text on followers: the dispatch side sends CDP key
  // events WITHOUT any text payload, so no character is inserted
  // by them; committed text travels separately as a text increment
  // (below). Two exceptions:
  //   * "Dead" keys are not reported: a dead key only arms an
  //     accent composition whose committed character arrives as a
  //     text increment.
  //   * The paste shortcuts (Ctrl/Cmd+V, Shift+Insert) are not
  //     reported as key events. Paste follows ONE path only: the
  //     insertFromPaste text increment. Sending both the native
  //     shortcut and the increment would paste twice. A paste
  //     that produces no input event (for example into a canvas)
  //     is therefore NOT synced in this version — it is an
  //     explicitly unsupported paste target, never guessed at.
  function isPasteShortcut(e) {
    if ((e.ctrlKey || e.metaKey) && (e.code === "KeyV" || e.key === "v" || e.key === "V")) {
      return true;
    }
    return e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey &&
      (e.code === "Insert" || e.key === "Insert");
  }
  function key(kind, e) {
    var m = mods(e);
    report({
      kind: kind,
      key: e.key,
      code: e.code,
      location: e.location || 0,
      repeat: !!e.repeat,
      composing: !!e.isComposing,
      alt: m.alt,
      ctrl: m.ctrl,
      meta: m.meta,
      shift: m.shift,
    });
  }
  window.addEventListener("keydown", function (e) {
    // A fresh keystroke ends any composition-twin window: text it
    // produces is genuine new input, never a composition twin.
    lastComposition = null;
    if (e.key === "Dead") return;
    if (isPasteShortcut(e)) return;
    key("keydown", e);
  }, true);
  window.addEventListener("keyup", function (e) {
    if (e.key === "Dead") return;
    if (isPasteShortcut(e)) return;
    key("keyup", e);
  }, true);

  // --- text increments ---
  // An IME composition commits its final text at compositionend;
  // some engines then ALSO fire an input event carrying the same
  // text. lastComposition remembers the just-committed text so
  // that immediate twin input event is reported only once (the
  // dispatch side applies the same next-event rule defensively).
  var lastComposition = null;
  window.addEventListener("input", function (e) {
    if (e.isComposing) return; // final text arrives at compositionend
    if (e.inputType !== "insertText" && e.inputType !== "insertFromPaste" &&
        e.inputType !== "insertFromDrop" && e.inputType !== "insertCompositionText") {
      return; // deletions and structure edits ride the key events
    }
    if (typeof e.data !== "string" || e.data === "") return;
    if (lastComposition !== null && e.data === lastComposition) {
      lastComposition = null;
      return; // twin of the compositionend report: send once only
    }
    lastComposition = null;
    report({
      kind: "text",
      text: e.data,
      source: e.inputType === "insertFromPaste" ? "paste" : "input",
    });
  }, true);
  window.addEventListener("compositionend", function (e) {
    if (typeof e.data === "string" && e.data !== "") {
      lastComposition = e.data;
      report({ kind: "text", text: e.data, source: "composition" });
    }
  }, true);
})();

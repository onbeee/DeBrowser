// DeBrowser Stage 10 page observer (isolated world "debrowser-sync").
//
// Installed on every page of a synced browser through CDP
// (Page.addScriptToEvaluateOnNewDocument for future documents,
// Runtime.evaluate in an isolated world for the current one). It
// only OBSERVES input and reports it through the CDP Runtime
// binding; it never calls preventDefault, never stops propagation,
// and never reads form values, page text, cookies, storage or
// history. Text is reported only as the increment the user just
// committed (an input event's data, or an IME composition's final
// result) — never as a field's full value.
(function () {
  "use strict";
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
  // A keydown that will produce text (a single printable character
  // with no command modifiers, outside IME composition) is NOT
  // reported: the text itself arrives via the input event, and
  // reporting both would duplicate the character on followers.
  // Everything else — shortcuts, Backspace, Delete, arrows, Enter,
  // Tab, function keys — is reported as a key event.
  function producesText(e) {
    if (e.isComposing) return true; // IME owns it; compositionend reports
    if (e.ctrlKey || e.metaKey || e.altKey) return false;
    return typeof e.key === "string" && e.key.length === 1;
  }
  function key(kind, e) {
    var m = mods(e);
    report({
      kind: kind,
      key: e.key,
      code: e.code,
      location: e.location || 0,
      repeat: !!e.repeat,
      alt: m.alt,
      ctrl: m.ctrl,
      meta: m.meta,
      shift: m.shift,
    });
  }
  window.addEventListener("keydown", function (e) {
    if (producesText(e)) return;
    if (e.key === "Dead") return;
    key("keydown", e);
  }, true);
  window.addEventListener("keyup", function (e) {
    if (producesText(e)) return;
    if (e.key === "Dead") return;
    key("keyup", e);
  }, true);

  // --- text increments ---
  window.addEventListener("input", function (e) {
    if (e.isComposing) return; // final text arrives at compositionend
    if (e.inputType !== "insertText" && e.inputType !== "insertFromPaste" &&
        e.inputType !== "insertFromDrop") {
      return; // deletions and structure edits ride the key events
    }
    if (typeof e.data === "string" && e.data !== "") {
      report({ kind: "text", text: e.data });
    }
  }, true);
  window.addEventListener("compositionend", function (e) {
    if (typeof e.data === "string" && e.data !== "") {
      report({ kind: "text", text: e.data });
    }
  }, true);
})();

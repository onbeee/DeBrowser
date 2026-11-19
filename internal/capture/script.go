// Package capture owns the in-page observer script of
// multi-instance synchronization (delivered over CDP, see
// Script). Observation only: nothing in this package injects
// input anywhere, and the script never reads the clipboard —
// paste keystrokes travel as ordinary keyboard events and
// each follower's own clipboard decides the outcome.
package capture

// BindingName is the Runtime binding the page script reports
// through. It exists only in the isolated world the sync
// engine creates for the script.
const BindingName = "__debrowserSyncReport"

// Script is the page observer. Installed only on the MASTER
// instance's pages, only in an isolated execution world, only
// on the top frame. It reports compact event descriptors; it
// never reads element values, text content or the DOM, and it
// never calls preventDefault — the page behaves exactly as if
// the script were absent.
//
// Event model (JSON):
//
//	pointerdown/pointerup/pointermove {x, y, button, buttons}
//	wheel   {x, y, deltaX, deltaY}
//	keydown/keyup {key, code, location, repeat, ctrl, alt, shift, meta, composing}
//	input   {text}          — an insertion increment only
//	compositionend {text}   — final IME text
//	visibility {visible}
//
// Input increments: the script diffs nothing and stores no
// field content; it forwards the InputEvent's own data payload
// (the inserted text). Deletions carry no text: they arrive as
// keydown events (Backspace/Delete).
const Script = `
(function () {
  if (window.top !== window.self) return;
  var report = window.__debrowserSyncReport;
  if (typeof report !== "function") return;
  var lastComposition = "";
  function send(kind, fields) {
    var ev = { kind: kind };
    for (var k in fields) ev[k] = fields[k];
    try { report(JSON.stringify(ev)); } catch (e) {}
  }
  function mods(e) {
    return { ctrl: !!e.ctrlKey, alt: !!e.altKey, shift: !!e.shiftKey, meta: !!e.metaKey };
  }
  window.addEventListener("pointerdown", function (e) {
    var f = { x: e.clientX, y: e.clientY, button: e.button, buttons: e.buttons };
    send("pointerdown", f);
  }, true);
  window.addEventListener("pointerup", function (e) {
    var f = { x: e.clientX, y: e.clientY, button: e.button, buttons: e.buttons };
    send("pointerup", f);
  }, true);
  window.addEventListener("pointermove", function (e) {
    send("pointermove", { x: e.clientX, y: e.clientY, button: e.button, buttons: e.buttons });
  }, true);
  window.addEventListener("wheel", function (e) {
    send("wheel", { x: e.clientX, y: e.clientY, deltaX: e.deltaX, deltaY: e.deltaY });
  }, true);
  window.addEventListener("keydown", function (e) {
    if (e.key === "Dead") return;
    var f = { key: e.key, code: e.code, location: e.location || 0, repeat: !!e.repeat, composing: !!e.isComposing };
    var m = mods(e); for (var k in m) f[k] = m[k];
    send("keydown", f);
  }, true);
  window.addEventListener("keyup", function (e) {
    if (e.key === "Dead") return;
    var f = { key: e.key, code: e.code, location: e.location || 0, repeat: false, composing: !!e.isComposing };
    var m = mods(e); for (var k in m) f[k] = m[k];
    send("keyup", f);
  }, true);
  window.addEventListener("input", function (e) {
    if (e.isComposing) return;
    if (e.inputType === "insertText" && typeof e.data === "string" && e.data) {
      send("input", { text: e.data });
    }
  }, true);
  window.addEventListener("compositionend", function (e) {
    lastComposition = e.data || "";
    if (lastComposition) send("compositionend", { text: lastComposition });
  }, true);
  function visibility() { send("visibility", { visible: document.visibilityState === "visible" }); }
  document.addEventListener("visibilitychange", visibility);
  visibility();
})();
`

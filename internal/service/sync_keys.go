package service

import (
	"github.com/onbeee/DeBrowser/internal/capture"
)

// Virtual-key to CDP key mapping for page-content keyboard
// sync. Pure and unit-tested. Text is produced for printable
// keys according to the current shift state; IME composition
// input is NOT synthesized here (see the sync support notes:
// composition text is not reliably transportable as key events,
// and this code never reads the user's typed text from anywhere
// — the text below is derived from the key identity itself).
func syncKeyForVK(vk uint32) (key, code, text string, isModifier bool) {
	shift := capture.CurrentModifiers()&8 != 0
	switch {
	case vk >= 0x41 && vk <= 0x5A: // A-Z
		letter := byte('a' + vk - 0x41)
		key = string(letter)
		if shift {
			key = string(letter - 'a' + 'A')
		}
		return key, "Key" + string(letter-'a'+'A'), key, false
	case vk >= 0x30 && vk <= 0x39: // 0-9
		digit := byte('0' + vk - 0x30)
		shifted := map[byte]byte{'1': '!', '2': '@', '3': '#', '4': '$', '5': '%', '6': '^', '7': '&', '8': '*', '9': '(', '0': ')'}
		key = string(digit)
		if shift {
			key = string(shifted[digit])
		}
		return key, "Digit" + string(digit), key, false
	case vk >= 0x70 && vk <= 0x7B: // F1-F12
		n := vk - 0x70 + 1
		name := "F" + string(byte('0'+n/10)) + string(byte('0'+n%10))
		if n < 10 {
			name = "F" + string(byte('0'+n))
		}
		return name, name, "", false
	case vk >= 0x60 && vk <= 0x69: // numpad digits
		digit := byte('0' + vk - 0x60)
		return string(digit), "Numpad" + string(digit), string(digit), false
	}
	type fixed struct {
		key, code, text string
		modifier        bool
	}
	fixedKeys := map[uint32]fixed{
		0x08: {"Backspace", "Backspace", "", false},
		0x09: {"Tab", "Tab", "", false},
		0x0D: {"Enter", "Enter", "\r", false},
		0x1B: {"Escape", "Escape", "", false},
		0x20: {" ", "Space", " ", false},
		0x21: {"PageUp", "PageUp", "", false},
		0x22: {"PageDown", "PageDown", "", false},
		0x23: {"End", "End", "", false},
		0x24: {"Home", "Home", "", false},
		0x25: {"ArrowLeft", "ArrowLeft", "", false},
		0x26: {"ArrowUp", "ArrowUp", "", false},
		0x27: {"ArrowRight", "ArrowRight", "", false},
		0x28: {"ArrowDown", "ArrowDown", "", false},
		0x2D: {"Insert", "Insert", "", false},
		0x2E: {"Delete", "Delete", "", false},
		0x10: {"Shift", "ShiftLeft", "", true},
		0x11: {"Control", "ControlLeft", "", true},
		0x12: {"Alt", "AltLeft", "", true},
		0xA0: {"Shift", "ShiftLeft", "", true},
		0xA1: {"Shift", "ShiftRight", "", true},
		0xA2: {"Control", "ControlLeft", "", true},
		0xA3: {"Control", "ControlRight", "", true},
		0xA4: {"Alt", "AltLeft", "", true},
		0xA5: {"Alt", "AltRight", "", true},
		0x5B: {"Meta", "MetaLeft", "", true},
		0x5C: {"Meta", "MetaRight", "", true},
		0xBA: {";", "Semicolon", ";", false},
		0xBB: {"=", "Equal", "=", false},
		0xBC: {",", "Comma", ",", false},
		0xBD: {"-", "Minus", "-", false},
		0xBE: {".", "Period", ".", false},
		0xBF: {"/", "Slash", "/", false},
		0xC0: {"`", "Backquote", "`", false},
		0xDB: {"[", "BracketLeft", "[", false},
		0xDC: {`\`, "Backslash", `\`, false},
		0xDD: {"]", "BracketRight", "]", false},
		0xDE: {"'", "Quote", "'", false},
		0x6A: {"*", "NumpadMultiply", "*", false},
		0x6B: {"+", "NumpadAdd", "+", false},
		0x6D: {"-", "NumpadSubtract", "-", false},
		0x6E: {".", "NumpadDecimal", ".", false},
		0x6F: {"/", "NumpadDivide", "/", false},
	}
	if f, ok := fixedKeys[vk]; ok {
		text := f.text
		if shift {
			switch vk {
			case 0xBA:
				text = ":"
			case 0xBB:
				text = "+"
			case 0xBC:
				text = "<"
			case 0xBD:
				text = "_"
			case 0xBE:
				text = ">"
			case 0xBF:
				text = "?"
			case 0xC0:
				text = "~"
			case 0xDB:
				text = "{"
			case 0xDC:
				text = "|"
			case 0xDD:
				text = "}"
			case 0xDE:
				text = "\""
			}
			if text != "" && len(text) == 1 && text[0] != '\r' {
				return text, f.code, text, f.modifier
			}
		}
		return f.key, f.code, text, f.modifier
	}
	return "", "", "", false
}

// currentModifiers reads the live modifier state in CDP bit
// layout (Alt=1, Ctrl=2, Meta=4, Shift=8).
func currentModifiers() int {
	return capture.CurrentModifiers()
}

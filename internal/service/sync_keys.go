package service

import "strings"

// Windows virtual-key codes for the keys synchronization
// forwards. Letters and digits derive from the physical code;
// named keys use the standard table. Unknown keys return 0,
// which Chromium accepts (the key/code pair still identifies
// the key semantically).
func vkForKey(ev PageEvent) int { return vkForCode(ev.Code) }

func vkForCode(code string) int {
	if len(code) == 4 && strings.HasPrefix(code, "Key") {
		return int(code[3]) // A-Z
	}
	if len(code) == 6 && strings.HasPrefix(code, "Digit") {
		return int(code[5]) // 0-9
	}
	if len(code) > 1 && code[0] == 'F' {
		n := 0
		for _, ch := range code[1:] {
			if ch < '0' || ch > '9' {
				n = -1
				break
			}
			n = n*10 + int(ch-'0')
		}
		if n >= 1 && n <= 12 {
			return 0x70 + n - 1 // VK_F1..
		}
	}
	if strings.HasPrefix(code, "Numpad") {
		switch code {
		case "Numpad0", "Numpad1", "Numpad2", "Numpad3", "Numpad4",
			"Numpad5", "Numpad6", "Numpad7", "Numpad8", "Numpad9":
			return 0x60 + int(code[6]-'0')
		case "NumpadMultiply":
			return 0x6A
		case "NumpadAdd":
			return 0x6B
		case "NumpadSubtract":
			return 0x6D
		case "NumpadDecimal":
			return 0x6E
		case "NumpadDivide":
			return 0x6F
		case "NumpadEnter":
			return 0x0D
		}
	}
	switch code {
	case "Backspace":
		return 0x08
	case "Tab":
		return 0x09
	case "Enter":
		return 0x0D
	case "ShiftLeft", "ShiftRight":
		return 0x10
	case "ControlLeft", "ControlRight":
		return 0x11
	case "AltLeft", "AltRight":
		return 0x12
	case "Pause":
		return 0x13
	case "CapsLock":
		return 0x14
	case "Escape":
		return 0x1B
	case "Space":
		return 0x20
	case "PageUp":
		return 0x21
	case "PageDown":
		return 0x22
	case "End":
		return 0x23
	case "Home":
		return 0x24
	case "ArrowLeft":
		return 0x25
	case "ArrowUp":
		return 0x26
	case "ArrowRight":
		return 0x27
	case "ArrowDown":
		return 0x28
	case "Insert":
		return 0x2D
	case "Delete":
		return 0x2E
	case "Semicolon":
		return 0xBA
	case "Equal":
		return 0xBB
	case "Comma":
		return 0xBC
	case "Minus":
		return 0xBD
	case "Period":
		return 0xBE
	case "Slash":
		return 0xBF
	case "Backquote":
		return 0xC0
	case "BracketLeft":
		return 0xDB
	case "Backslash":
		return 0xDC
	case "BracketRight":
		return 0xDD
	case "Quote":
		return 0xDE
	case "MetaLeft":
		return 0x5B
	case "MetaRight":
		return 0x5C
	case "ContextMenu":
		return 0x5D
	}
	return 0
}

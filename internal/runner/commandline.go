package runner

import "strings"

// UserDataDirArg extracts the value of the --user-data-dir
// argument from a process command line. It returns ok=false when
// the argument is missing, empty, repeated, or otherwise
// ambiguous: identity verification must fail closed on all of
// those, because a process whose instance folder cannot be
// established EXACTLY must never be treated as an instance's
// browser.
//
// Two command line shapes are supported:
//
//   - the Windows shape: one string, arguments separated by
//     spaces with Windows quoting rules (the shape read from a
//     process's PEB);
//   - the argv shape: NUL-separated arguments (the shape of
//     /proc/<pid>/cmdline on Unix).
//
// The value is extracted by parsing arguments — never by
// substring search — so a path that merely CONTAINS the expected
// folder (P-10 vs P-1), or the expected folder appearing in some
// other argument, does not count.
func UserDataDirArg(commandLine string) (string, bool) {
	var args []string
	if strings.ContainsRune(commandLine, '\x00') {
		args = strings.Split(commandLine, "\x00")
	} else {
		args = splitWindowsCommandLine(commandLine)
	}
	// args[0] is the program name, never an argument.
	value := ""
	count := 0
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--user-data-dir":
			count++
			if i+1 >= len(args) {
				return "", false // dangling flag: value unknown
			}
			value = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--user-data-dir="):
			count++
			value = strings.TrimPrefix(args[i], "--user-data-dir=")
		}
	}
	if count != 1 || value == "" {
		return "", false
	}
	return value, true
}

// splitWindowsCommandLine tokenizes a Windows command line using
// the CommandLineToArgvW rules: spaces and tabs separate
// arguments outside quotes, a double quote toggles the quoted
// state, and backslashes are literal except immediately before a
// quote (2n backslashes + quote = n literal backslashes and the
// quote acts as a delimiter; 2n+1 = n literal backslashes and a
// literal quote).
func splitWindowsCommandLine(s string) []string {
	var args []string
	var cur strings.Builder
	inQuotes := false
	hasArg := false
	flush := func() {
		if hasArg {
			args = append(args, cur.String())
			cur.Reset()
			hasArg = false
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\':
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			n := j - i
			if j < len(s) && s[j] == '"' {
				for k := 0; k < n/2; k++ {
					cur.WriteByte('\\')
				}
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					inQuotes = !inQuotes
				}
				hasArg = true
				i = j + 1
				continue
			}
			for k := 0; k < n; k++ {
				cur.WriteByte('\\')
			}
			hasArg = true
			i = j
		case c == '"':
			inQuotes = !inQuotes
			hasArg = true
			i++
		case (c == ' ' || c == '\t') && !inQuotes:
			flush()
			i++
		default:
			cur.WriteByte(c)
			hasArg = true
			i++
		}
	}
	flush()
	return args
}

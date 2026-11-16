package runner

import "testing"

// UserDataDirArg must extract the argument by parsing — the
// cases below are the identity-verification contract: anything
// missing, empty, repeated or ambiguous fails closed.
func TestUserDataDirArg(t *testing.T) {
	const dir = `C:\DeBrowser\data\profiles\P-1`
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"equals form", `brave.exe --user-data-dir=` + dir, dir, true},
		{"space form", `brave.exe --user-data-dir ` + dir, dir, true},
		{"quoted path with spaces", `brave.exe --user-data-dir="C:\My Profiles\P 1"`, `C:\My Profiles\P 1`, true},
		{"space form quoted", `brave.exe --user-data-dir "C:\My Profiles\P 1"`, `C:\My Profiles\P 1`, true},
		{"quoted executable", `"C:\Program Files\Brave\brave.exe" --user-data-dir=` + dir, dir, true},
		{"other arguments around", `brave.exe --no-first-run --user-data-dir=` + dir + ` https://example.com`, dir, true},
		{"value case preserved", `brave.exe --user-data-dir=c:\debrowser\DATA\profiles\p-1`, `c:\debrowser\DATA\profiles\p-1`, true},
		{"prefix sibling extracted whole", `brave.exe --user-data-dir=C:\DeBrowser\data\profiles\P-10`, `C:\DeBrowser\data\profiles\P-10`, true},
		{"missing", `brave.exe --no-first-run`, "", false},
		{"empty equals value", `brave.exe --user-data-dir=`, "", false},
		{"empty quoted value", `brave.exe --user-data-dir ""`, "", false},
		{"dangling flag", `brave.exe --user-data-dir`, "", false},
		{"duplicate same value", `brave.exe --user-data-dir=` + dir + ` --user-data-dir=` + dir, "", false},
		{"duplicate different values", `brave.exe --user-data-dir=` + dir + ` --user-data-dir=C:\other`, "", false},
		{"duplicate mixed forms", `brave.exe --user-data-dir ` + dir + ` --user-data-dir=` + dir, "", false},
		{"longer flag name is not the flag", `brave.exe --user-data-directory=` + dir, "", false},
		{"path in another argument does not count", `brave.exe --user-data-dir=C:\other --homepage=` + dir, `C:\other`, true},
		{"path as positional argument does not count", `brave.exe ` + dir, "", false},
		{"flag in executable position ignored", `--user-data-dir=` + dir + ` --no-first-run`, "", false},
		{"escaped quote inside value", `brave.exe --user-data-dir="C:\profiles\P\"1"`, `C:\profiles\P"1`, true},
		{"trailing backslash before closing quote", `brave.exe --user-data-dir="C:\profiles\P-1\\"`, `C:\profiles\P-1\`, true},
		{"backslashes not before a quote stay literal", `brave.exe --user-data-dir="C:\a\\b"`, `C:\a\\b`, true},
		{"tab separated", "brave.exe\t--user-data-dir=" + dir, dir, true},
		{"nul argv form", "brave.exe\x00--user-data-dir=" + dir + "\x00", dir, true},
		{"nul argv space form", "brave.exe\x00--user-data-dir\x00" + dir + "\x00", dir, true},
		{"nul argv missing", "brave.exe\x00--no-first-run\x00", "", false},
		{"empty command line", ``, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := UserDataDirArg(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Errorf("UserDataDirArg(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

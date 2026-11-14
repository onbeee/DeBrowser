package mihomocore

// The pinned official Mihomo release bundled with DeBrowser.
//
// Source: MetaCubeX/mihomo official GitHub Releases only. The
// SHA-256 below is the official release asset digest published
// by GitHub for the asset (the release ships no separate
// checksum file), re-verified by direct download on 2026-11-14:
// the downloaded zip's SHA-256 matches this value exactly, and
// the zip contains exactly one file, mihomo-windows-amd64.exe.
// The binary is used completely unmodified — DeBrowser manages
// its file and process lifecycle only, never its contents.
const (
	// PinnedVersion is the bundled release tag.
	PinnedVersion = "v1.19.34"
	// ReleasePageURL is the official release page.
	ReleasePageURL = "https://github.com/MetaCubeX/mihomo/releases/tag/" + PinnedVersion
	// WindowsZipName is the official Windows x64 (amd64) asset.
	WindowsZipName = "mihomo-windows-amd64-" + PinnedVersion + ".zip"
	// WindowsZipDownloadURL is the official asset download URL.
	WindowsZipDownloadURL = "https://github.com/MetaCubeX/mihomo/releases/download/" + PinnedVersion + "/" + WindowsZipName
	// WindowsZipSHA256 is the official SHA-256 of the zip asset.
	WindowsZipSHA256 = "f6b855de8722781caea62686f0301f89625fc558d48c1103ffdd98dad7f7f488"
	// WindowsExeName is the single file inside the zip asset.
	WindowsExeName = "mihomo-windows-amd64.exe"
	// GitHubAPIRepo is the official repository API base used by
	// the manual update check.
	GitHubAPIRepo = "https://api.github.com/repos/MetaCubeX/mihomo"
)

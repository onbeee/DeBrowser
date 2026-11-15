// Package model defines DeBrowser's core data types. It contains no
// behaviour and no dependencies, so every layer (store, service,
// future UI bindings) can share the same shapes.
package model

// InstanceStatus is the persisted lifecycle state of a browser
// instance. Live runtime state belongs to the Stage 3 instance
// manager; this is the durable record it writes.
type InstanceStatus string

const (
	StatusStopped InstanceStatus = "stopped"
	StatusRunning InstanceStatus = "running"
	StatusError   InstanceStatus = "error"
)

// Proxy is a proxy node definition. Credential material is never part
// of this struct: non-secret parameters live in ConfigJSON, and
// SecretRef is an opaque reference into the credential vault
// (internal/secrets).
type Proxy struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Protocol       string  `json:"protocol"`
	Address        string  `json:"address"`
	Port           *int    `json:"port,omitempty"`
	ConfigJSON     string  `json:"configJson"`
	SecretRef      string  `json:"secretRef"`
	Enabled        bool    `json:"enabled"`
	SortOrder      int     `json:"sortOrder"`
	Source         string  `json:"source"`
	SubscriptionID *string `json:"subscriptionId,omitempty"`
	// ConfigID is the imported Mihomo config this node came from
	// (Stage 6); nil for manually created nodes. SourceNodeName is
	// the node's name inside its source document — native
	// proxy-group members address nodes by that name.
	ConfigID       *string `json:"configId,omitempty"`
	SourceNodeName string  `json:"sourceNodeName"`
	DedupKey       string  `json:"dedupKey"`
	LastLatencyMs  *int    `json:"lastLatencyMs,omitempty"`
	LastTestOK     *bool   `json:"lastTestOk,omitempty"`
	LastTestedAt   *string `json:"lastTestedAt,omitempty"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
}

// Subscription is the metadata of a proxy subscription. The full
// subscription URL may carry credentials and lives exclusively in
// the credential vault; the database keeps only URLHint
// (scheme+host) and the opaque SecretRef.
type Subscription struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	URLHint           string  `json:"urlHint"`
	SecretRef         string  `json:"secretRef"`
	AllowLAN          bool    `json:"allowLan"`
	AutoUpdateMinutes int     `json:"autoUpdateMinutes"`
	LastUpdatedAt     *string `json:"lastUpdatedAt,omitempty"`
	LastStatus        string  `json:"lastStatus"`
	LastError         string  `json:"lastError"`
	CreatedAt         string  `json:"createdAt"`
	UpdatedAt         string  `json:"updatedAt"`
}

// Instance is the metadata of one browser instance (one isolated
// Chromium profile). The profile contents live on disk under
// UserDataDir and are never stored in SQLite.
type Instance struct {
	ID              string         `json:"id"`
	Name            string         `json:"name"`
	KernelID        string         `json:"kernelId"`
	KernelName      string         `json:"kernelName"`
	KernelVersion   string         `json:"kernelVersion"`
	UserDataDir     string         `json:"userDataDir"`
	ProxyID         *string        `json:"proxyId,omitempty"`
	ProxyListenPort *int           `json:"proxyListenPort,omitempty"`
	Status          InstanceStatus `json:"status"`
	StartURL        string         `json:"startUrl"`
	SortOrder       int            `json:"sortOrder"`
	CreatedAt       string         `json:"createdAt"`
	UpdatedAt       string         `json:"updatedAt"`
}

// MihomoConfig is one historically imported Mihomo configuration
// document (Stage 6). The type remains only for compatibility
// with existing databases; no current feature creates or shows
// these rows.
type MihomoConfig struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	SourceType         string  `json:"sourceType"` // "file" | "subscription"
	SubscriptionID     *string `json:"subscriptionId,omitempty"`
	FilePath           string  `json:"filePath"` // relative to data/mihomo/
	SHA256             string  `json:"sha256"`
	SecretRef          string  `json:"secretRef"`
	FormatVersion      int     `json:"formatVersion"`
	SecurityReportJSON string  `json:"securityReportJson"`
	CreatedAt          string  `json:"createdAt"`
	UpdatedAt          string  `json:"updatedAt"`
}

// MihomoProxyGroup is one native Mihomo proxy-group of an imported
// config, preserved structurally. It is the config author's group,
// not a DeBrowser grouping feature.
type MihomoProxyGroup struct {
	ID          string                   `json:"id"`
	ConfigID    string                   `json:"configId"`
	Name        string                   `json:"name"`
	Type        string                   `json:"type"`
	OptionsJSON string                   `json:"optionsJson"`
	SortOrder   int                      `json:"sortOrder"`
	Members     []MihomoProxyGroupMember `json:"members,omitempty"`
	CreatedAt   string                   `json:"createdAt"`
	UpdatedAt   string                   `json:"updatedAt"`
}

// MihomoProxyGroupMember is one member of a native proxy-group, in
// document order. Exactly one target is set, per Kind: ProxyID for
// "proxy", TargetGroupID for "group", neither for "builtin".
type MihomoProxyGroupMember struct {
	GroupID       string  `json:"groupId"`
	SortOrder     int     `json:"sortOrder"`
	MemberName    string  `json:"memberName"`
	Kind          string  `json:"kind"` // "proxy" | "group" | "builtin"
	ProxyID       *string `json:"proxyId,omitempty"`
	TargetGroupID *string `json:"targetGroupId,omitempty"`
}

// Kernel is one DeBrowser-managed standalone Chromium build
// (Stage 9A): a Chrome for Testing download that DeBrowser fetched,
// verified and installed under <root>/kernels/<version>. Only
// managed kernels are persisted; system browsers are detected live
// and never stored. InstallPath and ExecutablePath are relative to
// the portable application root and are resolved and re-validated
// on every use.
type Kernel struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	Channel        string `json:"channel"`
	Platform       string `json:"platform"`
	InstallPath    string `json:"installPath"`
	ExecutablePath string `json:"executablePath"`
	Status         string `json:"status"` // "ready" | "verification_failed" | "missing"
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

// ExternalBrowser is a browser the user added by hand (Stage 9A):
// a PURE PATH REGISTRATION. The user picks a program executable
// in the file picker and DeBrowser stores only the chosen path
// and a display name derived from the file name. DeBrowser
// performs NO identity, version, signature or publisher check on
// the chosen file, never launches it in this stage, and never
// copies, moves, modifies or deletes it; removing the
// registration deletes only this record.
type ExternalBrowser struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ExecutablePath string `json:"executablePath"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
}

// Bookmark is a single bookmark. A nil InstanceID means the bookmark
// is global and applies to every instance.
type Bookmark struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	URL        string  `json:"url"`
	InstanceID *string `json:"instanceId,omitempty"`
	SortOrder  int     `json:"sortOrder"`
	CreatedAt  string  `json:"createdAt"`
	UpdatedAt  string  `json:"updatedAt"`
}

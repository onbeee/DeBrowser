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

// MihomoConfig is one imported Mihomo configuration document
// (Stage 6). The sanitized document itself is a file under
// data/mihomo/configs/; this row holds only its relative path,
// content hash and the opaque vault reference of the extracted
// config-level credential bundle.
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

// CustomGroupMember is a user-added membership overlay row
// (Stage 6C): the user attached one of their custom nodes
// (proxies.source = 'custom') to a native proxy-group of an
// imported config. The group is addressed by NAME because a
// subscription refresh deletes and recreates the group rows; the
// overlay is keyed by (config, group name, proxy) so it survives
// refreshes, and it is kept strictly separate from the imported
// members in mihomo_proxy_group_members. It never modifies the
// imported document.
type CustomGroupMember struct {
	ConfigID  string `json:"configId"`
	GroupName string `json:"groupName"`
	ProxyID   string `json:"proxyId"`
	SortOrder int    `json:"sortOrder"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
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

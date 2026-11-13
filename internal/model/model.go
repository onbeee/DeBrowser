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
	GroupID        *string `json:"groupId,omitempty"`
	SortOrder      int     `json:"sortOrder"`
	Source         string  `json:"source"`
	SubscriptionID *string `json:"subscriptionId,omitempty"`
	DedupKey       string  `json:"dedupKey"`
	LastLatencyMs  *int    `json:"lastLatencyMs,omitempty"`
	LastTestOK     *bool   `json:"lastTestOk,omitempty"`
	LastTestedAt   *string `json:"lastTestedAt,omitempty"`
	CreatedAt      string  `json:"createdAt"`
	UpdatedAt      string  `json:"updatedAt"`
}

// ProxyGroup is a named, user-managed grouping for proxies.
// Deleting a group never deletes its proxies (their group_id is
// set to NULL by the schema).
type ProxyGroup struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	SortOrder int    `json:"sortOrder"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
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
	GroupID           *string `json:"groupId,omitempty"`
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

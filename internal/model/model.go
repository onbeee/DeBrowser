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
// SecretRef is an opaque reference to a future secure store.
type Proxy struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	Address    string `json:"address"`
	Port       *int   `json:"port,omitempty"`
	ConfigJSON string `json:"configJson"`
	SecretRef  string `json:"secretRef"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"createdAt"`
	UpdatedAt  string `json:"updatedAt"`
}

// Instance is the metadata of one browser instance (one isolated
// Chromium profile). The profile contents live on disk under
// UserDataDir and are never stored in SQLite.
type Instance struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	KernelID      string         `json:"kernelId"`
	KernelName    string         `json:"kernelName"`
	KernelVersion string         `json:"kernelVersion"`
	UserDataDir   string         `json:"userDataDir"`
	ProxyID       *string        `json:"proxyId,omitempty"`
	Status        InstanceStatus `json:"status"`
	StartURL      string         `json:"startUrl"`
	SortOrder     int            `json:"sortOrder"`
	CreatedAt     string         `json:"createdAt"`
	UpdatedAt     string         `json:"updatedAt"`
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

package runhost

// The update verb's wire: `update cli <version>` sends a signed CLI release
// for the server's architecture, `update gvisor <release>` gVisor's release
// archive. Each input is an UpdateHeader line followed by Size bytes.

// MaxUpdate bounds what an update sends: a CLI binary or gVisor's archive.
const MaxUpdate = 512 << 20

// RestartExit is the service's exit status when it stops to restart into
// the CLI an update installed; systemd starts it again.
const RestartExit = 75

// UpdateHeader precedes an update's bytes.
type UpdateHeader struct {
	// Name is the binary's file name in Sums, for a CLI release.
	Name string `json:"name,omitempty"`
	// Sums and Sig are the release's SHA256SUMS and its minisign
	// signature, for a CLI release.
	Sums []byte `json:"sums,omitempty"`
	Sig  []byte `json:"sig,omitempty"`
	// Size is how many bytes follow the header.
	Size int64 `json:"size"`
}

// Updated is the update verb's answer.
type Updated struct {
	// What is "cli" or "gvisor".
	What string `json:"what"`
	From string `json:"from"`
	To   string `json:"to"`
	// Installed is false when the runhost already ran To.
	Installed bool `json:"installed"`
	// WaitsForJobs is how many running jobs the service waits for before
	// it restarts into the CLI it installed.
	WaitsForJobs int `json:"waitsForJobs,omitempty"`
}

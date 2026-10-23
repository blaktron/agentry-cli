package sockfilter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Create bodies. The daemon decodes a body with encoding/json, which
// matches a field name in any case; a check that read the body's keys as
// written would miss "privileged" beside "Privileged". So each body is
// decoded into the structs below the same way the daemon decodes it, a
// field these structs do not name refuses the request (a daemon may learn
// fields the filter has not judged), the decoded value is checked, and
// what goes to the daemon is its re-encoding: exactly what was checked,
// under the daemon's own field names.
//
// A field kept as json.RawMessage passes as the caller wrote it; those
// with anything to say about the host are typed and checked.

type containerCreateBody struct {
	Hostname         string            `json:"Hostname,omitempty"`
	Domainname       string            `json:"Domainname,omitempty"`
	User             string            `json:"User,omitempty"`
	AttachStdin      bool              `json:"AttachStdin,omitempty"`
	AttachStdout     bool              `json:"AttachStdout,omitempty"`
	AttachStderr     bool              `json:"AttachStderr,omitempty"`
	ExposedPorts     json.RawMessage   `json:"ExposedPorts,omitempty"`
	Tty              bool              `json:"Tty,omitempty"`
	OpenStdin        bool              `json:"OpenStdin,omitempty"`
	StdinOnce        bool              `json:"StdinOnce,omitempty"`
	Env              []string          `json:"Env,omitempty"`
	Cmd              json.RawMessage   `json:"Cmd,omitempty"`
	Healthcheck      json.RawMessage   `json:"Healthcheck,omitempty"`
	ArgsEscaped      bool              `json:"ArgsEscaped,omitempty"`
	Image            string            `json:"Image,omitempty"`
	Volumes          json.RawMessage   `json:"Volumes,omitempty"`
	WorkingDir       string            `json:"WorkingDir,omitempty"`
	Entrypoint       json.RawMessage   `json:"Entrypoint,omitempty"`
	NetworkDisabled  bool              `json:"NetworkDisabled,omitempty"`
	MacAddress       string            `json:"MacAddress,omitempty"`
	OnBuild          json.RawMessage   `json:"OnBuild,omitempty"`
	Labels           map[string]string `json:"Labels,omitempty"`
	StopSignal       string            `json:"StopSignal,omitempty"`
	StopTimeout      *int              `json:"StopTimeout,omitempty"`
	Shell            json.RawMessage   `json:"Shell,omitempty"`
	HostConfig       *hostConfig       `json:"HostConfig,omitempty"`
	NetworkingConfig *networkingConfig `json:"NetworkingConfig,omitempty"`
}

type hostConfig struct {
	Binds           []string          `json:"Binds,omitempty"`
	ContainerIDFile string            `json:"ContainerIDFile,omitempty"`
	LogConfig       *logConfig        `json:"LogConfig,omitempty"`
	NetworkMode     string            `json:"NetworkMode,omitempty"`
	PortBindings    json.RawMessage   `json:"PortBindings,omitempty"`
	RestartPolicy   *restartPolicy    `json:"RestartPolicy,omitempty"`
	AutoRemove      bool              `json:"AutoRemove,omitempty"`
	VolumeDriver    string            `json:"VolumeDriver,omitempty"`
	VolumesFrom     []string          `json:"VolumesFrom,omitempty"`
	ConsoleSize     json.RawMessage   `json:"ConsoleSize,omitempty"`
	Annotations     map[string]string `json:"Annotations,omitempty"`
	CapAdd          []string          `json:"CapAdd,omitempty"`
	CapDrop         []string          `json:"CapDrop,omitempty"`
	CgroupnsMode    string            `json:"CgroupnsMode,omitempty"`
	DNS             []string          `json:"Dns,omitempty"`
	DNSOptions      []string          `json:"DnsOptions,omitempty"`
	DNSSearch       []string          `json:"DnsSearch,omitempty"`
	ExtraHosts      []string          `json:"ExtraHosts,omitempty"`
	GroupAdd        []string          `json:"GroupAdd,omitempty"`
	IpcMode         string            `json:"IpcMode,omitempty"`
	Cgroup          string            `json:"Cgroup,omitempty"`
	Links           []string          `json:"Links,omitempty"`
	OomScoreAdj     int               `json:"OomScoreAdj,omitempty"`
	PidMode         string            `json:"PidMode,omitempty"`
	Privileged      bool              `json:"Privileged,omitempty"`
	PublishAllPorts bool              `json:"PublishAllPorts,omitempty"`
	ReadonlyRootfs  bool              `json:"ReadonlyRootfs,omitempty"`
	SecurityOpt     []string          `json:"SecurityOpt,omitempty"`
	StorageOpt      map[string]string `json:"StorageOpt,omitempty"`
	Tmpfs           map[string]string `json:"Tmpfs,omitempty"`
	UTSMode         string            `json:"UTSMode,omitempty"`
	UsernsMode      string            `json:"UsernsMode,omitempty"`
	ShmSize         int64             `json:"ShmSize,omitempty"`
	Sysctls         map[string]string `json:"Sysctls,omitempty"`
	Runtime         string            `json:"Runtime,omitempty"`
	Isolation       string            `json:"Isolation,omitempty"`
	MaskedPaths     json.RawMessage   `json:"MaskedPaths,omitempty"`
	ReadonlyPaths   json.RawMessage   `json:"ReadonlyPaths,omitempty"`
	Init            *bool             `json:"Init,omitempty"`
	Mounts          []mount           `json:"Mounts,omitempty"`

	// Resources.
	CPUShares            int64           `json:"CpuShares,omitempty"`
	Memory               int64           `json:"Memory,omitempty"`
	NanoCPUs             int64           `json:"NanoCpus,omitempty"`
	CgroupParent         string          `json:"CgroupParent,omitempty"`
	BlkioWeight          uint16          `json:"BlkioWeight,omitempty"`
	BlkioWeightDevice    json.RawMessage `json:"BlkioWeightDevice,omitempty"`
	BlkioDeviceReadBps   json.RawMessage `json:"BlkioDeviceReadBps,omitempty"`
	BlkioDeviceWriteBps  json.RawMessage `json:"BlkioDeviceWriteBps,omitempty"`
	BlkioDeviceReadIOps  json.RawMessage `json:"BlkioDeviceReadIOps,omitempty"`
	BlkioDeviceWriteIOps json.RawMessage `json:"BlkioDeviceWriteIOps,omitempty"`
	CPUPeriod            int64           `json:"CpuPeriod,omitempty"`
	CPUQuota             int64           `json:"CpuQuota,omitempty"`
	CPURealtimePeriod    int64           `json:"CpuRealtimePeriod,omitempty"`
	CPURealtimeRuntime   int64           `json:"CpuRealtimeRuntime,omitempty"`
	CpusetCpus           string          `json:"CpusetCpus,omitempty"`
	CpusetMems           string          `json:"CpusetMems,omitempty"`
	Devices              json.RawMessage `json:"Devices,omitempty"`
	DeviceCgroupRules    json.RawMessage `json:"DeviceCgroupRules,omitempty"`
	DeviceRequests       json.RawMessage `json:"DeviceRequests,omitempty"`
	KernelMemory         int64           `json:"KernelMemory,omitempty"`
	KernelMemoryTCP      int64           `json:"KernelMemoryTCP,omitempty"`
	MemoryReservation    int64           `json:"MemoryReservation,omitempty"`
	MemorySwap           int64           `json:"MemorySwap,omitempty"`
	MemorySwappiness     *int64          `json:"MemorySwappiness,omitempty"`
	OomKillDisable       *bool           `json:"OomKillDisable,omitempty"`
	PidsLimit            *int64          `json:"PidsLimit,omitempty"`
	Ulimits              json.RawMessage `json:"Ulimits,omitempty"`
	CPUCount             int64           `json:"CpuCount,omitempty"`
	CPUPercent           int64           `json:"CpuPercent,omitempty"`
	IOMaximumIOps        uint64          `json:"IOMaximumIOps,omitempty"`
	IOMaximumBandwidth   uint64          `json:"IOMaximumBandwidth,omitempty"`
}

type logConfig struct {
	Type   string            `json:"Type,omitempty"`
	Config map[string]string `json:"Config,omitempty"`
}

type restartPolicy struct {
	Name              string `json:"Name,omitempty"`
	MaximumRetryCount int    `json:"MaximumRetryCount,omitempty"`
}

type mount struct {
	Type          string          `json:"Type,omitempty"`
	Source        string          `json:"Source,omitempty"`
	Target        string          `json:"Target,omitempty"`
	ReadOnly      bool            `json:"ReadOnly,omitempty"`
	Consistency   string          `json:"Consistency,omitempty"`
	BindOptions   *bindOptions    `json:"BindOptions,omitempty"`
	VolumeOptions *volumeOptions  `json:"VolumeOptions,omitempty"`
	TmpfsOptions  *tmpfsOptions   `json:"TmpfsOptions,omitempty"`
	ImageOptions  json.RawMessage `json:"ImageOptions,omitempty"`
	// ClusterOptions is swarm's; a run has none.
	ClusterOptions json.RawMessage `json:"ClusterOptions,omitempty"`
}

type bindOptions struct {
	Propagation            string `json:"Propagation,omitempty"`
	NonRecursive           bool   `json:"NonRecursive,omitempty"`
	CreateMountpoint       bool   `json:"CreateMountpoint,omitempty"`
	ReadOnlyNonRecursive   bool   `json:"ReadOnlyNonRecursive,omitempty"`
	ReadOnlyForceRecursive bool   `json:"ReadOnlyForceRecursive,omitempty"`
}

type volumeOptions struct {
	NoCopy       bool              `json:"NoCopy,omitempty"`
	Labels       map[string]string `json:"Labels,omitempty"`
	Subpath      string            `json:"Subpath,omitempty"`
	DriverConfig json.RawMessage   `json:"DriverConfig,omitempty"`
}

type tmpfsOptions struct {
	SizeBytes int64           `json:"SizeBytes,omitempty"`
	Mode      uint32          `json:"Mode,omitempty"`
	Options   json.RawMessage `json:"Options,omitempty"`
}

type networkingConfig struct {
	EndpointsConfig map[string]*endpointSettings `json:"EndpointsConfig,omitempty"`
}

type endpointSettings struct {
	IPAMConfig          json.RawMessage   `json:"IPAMConfig,omitempty"`
	Links               []string          `json:"Links,omitempty"`
	Aliases             []string          `json:"Aliases,omitempty"`
	MacAddress          string            `json:"MacAddress,omitempty"`
	DriverOpts          map[string]string `json:"DriverOpts,omitempty"`
	GwPriority          int               `json:"GwPriority,omitempty"`
	NetworkID           string            `json:"NetworkID,omitempty"`
	EndpointID          string            `json:"EndpointID,omitempty"`
	Gateway             string            `json:"Gateway,omitempty"`
	IPAddress           string            `json:"IPAddress,omitempty"`
	IPPrefixLen         int               `json:"IPPrefixLen,omitempty"`
	IPv6Gateway         string            `json:"IPv6Gateway,omitempty"`
	GlobalIPv6Address   string            `json:"GlobalIPv6Address,omitempty"`
	GlobalIPv6PrefixLen int               `json:"GlobalIPv6PrefixLen,omitempty"`
	DNSNames            []string          `json:"DNSNames,omitempty"`
}

type networkCreateBody struct {
	Name           string            `json:"Name,omitempty"`
	CheckDuplicate bool              `json:"CheckDuplicate,omitempty"`
	Driver         string            `json:"Driver,omitempty"`
	Scope          string            `json:"Scope,omitempty"`
	Internal       bool              `json:"Internal,omitempty"`
	Attachable     bool              `json:"Attachable,omitempty"`
	Ingress        bool              `json:"Ingress,omitempty"`
	ConfigOnly     bool              `json:"ConfigOnly,omitempty"`
	ConfigFrom     json.RawMessage   `json:"ConfigFrom,omitempty"`
	IPAM           *ipam             `json:"IPAM,omitempty"`
	EnableIPv4     *bool             `json:"EnableIPv4,omitempty"`
	EnableIPv6     *bool             `json:"EnableIPv6,omitempty"`
	Options        map[string]string `json:"Options,omitempty"`
	Labels         map[string]string `json:"Labels,omitempty"`
}

type ipam struct {
	Driver  string            `json:"Driver,omitempty"`
	Config  json.RawMessage   `json:"Config,omitempty"`
	Options map[string]string `json:"Options,omitempty"`
}

type volumeCreateBody struct {
	Name              string            `json:"Name,omitempty"`
	Driver            string            `json:"Driver,omitempty"`
	DriverOpts        map[string]string `json:"DriverOpts,omitempty"`
	Labels            map[string]string `json:"Labels,omitempty"`
	ClusterVolumeSpec json.RawMessage   `json:"ClusterVolumeSpec,omitempty"`
}

type execCreateBody struct {
	AttachStdin  bool            `json:"AttachStdin,omitempty"`
	AttachStdout bool            `json:"AttachStdout,omitempty"`
	AttachStderr bool            `json:"AttachStderr,omitempty"`
	ConsoleSize  json.RawMessage `json:"ConsoleSize,omitempty"`
	DetachKeys   string          `json:"DetachKeys,omitempty"`
	Tty          bool            `json:"Tty,omitempty"`
	Env          []string        `json:"Env,omitempty"`
	Cmd          json.RawMessage `json:"Cmd,omitempty"`
	Privileged   bool            `json:"Privileged,omitempty"`
	User         string          `json:"User,omitempty"`
	WorkingDir   string          `json:"WorkingDir,omitempty"`
}

// rewrite reads a body, has check decode and judge it, and replaces the
// body with check's value re-encoded.
func (f *Filter) rewrite(r *http.Request, check func([]byte) (any, error)) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		return deny("body: %v", err)
	}
	_ = r.Body.Close()
	v, err := check(body)
	if err != nil {
		return err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return deny("body: %v", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", fmt.Sprint(len(out)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Transfer-Encoding")
	r.TransferEncoding = nil
	return nil
}

// decodeStrict decodes as the daemon does, but refuses a field it does
// not know and anything after the one value.
func decodeStrict(body []byte, into any) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return deny("the request body holds what the filter does not judge: %v", err)
	}
	if dec.More() {
		return deny("the request body holds more than one value")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return deny("the request body holds more than one value")
	}
	return nil
}

func (f *Filter) label(labels map[string]string) map[string]string {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[f.cfg.Label] = f.cfg.RunID
	labels[f.cfg.OwnerLabel] = f.cfg.RunID
	return labels
}

func (f *Filter) containerCreate(ctx context.Context, body []byte) (any, error) {
	var c containerCreateBody
	if err := decodeStrict(body, &c); err != nil {
		return nil, err
	}
	if c.HostConfig == nil {
		c.HostConfig = &hostConfig{}
	}
	hc := c.HostConfig
	if err := f.checkHost(ctx, hc); err != nil {
		return nil, err
	}
	// The container is made from the image the check saw: its id.
	imageID, err := f.resolveImage(ctx, c.Image)
	if err != nil {
		return nil, err
	}
	c.Image = imageID
	scope, err := f.browserScope(ctx, &c, imageID)
	if err != nil {
		return nil, err
	}
	if err := f.checkNetworks(ctx, &c, scope); err != nil {
		return nil, err
	}
	tools, err := f.isToolsServer(ctx, &c, imageID)
	if err != nil {
		return nil, err
	}
	appLink, err := f.isAppLink(ctx, &c, imageID)
	if err != nil {
		return nil, err
	}
	offline := hc.NetworkMode == "none" && (c.NetworkingConfig == nil || len(c.NetworkingConfig.EndpointsConfig) == 0)
	if err := f.checkStorage(ctx, hc, mounter{tools: tools, appLink: appLink, offline: offline, create: &c, imageID: imageID}); err != nil {
		return nil, err
	}
	web, err := f.webShape(ctx, &c)
	if err != nil {
		return nil, err
	}
	if web && f.cfg.WebResolvConf != "" {
		// Added after the storage check: the run's own file, not one the
		// gateway named.
		hc.Binds = append(hc.Binds, f.cfg.WebResolvConf+":"+resolvConfTarget+":ro")
	}
	if err := f.toolsTmpfs(ctx, &c, imageID); err != nil {
		return nil, err
	}
	// The browser server's class (browser.go).
	browser, err := f.isBrowser(ctx, imageID)
	if err != nil {
		return nil, err
	}
	if browser {
		if err := f.browserShape(&c); err != nil {
			return nil, err
		}
	}
	if err := f.bound(hc); err != nil {
		return nil, err
	}
	relay, err := f.relayPin(ctx, imageID)
	if err != nil {
		return nil, err
	}
	// under is the runtime this container must run under, for the
	// response's check: what the body asks the daemon for, or, through a
	// per-run podman service, the service's default for every class.
	under := ""
	if relay != "" {
		if err := f.relayShape(&c, relay); err != nil {
			return nil, err
		}
		f.relayPinHosts(&c)
		if f.cfg.RelayResolvConf != "" && f.cfg.Runtime != "" {
			// Under the run's runtime, with the run's own name servers:
			// added after the shape check, which holds
			// the gateway to naming no file of a relay's. A relay is
			// created with both its networks, so none is connected after
			// it starts (a sandbox takes no such network).
			hc.Binds = append(hc.Binds, f.cfg.RelayResolvConf+":"+resolvConfTarget+":ro")
			hc.Runtime = f.cfg.Runtime
			under = f.cfg.Runtime
		}
		if f.cfg.Podman {
			// A rootless engine keeps the kernel's floor on the ports a
			// plain user may bind, and the relay binds 443 as one: the
			// floor is lowered for it, as Docker Engine lowers it in every
			// container. The gateway asked for no sysctl (checkHost).
			hc.Sysctls = map[string]string{relayPortSysctl: "0"}
		}
	} else {
		if f.cfg.Runtime != "" || f.cfg.Podman {
			if err := f.pinLinks(ctx, &c); err != nil {
				return nil, err
			}
		}
		if f.cfg.Runtime != "" {
			hc.Runtime = f.cfg.Runtime
			under = f.cfg.Runtime
		}
	}
	if f.cfg.Podman {
		// podman refuses a link; the hosts entries pinLinks wrote answer
		// what the link would have.
		clearLinks(&c)
		if f.cfg.Runtime != "" {
			under = f.cfg.Runtime
		}
	}
	if under != "" && f.cfg.SELinuxUnconfined {
		hc.SecurityOpt = append(hc.SecurityOpt, "label=disable")
	}
	if m, ok := ctx.Value(createMetaKey{}).(*createMeta); ok {
		m.class, m.runtime = classOf(web, tools, browser, relay != ""), under
	}
	// The shapes above name their own refusal of a health check; any other
	// container's is refused here.
	if err := noHealthcheck(c.Healthcheck); err != nil {
		return nil, err
	}
	// A relay and the browser server get no capability back.
	capabilities(hc, web || tools, relay != "" || browser)
	c.Labels = f.label(c.Labels)
	// Only the run's tools server is marked as it, whatever the gateway
	// asked: the runner finds the container to relay a published port into
	// by this label.
	delete(c.Labels, ToolsLabel)
	if tools {
		c.Labels[ToolsLabel] = f.cfg.RunID
	}
	return c, nil
}

// noHealthcheck refuses a health check that runs anything: the daemon runs
// its command inside the container, with the container's binds, as an exec
// does. A check that only switches off the image's own ({"Test":["NONE"]})
// passes.
func noHealthcheck(raw json.RawMessage) error {
	if !present(raw) {
		return nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return deny("a health check the filter cannot read: %v", err)
	}
	if len(doc) == 1 {
		for k, v := range doc {
			var test []string
			if strings.EqualFold(k, "Test") && json.Unmarshal(v, &test) == nil && len(test) == 1 && test[0] == "NONE" {
				return nil
			}
		}
	}
	return deny("a health check: the daemon runs its command inside the container, with the container's folders")
}

// classOf is a gateway-launched container's class in the receipt's words.
func classOf(web, tools, browser, relay bool) string {
	switch {
	case relay:
		return "relay"
	case browser:
		return "browser server"
	case web:
		return "web tools server"
	case tools:
		return "tools server"
	}
	return "declared MCP server"
}

// clearLinks removes a body's links, the hosts entries having taken their
// place.
func clearLinks(c *containerCreateBody) {
	c.HostConfig.Links = nil
	if nc := c.NetworkingConfig; nc != nil {
		for _, ep := range nc.EndpointsConfig {
			if ep != nil {
				ep.Links = nil
			}
		}
	}
}

// The capabilities a gateway-launched container keeps. The gateway asks for
// none of its own (checkHost refuses CapAdd) and can drop none back: every
// container it creates drops them all, and gets back only what its class
// needs, as the runner's own containers do.
var (
	// toolsCapabilities are the runner's tools servers' (the run's tools
	// server and the web tools' server, both the runner image's toolsrv
	// as root): CHOWN and FOWNER for what it writes and hands to the run's
	// owner, DAC_OVERRIDE to write into binds that owner holds, SETUID and
	// SETGID to start Typst and ripgrep as a user of their own, and KILL
	// to end them.
	toolsCapabilities = []string{"CHOWN", "DAC_OVERRIDE", "FOWNER", "KILL", "SETGID", "SETUID"}
	// serverReadCapabilities are a declared MCP server's: its package is
	// a read-only bind of the run's server store, which the run's owner
	// holds at 0700, and the server runs as its image's root, so it reads
	// past the store's mode and writes nothing of the host's.
	serverReadCapabilities = []string{"DAC_READ_SEARCH"}
	// serverWriteCapabilities are a declared server's that is also given
	// a writable host folder (its connection's token folder): it writes
	// into a folder the run's owner holds.
	serverWriteCapabilities = []string{"DAC_OVERRIDE"}
)

// ToolsCapabilities, ServerReadCapabilities and ServerWriteCapabilities
// are what the filter gives back to each class, for the receipt to name.
func ToolsCapabilities() []string       { return slices.Clone(toolsCapabilities) }
func ServerReadCapabilities() []string  { return slices.Clone(serverReadCapabilities) }
func ServerWriteCapabilities() []string { return slices.Clone(serverWriteCapabilities) }

// capabilities drops every capability of a gateway-launched container
// and adds back its class's: none for a relay, toolsCapabilities for the
// runner's tools servers, and for a declared server read past the store's
// mode, plus write where it has a writable host folder.
func capabilities(hc *hostConfig, tools, relay bool) {
	hc.CapDrop = []string{"ALL"}
	switch {
	case relay:
		hc.CapAdd = nil
	case tools:
		hc.CapAdd = slices.Clone(toolsCapabilities)
	case writesHost(hc):
		hc.CapAdd = append(slices.Clone(serverReadCapabilities), serverWriteCapabilities...)
	default:
		hc.CapAdd = slices.Clone(serverReadCapabilities)
	}
}

// writesHost says whether a container is given a host folder it may write:
// a bind of an absolute path without "ro", or a bind mount not read-only.
// A volume is docker's own, written as root without any capability.
func writesHost(hc *hostConfig) bool {
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if !strings.HasPrefix(parts[0], "/") {
			continue
		}
		if len(parts) < 3 || !slices.Contains(strings.Split(parts[2], ","), "ro") {
			return true
		}
	}
	for _, m := range hc.Mounts {
		if m.Type == "bind" && !m.ReadOnly {
			return true
		}
	}
	return false
}

// mounter is which of the run's own containers a create is, for the
// volumes only it may mount.
type mounter struct {
	tools, appLink bool
	// offline says the container joins no network.
	offline bool
	// create and imageID are the create itself and its image's id, which
	// a held root's holders are matched against (Config.HeldRoots).
	create  *containerCreateBody
	imageID string
}

// isAppLink says whether a create is the run's app link: its one command
// (Config.AppLinkCmd) in its image (Config.AppLinkImage, by id), run by the
// image's own entrypoint.
func (f *Filter) isAppLink(ctx context.Context, c *containerCreateBody, imageID string) (bool, error) {
	if f.cfg.AppLinkVolume == "" || f.cfg.AppLinkImage == "" || len(f.cfg.AppLinkCmd) == 0 {
		return false, nil
	}
	if present(c.Entrypoint) || present(c.Shell) {
		return false, nil
	}
	var cmd []string
	if !present(c.Cmd) || json.Unmarshal(c.Cmd, &cmd) != nil || !slices.Equal(cmd, f.cfg.AppLinkCmd) {
		return false, nil
	}
	id, err := f.resolveImage(ctx, f.cfg.AppLinkImage)
	if err != nil {
		return false, err
	}
	return imageID == id, nil
}

// isToolsServer says whether a create is the run's tools server: its one
// command (Config.ToolsCmd) in its image (Config.ToolsImage, by id), run by
// the image's own entrypoint, since another entrypoint would take the
// command for its arguments.
func (f *Filter) isToolsServer(ctx context.Context, c *containerCreateBody, imageID string) (bool, error) {
	if f.cfg.ToolsImage == "" || len(f.cfg.ToolsCmd) == 0 {
		return false, nil
	}
	if present(c.Entrypoint) || present(c.Shell) {
		return false, nil
	}
	var cmd []string
	if !present(c.Cmd) || json.Unmarshal(c.Cmd, &cmd) != nil || !slices.Equal(cmd, f.cfg.ToolsCmd) {
		return false, nil
	}
	tools, err := f.toolsImage(ctx)
	if err != nil {
		return false, err
	}
	return imageID == tools, nil
}

// toolsTmpfs mounts Config.ToolsTmpfs into the run's tools server, the
// container of ToolsImage that runs ToolsCmd, and into no other: its /tmp
// and scratch folder become size-capped tmpfs, so
// what a declared command or the scratch tools write is bounded and never
// reaches the host's disk. Added after the storage check, as the run's
// own; a request that puts anything of the gateway's at or beneath one of
// those targets is refused rather than shadowed.
func (f *Filter) toolsTmpfs(ctx context.Context, c *containerCreateBody, imageID string) error {
	if len(f.cfg.ToolsTmpfs) == 0 {
		return nil
	}
	if tools, err := f.isToolsServer(ctx, c, imageID); err != nil || !tools {
		return err
	}
	hc := c.HostConfig
	var declared map[string]json.RawMessage
	if present(c.Volumes) {
		if err := json.Unmarshal(c.Volumes, &declared); err != nil {
			return deny("the tools server's Volumes are a map of targets")
		}
	}
	shared, err := f.toolsShared()
	if err != nil {
		return err
	}
	targets := make([]string, 0, len(declared)+len(hc.Binds)+len(hc.Mounts)+len(hc.Tmpfs))
	for t := range declared {
		targets = append(targets, t)
	}
	for _, b := range hc.Binds {
		if parts := strings.Split(b, ":"); len(parts) >= 2 {
			if src, ok := shared[path.Clean(parts[1])]; ok && parts[0] == src {
				continue
			}
			targets = append(targets, parts[1])
		}
	}
	for _, m := range hc.Mounts {
		targets = append(targets, m.Target)
	}
	for t := range hc.Tmpfs {
		targets = append(targets, t)
	}
	for mine := range f.cfg.ToolsTmpfs {
		for _, t := range targets {
			if t := path.Clean(t); t == mine || strings.HasPrefix(t, mine+"/") {
				return deny("the tools server's %s is the run's own tmpfs; the gateway mounts nothing at %s", mine, t)
			}
		}
	}
	if hc.Tmpfs == nil {
		hc.Tmpfs = map[string]string{}
	}
	for target, opts := range f.cfg.ToolsTmpfs {
		hc.Tmpfs[target] = opts
	}
	return nil
}

// toolsShared is Config.ToolsShared with each host folder resolved as the
// storage check resolves a bind's source, so the two compare as equals.
func (f *Filter) toolsShared() (map[string]string, error) {
	out := map[string]string{}
	for target, src := range f.cfg.ToolsShared {
		resolved, err := f.hostPath(src, true)
		if err != nil {
			return nil, err
		}
		out[path.Clean(target)] = resolved
	}
	return out, nil
}

// pinLinks gives a container that will run under the run's runtime an
// /etc/hosts entry for each container it links to: the link's alias at
// that container's address on a network the two share. A sandboxed
// runtime's own network stack does not reach docker's embedded DNS, which
// is what answers a link's name (observed under gVisor), so
// a declared server would not find its relay; the entry answers exactly
// what the DNS would have. The links stay, and every linked container has
// already been confirmed this run's gateway's (checkHost, checkNetworks).
// A link to a container on none of this one's networks, or with no
// address there yet, is refused rather than left to fail inside the run.
func (f *Filter) pinLinks(ctx context.Context, c *containerCreateBody) error {
	hc := c.HostConfig
	joins := map[string]bool{}
	if m := hc.NetworkMode; m != "" && m != "none" && m != "default" {
		joins[m] = true
	}
	links := append([]string(nil), hc.Links...)
	if nc := c.NetworkingConfig; nc != nil {
		for id, ep := range nc.EndpointsConfig {
			joins[id] = true
			if ep != nil {
				links = append(links, ep.Links...)
			}
		}
	}
	for _, l := range links {
		name, alias, hasAlias := strings.Cut(l, ":")
		name = strings.TrimPrefix(name, "/")
		if !hasAlias {
			alias = name
		}
		// Docker's own form of a stored link is "/name:/container/alias".
		alias = alias[strings.LastIndex(alias, "/")+1:]
		if !hostNameRE.MatchString(alias) {
			return deny("link alias %q is not a host name", alias)
		}
		// A link makes the linked container answer to its name as well as
		// the alias, and the gateway uses both: a server reaches its L4
		// relay by the declared host (the alias), the tools server its L7
		// relay by the relay's name in http_proxy.
		if !hostNameRE.MatchString(name) {
			return deny("linked container name %q is not a host name", name)
		}
		ip, err := f.linkedAddress(ctx, name, joins)
		if err != nil {
			return err
		}
		// docker's CLI writes a link on a user-defined network in both
		// places; one entry per name answers it.
		for _, n := range []string{alias, name} {
			if entry := n + ":" + ip; !slices.Contains(hc.ExtraHosts, entry) {
				hc.ExtraHosts = append(hc.ExtraHosts, entry)
			}
		}
	}
	return nil
}

// hostNameRE is a host name and nothing else, one line: what a relay may
// be pointed at (its entrypoint writes it into the proxy's configuration)
// and what a pinned /etc/hosts entry may name.
var hostNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// relayPortsRE is an L4 relay's other setting, held to its form likewise;
// relayPortRE one port of an L7 relay's host:port.
var (
	relayPortsRE = regexp.MustCompile(`^[0-9]{1,5}(,[0-9]{1,5})*$`)
	relayPortRE  = regexp.MustCompile(`^[0-9]{1,5}$`)
)

// resolvConfTarget is where a container reads its name servers.
const resolvConfTarget = "/etc/resolv.conf"

// webShape holds a container on the run's web network to the web tools'
// server (Config.WebNetwork): its image's own entrypoint, the one command
// WebCmd, the run's tools spec read-only at WebSpecTarget, the web network
// and no other, no link, and name resolution as the run gives it (no
// resolv.conf, DNS setting or hosts entry of the gateway's). The network
// reaches past the host, and the server's own address rule is what bounds
// it; any other code there would be bounded by nothing. It answers whether
// the container is that server.
func (f *Filter) webShape(ctx context.Context, c *containerCreateBody) (bool, error) {
	if f.cfg.WebNetwork == "" {
		return false, nil
	}
	web, err := f.ownNetwork(ctx, f.cfg.WebNetwork, true)
	if err != nil {
		return false, err
	}
	hc := c.HostConfig
	joins := hc.NetworkMode == web.id
	others := hc.NetworkMode != "" && hc.NetworkMode != web.id && hc.NetworkMode != "none" && hc.NetworkMode != "default"
	if nc := c.NetworkingConfig; nc != nil {
		for id, ep := range nc.EndpointsConfig {
			if id == web.id {
				joins = true
			} else {
				others = true
			}
			if ep != nil && len(ep.Links) > 0 {
				others = true
			}
		}
	}
	if !joins {
		return false, nil
	}
	var cmd []string
	switch {
	case others || len(hc.Links) > 0:
		return false, deny("the web tools' server joins the web network and no other, with no link")
	case present(c.Entrypoint) || present(c.Shell) || present(c.Healthcheck) || present(c.OnBuild):
		return false, deny("the web tools' server runs its image's own entrypoint, nothing else")
	case json.Unmarshal(c.Cmd, &cmd) != nil || !slices.Equal(cmd, f.cfg.WebCmd):
		return false, deny("the web tools' server runs its tools role with the run's spec, nothing else")
	case c.User != "" || c.WorkingDir != "":
		return false, deny("the web tools' server's user and working directory are its image's")
	case len(hc.DNS) > 0 || len(hc.DNSSearch) > 0 || len(hc.DNSOptions) > 0 || len(hc.ExtraHosts) > 0:
		// Each would decide what a page's host name resolves to.
		return false, deny("the web tools' server resolves names as the run does")
	}
	// Nothing may sit over /etc, where its name resolution is read, but
	// its own spec: no declared volume, tmpfs or mount there. Elsewhere the
	// storage rules hold as for any container (the pinned gateway declares
	// its binds' targets in Volumes too, as docker's clients do).
	var declared map[string]json.RawMessage
	if present(c.Volumes) {
		if err := json.Unmarshal(c.Volumes, &declared); err != nil {
			return false, deny("the web tools' server's Volumes are a map of targets")
		}
	}
	for target := range declared {
		if path.Clean(target) != f.cfg.WebSpecTarget && coversResolvConf(target) {
			return false, deny("the web tools' server resolves names as the run does")
		}
	}
	for target := range hc.Tmpfs {
		if coversResolvConf(target) {
			return false, deny("the web tools' server resolves names as the run does")
		}
	}
	spec := false
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if len(parts) >= 2 && path.Clean(parts[1]) == f.cfg.WebSpecTarget {
			if len(parts) != 3 || !slices.Contains(strings.Split(parts[2], ","), "ro") || !f.sameFile(parts[0], f.cfg.WebSpec) {
				return false, deny("the web tools' server reads the run's own tools spec, read-only")
			}
			spec = true
			continue
		}
		if len(parts) >= 2 && coversResolvConf(parts[1]) {
			return false, deny("the web tools' server resolves names as the run does")
		}
	}
	for _, m := range hc.Mounts {
		if path.Clean(m.Target) == f.cfg.WebSpecTarget {
			return false, deny("the web tools' server reads the run's own tools spec, read-only")
		}
		if coversResolvConf(m.Target) {
			return false, deny("the web tools' server resolves names as the run does")
		}
	}
	if !spec {
		return false, deny("the web tools' server reads the run's own tools spec, read-only")
	}
	return true, nil
}

// coversResolvConf says whether a mount at target would decide the
// container's name resolution: anything under /etc (resolv.conf, hosts,
// nsswitch.conf, gai.conf), /etc itself or the root. The web tools'
// server's own spec, under /etc/agentry, is checked on its own first.
func coversResolvConf(target string) bool {
	t := path.Clean(target)
	return t == "/" || t == "/etc" || strings.HasPrefix(t, "/etc/")
}

// sameFile says whether two host paths name one file.
func (f *Filter) sameFile(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	return err == nil && ra == rb
}

// relayShape holds a relay, the one kind of container with a way off the
// host, to what the pinned gateway gives one: its image's own entrypoint
// and command, no files, and its targets as settings, each one the run
// declared (its servers' egress, and the agent's own on 443). The pin says
// which relay it is, and so which settings it takes.
func (f *Filter) relayShape(c *containerCreateBody, pin string) error {
	hc := c.HostConfig
	switch {
	case present(c.Entrypoint) || present(c.Cmd) || present(c.Shell) || present(c.Healthcheck) || present(c.OnBuild):
		return deny("a relay runs its image's own entrypoint and command, nothing else")
	case c.User != "" || c.WorkingDir != "":
		return deny("a relay's user and working directory are its image's")
	case present(c.Volumes) || len(hc.Binds) > 0 || len(hc.Mounts) > 0 || len(hc.Tmpfs) > 0:
		return deny("a relay takes no files")
	case len(hc.ExtraHosts) > 0 || len(hc.DNS) > 0 || len(hc.DNSSearch) > 0 || len(hc.DNSOptions) > 0 || len(hc.Links) > 0:
		// Each would decide what the declared host name resolves to.
		return deny("a relay resolves its host as the daemon does")
	}
	if nc := c.NetworkingConfig; nc != nil {
		for _, ep := range nc.EndpointsConfig {
			if ep != nil && len(ep.Links) > 0 {
				return deny("a relay resolves its host as the daemon does")
			}
		}
	}
	// The pinned gateway's two relays take their targets as settings, each
	// its own form, bound to its image: the L4 relay one host and its ports
	// (PROXY_HOSTNAME, PROXY_PORTS), the L7 relay a list of host:port it
	// may CONNECT to (ALLOWED_HOSTS), which is how the tools server's fetch
	// reaches the agent's declared hosts. Nothing else,
	// and every target declared.
	settings := map[string]string{}
	for _, kv := range c.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "PROXY_HOSTNAME", "PROXY_PORTS", "ALLOWED_HOSTS":
			if _, dup := settings[k]; dup {
				return deny("relay setting %s given twice", k)
			}
			settings[k] = v
		default:
			return deny("relay setting %s", k)
		}
	}
	if strings.Contains(pin, "/mcp-l7proxy@") {
		allowed, ok := settings["ALLOWED_HOSTS"]
		if !ok || len(settings) != 1 {
			return deny("an L7 relay takes ALLOWED_HOSTS and nothing else")
		}
		return f.l7Targets(allowed)
	}
	if _, ok := settings["ALLOWED_HOSTS"]; ok {
		return deny("an L4 relay takes PROXY_HOSTNAME and PROXY_PORTS, not ALLOWED_HOSTS")
	}
	host, ports := settings["PROXY_HOSTNAME"], settings["PROXY_PORTS"]
	if !hostNameRE.MatchString(host) {
		return deny("relay host %q is not a host name", host)
	}
	if !relayPortsRE.MatchString(ports) {
		return deny("relay ports %q are not ports", ports)
	}
	for _, p := range strings.Split(ports, ",") {
		if !f.relayAllowed(host, p) {
			return deny("relay to %s port %s is not one the run declared", host, p)
		}
	}
	return nil
}

// l7Targets holds the L7 relay's ALLOWED_HOSTS to what the run declared:
// a comma-separated list of host:port, each a host name and a port the
// run's egress covers.
func (f *Filter) l7Targets(allowed string) error {
	if allowed == "" {
		return deny("an L7 relay with no ALLOWED_HOSTS")
	}
	for _, target := range strings.Split(allowed, ",") {
		host, port, ok := strings.Cut(target, ":")
		if !ok || !hostNameRE.MatchString(host) || !relayPortRE.MatchString(port) {
			return deny("relay target %q is not host:port", target)
		}
		if !f.relayAllowed(host, port) {
			return deny("relay to %s port %s is not one the run declared", host, port)
		}
	}
	return nil
}

// relayAllowed says whether a relay to host:port is declared: an entry
// "host:port" allows that port, an entry "host" the web's two (the ports a
// declaration without one means); "*" and "*.domain" match hosts the same
// way. No entries, no relay.
func (f *Filter) relayAllowed(host, port string) bool {
	for _, entry := range f.cfg.RelayHosts {
		h, p, hasPort := strings.Cut(entry, ":")
		switch {
		case hasPort && p != port:
			continue
		case !hasPort && port != "443" && port != "80":
			continue
		}
		switch {
		case h == "*" || strings.EqualFold(h, host):
			return true
		case strings.HasPrefix(h, "*.") && len(host) > len(h)-1 && strings.EqualFold(host[len(host)-len(h)+1:], h[1:]):
			return true
		}
	}
	return false
}

// checkHost refuses what would reach the host past the container.
func (f *Filter) checkHost(ctx context.Context, hc *hostConfig) error {
	switch {
	case hc.Privileged:
		return deny("a privileged container")
	case len(hc.CapAdd) > 0:
		return deny("HostConfig.CapAdd %v", hc.CapAdd)
	case !empty(hc.Devices) || !empty(hc.DeviceRequests) || !empty(hc.DeviceCgroupRules):
		return deny("a container with host devices")
	case len(hc.VolumesFrom) > 0:
		return deny("HostConfig.VolumesFrom")
	case len(hc.Sysctls) > 0:
		return deny("HostConfig.Sysctls")
	case hc.PublishAllPorts || !empty(hc.PortBindings):
		return deny("a container publishing ports on the host")
	case hc.CgroupParent != "" || hc.Cgroup != "":
		return deny("a container placed in another cgroup")
	case hc.Runtime != "":
		// The run's runtime is the policy's, set by the filter itself
		// (Config.Runtime): the gateway does not get to pick another.
		return deny("HostConfig.Runtime %q: the run's runtime is the policy's, not the gateway's to choose", hc.Runtime)
	case hc.Isolation != "" && hc.Isolation != "default":
		return deny("HostConfig.Isolation %q", hc.Isolation)
	case present(hc.MaskedPaths) || present(hc.ReadonlyPaths):
		// Any value replaces the daemon's list; an empty one masks nothing.
		return deny("a container unmasking the kernel's paths")
	case len(hc.Annotations) > 0:
		return deny("runtime annotations")
	case len(hc.StorageOpt) > 0 || hc.VolumeDriver != "":
		return deny("a storage or volume driver option")
	case !empty(hc.BlkioWeightDevice) || !empty(hc.BlkioDeviceReadBps) || !empty(hc.BlkioDeviceWriteBps) ||
		!empty(hc.BlkioDeviceReadIOps) || !empty(hc.BlkioDeviceWriteIOps):
		return deny("per-device IO settings")
	case hc.OomScoreAdj < 0:
		// Below zero shields it from the OOM killer at others' expense.
		return deny("an OOM score below the default")
	case !empty(hc.Ulimits):
		return deny("ulimits (realtime priority and locked memory among them)")
	case hc.CPUShares > 1024:
		return deny("a CPU share above the default weight")
	case hc.OomKillDisable != nil && *hc.OomKillDisable:
		return deny("a container the OOM killer may not end")
	}
	if hc.LogConfig != nil {
		switch hc.LogConfig.Type {
		case "", "json-file", "local", "none":
		default:
			return deny("log driver %q (it can carry a container's output off the host)", hc.LogConfig.Type)
		}
		if len(hc.LogConfig.Config) > 0 {
			return deny("log driver options")
		}
	}
	if rp := hc.RestartPolicy; rp != nil && rp.Name != "" && rp.Name != "no" {
		return deny("restart policy %q: a run's containers end with it", rp.Name)
	}
	for _, o := range hc.SecurityOpt {
		if !strengthens(o) {
			return deny("HostConfig.SecurityOpt %q", o)
		}
	}
	for field, mode := range map[string]string{
		"PidMode": hc.PidMode, "IpcMode": hc.IpcMode, "UTSMode": hc.UTSMode,
		"UsernsMode": hc.UsernsMode, "CgroupnsMode": hc.CgroupnsMode,
	} {
		if err := namespaceMode(field, mode); err != nil {
			return err
		}
	}
	for _, l := range hc.Links {
		name, _, _ := strings.Cut(l, ":")
		if _, err := f.ownContainer(ctx, strings.TrimPrefix(name, "/")); err != nil {
			return err
		}
	}
	return nil
}

// strengthens says whether a security option only tightens the default:
// the allowlist is no-new-privileges, spelled as docker accepts it.
func strengthens(opt string) bool {
	o := strings.ToLower(strings.ReplaceAll(opt, ":", "="))
	return o == "no-new-privileges" || o == "no-new-privileges=true"
}

// present says whether a field was given at all, even as [] or {}.
func present(r json.RawMessage) bool {
	s := string(bytes.TrimSpace(r))
	return s != "" && s != "null"
}

func empty(r json.RawMessage) bool {
	s := string(bytes.TrimSpace(r))
	return s == "" || s == "null" || s == "[]" || s == "{}"
}

func namespaceMode(field, mode string) error {
	switch mode {
	case "", "private", "shareable", "none":
		return nil
	}
	// "host" reaches the host; "container:<id>" joins another container's
	// namespace, a relay's or the runner's own among them.
	return deny("HostConfig.%s %q", field, mode)
}

// checkNetworks judges the networks a container joins (checkNetworkList)
// and then gives one that names no primary network a primary of the
// run's, since the daemon would put it on its default bridge: the one
// network it names as an endpoint, or none.
func (f *Filter) checkNetworks(ctx context.Context, c *containerCreateBody, scope browserScope) error {
	if err := f.checkNetworkList(ctx, c, scope); err != nil {
		return err
	}
	hc := c.HostConfig
	if hc.NetworkMode != "" && hc.NetworkMode != "default" {
		return nil
	}
	var eps []string
	if c.NetworkingConfig != nil {
		for id := range c.NetworkingConfig.EndpointsConfig {
			eps = append(eps, id)
		}
	}
	switch len(eps) {
	case 0:
		hc.NetworkMode = "none"
	case 1:
		hc.NetworkMode = eps[0]
	default:
		// Which of them would be primary is the gateway's to say, not the
		// filter's to guess.
		return deny("a container that joins several networks names which one is its own")
	}
	return nil
}

// checkNetworkList resolves every network the container joins and applies
// the rules to its name: the run's or a shared one, and an egress network
// only for a pinned relay.
func (f *Filter) checkNetworkList(ctx context.Context, c *containerCreateBody, scope browserScope) error {
	// The web tools' server asks for the daemon's default network; it gets
	// the run's web network (Config.WebNetwork), and nothing else does.
	web := false
	if f.cfg.WebNetwork != "" && c.Image != "" {
		id, err := f.webImage(ctx)
		web = err == nil && c.Image == id
	}
	netName := func(name string) string {
		name = f.runNetwork(name)
		if web && (name == "" || name == "default") {
			return f.cfg.WebNetwork
		}
		if scope != notBrowser && name == f.cfg.BrowserNetworkFor {
			return f.cfg.BrowserNetwork
		}
		return name
	}
	mode := netName(c.HostConfig.NetworkMode)
	switch {
	case mode == "" || mode == "none" || mode == "default":
	case mode == "host":
		return deny("host networking")
	case mode == "bridge":
		return deny("the daemon's default bridge")
	case strings.HasPrefix(mode, "container:"):
		// Another container's network namespace: a relay's would carry
		// its way off the host.
		return deny("a container in another's network namespace")
	default:
		n, err := f.ownNetwork(ctx, mode, true)
		if err != nil {
			return err
		}
		if err := f.egressAllowed(ctx, n.name, c.Image); err != nil {
			return err
		}
		if err := f.browserNetworkAllowed(scope, n.name); err != nil {
			return err
		}
		c.HostConfig.NetworkMode = n.id
	}
	if c.NetworkingConfig == nil {
		return nil
	}
	byID := map[string]*endpointSettings{}
	for key, ep := range c.NetworkingConfig.EndpointsConfig {
		if name := netName(key); name == "default" || name == "bridge" {
			// The daemon reads an endpoint named "default" as its bridge,
			// and "bridge" is the bridge by its own name.
			return deny("the daemon's default bridge")
		}
		n, err := f.ownNetwork(ctx, netName(key), true)
		if err != nil {
			return err
		}
		if err := f.egressAllowed(ctx, n.name, c.Image); err != nil {
			return err
		}
		if err := f.browserNetworkAllowed(scope, n.name); err != nil {
			return err
		}
		if ep != nil {
			if len(ep.DriverOpts) > 0 {
				return deny("network driver options on %s", n.name)
			}
			if ep.NetworkID != "" {
				if other, err := f.ownNetwork(ctx, ep.NetworkID, true); err != nil || other.id != n.id {
					return deny("endpoint %s names another network's id", n.name)
				}
				ep.NetworkID = n.id
			}
			for _, l := range ep.Links {
				name, _, _ := strings.Cut(l, ":")
				if _, err := f.ownContainer(ctx, strings.TrimPrefix(name, "/")); err != nil {
					return err
				}
			}
		}
		byID[n.id] = ep
	}
	c.NetworkingConfig.EndpointsConfig = byID
	return nil
}

// checkStorage requires every bind and mount to stay in the run: host
// paths under the gateway's roots, written only under its writable ones,
// forwarded as resolved; volumes the run's, or for the tools server one of
// Config.ToolsVolumes; no driver configuration.
func (f *Filter) checkStorage(ctx context.Context, hc *hostConfig, who mounter) error {
	for i, b := range hc.Binds {
		out, err := f.bind(ctx, b, who)
		if err != nil {
			return err
		}
		hc.Binds[i] = out
	}
	for i := range hc.Mounts {
		if err := f.mount(ctx, &hc.Mounts[i], who); err != nil {
			return err
		}
	}
	return nil
}

// volume admits a named volume: the run's own; for the run's tools server
// alone, one Config.ToolsVolumes names exactly; for the app link's
// container alone, Config.AppLinkVolume; for the containers a held volume
// names alone, that volume (Config.HeldVolumes). Each must exist (the daemon
// would make a missing one, unlabelled, at the create).
func (f *Filter) volume(ctx context.Context, name string, who mounter) error {
	held := false
	for _, h := range f.cfg.HeldVolumes {
		if h.Root != name {
			continue
		}
		ok, err := f.holds(ctx, who, h.By)
		if err != nil {
			return err
		}
		if !ok {
			return deny("volume %s is held to the run's container that needs it", name)
		}
		held = true
	}
	if held || who.tools && slices.Contains(f.cfg.ToolsVolumes, name) || who.appLink && name == f.cfg.AppLinkVolume {
		var v struct{ Name string }
		code, err := f.inspect(ctx, "/volumes/"+url.PathEscape(name), &v)
		if err != nil {
			return err
		}
		if code == http.StatusNotFound {
			return notFound{"volume", name}
		}
		return nil
	}
	return f.ownVolume(ctx, name)
}

// bindOptionsAllowed are the options of a "source:target:options" bind
// that keep it inside the run; propagation to or from the host and
// relabelling are not among them.
var bindOptionsAllowed = map[string]bool{
	"ro": true, "rw": true, "nocopy": true, "private": true, "rprivate": true,
	"consistent": true, "cached": true, "delegated": true,
}

func (f *Filter) bind(ctx context.Context, b string, who mounter) (string, error) {
	parts := strings.Split(b, ":")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" {
		return "", deny("bind %q is not source:target[:options]", b)
	}
	src := parts[0]
	write := true
	if len(parts) == 3 {
		for _, o := range strings.Split(parts[2], ",") {
			if !bindOptionsAllowed[o] {
				return "", deny("bind option %q", o)
			}
			if o == "ro" {
				write = false
			}
		}
	}
	if !strings.HasPrefix(src, "/") {
		return b, f.volume(ctx, src, who)
	}
	resolved, err := f.hostPath(src, write)
	if err != nil {
		return "", err
	}
	if err := f.held(ctx, resolved, write, who); err != nil {
		return "", err
	}
	parts[0] = resolved
	return strings.Join(parts, ":"), nil
}

// held refuses a bind of a root the run's tools server alone may bind
// (Config.ToolsOnlyRoots), or bind writable (Config.ToolsWritableRoots),
// by any other container; of one only an offline container may bind
// (Config.OfflineRoots) by another that joins a network; and of a held
// root (Config.HeldRoots) by a container it does not name. resolved is the
// bind's resolved source. A source at, beneath or above a root is judged as
// the root: a folder that holds one would give the container what it
// holds.
func (f *Filter) held(ctx context.Context, resolved string, write bool, who mounter) error {
	touches := func(root string) bool { return touchesRoot(resolved, root) }
	anyOf := func(roots []string) bool {
		return slices.ContainsFunc(roots, touches)
	}
	if !who.tools {
		switch {
		case anyOf(f.cfg.ToolsOnlyRoots):
			return deny("bind source %s is one only the run's tools server may bind", resolved)
		case write && anyOf(f.cfg.ToolsWritableRoots):
			return deny("bind source %s is one only the run's tools server may write", resolved)
		case !who.offline && anyOf(f.cfg.OfflineRoots):
			return deny("bind source %s is given only to a container that joins no network", resolved)
		}
	}
	for _, h := range f.cfg.HeldRoots {
		if !touches(h.Root) {
			continue
		}
		ok, err := f.holds(ctx, who, h.By)
		if err != nil {
			return err
		}
		if !ok {
			return deny("bind source %s is held to the run's container that needs it", resolved)
		}
	}
	return nil
}

// touchesRoot says whether a resolved path is at, beneath or above root
// (with its links followed): a folder that holds a root would give what the
// root holds.
func touchesRoot(resolved, root string) bool {
	r, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return false
	}
	return within(resolved, r) || within(r, resolved)
}

// keepsRootsFromGateway says whether a container of the run binds a path at,
// beneath or above a root the filter holds to particular containers (a
// held root, a root only the tools server may bind, or one only an offline
// container may), or mounts a held volume.
func (f *Filter) keepsRootsFromGateway(c container) bool {
	for _, h := range f.cfg.HeldVolumes {
		if slices.Contains(c.volumes, h.Root) {
			return true
		}
	}
	roots := slices.Concat(f.cfg.ToolsOnlyRoots, f.cfg.ToolsWritableRoots, f.cfg.OfflineRoots)
	for _, h := range f.cfg.HeldRoots {
		roots = append(roots, h.Root)
	}
	for _, src := range c.sources {
		resolved := src
		if r, err := filepath.EvalSymlinks(filepath.Clean(src)); err == nil {
			resolved = r
		}
		if slices.ContainsFunc(roots, func(root string) bool { return touchesRoot(resolved, root) }) {
			return true
		}
	}
	return false
}

// within says whether path p is root or beneath it.
func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// holds says whether the create who is is one of by: its image, by id, a
// holder's, run by the image's own entrypoint with that holder's command.
func (f *Filter) holds(ctx context.Context, who mounter, by []Holder) (bool, error) {
	c := who.create
	if c == nil || present(c.Entrypoint) || present(c.Shell) {
		return false, nil
	}
	var cmd []string
	if present(c.Cmd) && json.Unmarshal(c.Cmd, &cmd) != nil {
		return false, nil
	}
	var got []string
	gotKnown := false
	for _, h := range by {
		if !slices.Equal(cmd, h.Cmd) {
			continue
		}
		id, err := f.holderImage(ctx, h.Image)
		if err != nil {
			return false, err
		}
		if id == "" || id != who.imageID {
			continue
		}
		if !gotKnown {
			if got, err = createdBinds(c); err != nil {
				return false, nil //nolint:nilerr // a create whose folders cannot be read is not the holder
			}
			gotKnown = true
		}
		if want := holderBinds(h.Binds); slices.Equal(got, want) {
			return true, nil
		}
	}
	return false, nil
}

// bindKey spells one place a container is given something: where it
// comes from and where it lands. A host folder is its resolved path, as the
// daemon mounts it; a named volume, an anonymous one and a tmpfs have their
// own spellings, so none stands in for another.
func bindKey(source, target string) string {
	return source + "\x00" + path.Clean(target)
}

// resolvedSource is a bind source as the daemon mounts it: an absolute path
// with its links followed, a named volume as its name. A path that cannot be
// resolved is kept as written, which no resolved source equals.
func resolvedSource(src string) string {
	if !strings.HasPrefix(src, "/") {
		return "volume:" + src
	}
	if r, err := filepath.EvalSymlinks(filepath.Clean(src)); err == nil {
		return r
	}
	return filepath.Clean(src)
}

// createdBinds is everything a create gives its container to mount, sorted
// and without repeats: its binds, its mounts of every kind, its tmpfs and
// the volumes its config declares.
func createdBinds(c *containerCreateBody) ([]string, error) {
	var keys []string
	if c.HostConfig != nil {
		for _, b := range c.HostConfig.Binds {
			parts := strings.Split(b, ":")
			if len(parts) < 2 || len(parts) > 3 || parts[0] == "" {
				return nil, fmt.Errorf("bind %q is not source:target[:options]", b)
			}
			keys = append(keys, bindKey(resolvedSource(parts[0]), parts[1]))
		}
		for _, m := range c.HostConfig.Mounts {
			switch m.Type {
			case "bind", "volume":
				if m.Source == "" {
					keys = append(keys, bindKey("anonymous", m.Target))
				} else {
					keys = append(keys, bindKey(resolvedSource(m.Source), m.Target))
				}
			default:
				keys = append(keys, bindKey(m.Type, m.Target))
			}
		}
		for target := range c.HostConfig.Tmpfs {
			keys = append(keys, bindKey("tmpfs", target))
		}
	}
	if present(c.Volumes) {
		var declared map[string]json.RawMessage
		if err := json.Unmarshal(c.Volumes, &declared); err != nil {
			return nil, err
		}
		for target := range declared {
			keys = append(keys, bindKey("anonymous", target))
		}
	}
	return sortedSet(keys), nil
}

// holderBinds is a holder's binds, spelled as createdBinds spells a create's.
func holderBinds(binds []HolderBind) []string {
	keys := make([]string, 0, len(binds))
	for _, b := range binds {
		keys = append(keys, bindKey(resolvedSource(b.Source), b.Target))
	}
	return sortedSet(keys)
}

func sortedSet(keys []string) []string {
	slices.Sort(keys)
	return slices.Compact(keys)
}

// holderImage is a holder's image resolved to its id, kept once it
// resolved; "" for an image the daemon does not hold, which no create can
// be of.
func (f *Filter) holderImage(ctx context.Context, ref string) (string, error) {
	f.webMu.Lock()
	defer f.webMu.Unlock()
	if id, ok := f.holderImageIDs[ref]; ok {
		return id, nil
	}
	id, err := f.resolveImage(ctx, ref)
	var nf notFound
	if errors.As(err, &nf) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if f.holderImageIDs == nil {
		f.holderImageIDs = map[string]string{}
	}
	f.holderImageIDs[ref] = id
	return id, nil
}

func (f *Filter) mount(ctx context.Context, m *mount, who mounter) error {
	if !empty(m.ImageOptions) || !empty(m.ClusterOptions) {
		return deny("a %q mount with image or cluster options", m.Type)
	}
	switch m.Type {
	case "tmpfs":
		if m.TmpfsOptions != nil && !empty(m.TmpfsOptions.Options) {
			return deny("tmpfs mount options")
		}
		return nil
	case "volume":
		if m.VolumeOptions != nil && !empty(m.VolumeOptions.DriverConfig) {
			// local's options mount devices and host paths.
			return deny("a volume mount with driver configuration")
		}
		if m.Source == "" {
			return nil // anonymous: created for, and removed with, the container
		}
		return f.volume(ctx, m.Source, who)
	case "bind":
		if bo := m.BindOptions; bo != nil {
			if bo.Propagation != "" && bo.Propagation != "private" && bo.Propagation != "rprivate" {
				return deny("bind propagation %q", bo.Propagation)
			}
			if bo.CreateMountpoint {
				return deny("a bind that creates its host path")
			}
		}
		resolved, err := f.hostPath(m.Source, !m.ReadOnly)
		if err != nil {
			return err
		}
		if err := f.held(ctx, resolved, !m.ReadOnly, who); err != nil {
			return err
		}
		m.Source = resolved
		return nil
	}
	return deny("a %q mount", m.Type)
}

// hostPath resolves symlinks on the host, where the daemon will, requires
// the result under a bind root (a writable one if write), and returns it:
// the daemon mounts the resolved path, not a link that could be swapped.
func (f *Filter) hostPath(src string, write bool) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Clean(src))
	if err != nil {
		return "", deny("bind source %s: %v", src, err)
	}
	// Under a root the run's containers can write, a source must be that
	// root itself: a directory inside it could be swapped for a symlink
	// between this check and the daemon's mount at start, and point
	// anywhere on the host. The roots' own path components are the
	// runner's, which no container writes.
	for _, root := range f.cfg.WritableRoots {
		r, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		if resolved != r && strings.HasPrefix(resolved, r+string(filepath.Separator)) {
			return "", deny("bind source %s is inside a directory the run writes; only the directory itself may be bound", src)
		}
	}
	roots := f.cfg.BindRoots
	if write {
		roots = f.cfg.WritableRoots
	}
	for _, root := range roots {
		r, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			continue
		}
		if resolved == r || strings.HasPrefix(resolved, r+string(filepath.Separator)) {
			return resolved, nil
		}
	}
	if write {
		return "", deny("bind source %s is not one the run lets the gateway write", src)
	}
	return "", deny("bind source %s is outside the run's directories", src)
}

// bound applies the run's ceilings: more is refused, none gets them.
func (f *Filter) bound(hc *hostConfig) error {
	lim := f.cfg.Limits
	if lim.MemoryBytes > 0 {
		switch {
		case hc.Memory == 0:
			hc.Memory = lim.MemoryBytes
		case hc.Memory > lim.MemoryBytes:
			return deny("memory %d above the run's %d", hc.Memory, lim.MemoryBytes)
		}
		// Swap is bounded with memory: -1 (unlimited), 0 (twice the
		// memory) and anything larger all become none beyond it.
		if hc.MemorySwap != hc.Memory {
			hc.MemorySwap = hc.Memory
		}
		if hc.ShmSize > hc.Memory {
			return deny("shared memory %d above the run's memory %d", hc.ShmSize, hc.Memory)
		}
	}
	if lim.NanoCPUs > 0 {
		if hc.CPUQuota != 0 || hc.CPUPeriod != 0 || hc.CPURealtimeRuntime != 0 || hc.CPURealtimePeriod != 0 {
			return deny("CPU quota fields: the run bounds CPU by NanoCpus")
		}
		switch {
		case hc.NanoCPUs == 0:
			hc.NanoCPUs = lim.NanoCPUs
		case hc.NanoCPUs > lim.NanoCPUs:
			return deny("CPU %d above the run's %d", hc.NanoCPUs, lim.NanoCPUs)
		}
	}
	if lim.Pids > 0 {
		switch {
		case hc.PidsLimit == nil || *hc.PidsLimit <= 0:
			p := lim.Pids
			hc.PidsLimit = &p
		case *hc.PidsLimit > lim.Pids:
			return deny("pids %d above the run's %d", *hc.PidsLimit, lim.Pids)
		}
	}
	return nil
}

func (f *Filter) networkCreate(ctx context.Context, body []byte) (any, error) {
	var n networkCreateBody
	if err := decodeStrict(body, &n); err != nil {
		return nil, err
	}
	asked := n.Name
	n.Name = f.runNetwork(n.Name)
	if f.shared(n.Name) {
		// The runner makes the shared networks; a gateway asking for one
		// gets the daemon's answer when it is there, and a refusal when
		// it is not (the runner should have made it).
		if _, err := f.ownNetwork(ctx, n.Name, true); err == nil {
			return nil, exists{asked}
		}
		return nil, deny("network %s is the runner's for this run; the runner makes it", n.Name)
	}
	switch {
	case strings.HasPrefix(n.Name, "agentry-"):
		// The runner's namespace: a gateway taking one of its names could
		// keep another run from starting.
		return nil, deny("network name %s is the runner's", n.Name)
	case n.Driver != "" && n.Driver != "bridge":
		return nil, deny("a %q network", n.Driver)
	case !n.Internal:
		// A network of the gateway's own with a way out would give any
		// container on it the egress the relay rule withholds.
		return nil, deny("a network that is not internal")
	case n.Scope != "" && n.Scope != "local":
		return nil, deny("a %q scoped network", n.Scope)
	case n.Ingress || n.ConfigOnly || !empty(n.ConfigFrom):
		return nil, deny("an ingress or config network")
	case len(n.Options) > 0:
		return nil, deny("network driver options")
	case n.EnableIPv6 != nil && *n.EnableIPv6:
		return nil, deny("an IPv6 network")
	case n.IPAM != nil && ((n.IPAM.Driver != "" && n.IPAM.Driver != "default") || !empty(n.IPAM.Config) || len(n.IPAM.Options) > 0):
		return nil, deny("a network with its own address plan")
	}
	n.Labels = f.label(n.Labels)
	return n, nil
}

func (f *Filter) volumeCreate(body []byte) (any, error) {
	var v volumeCreateBody
	if err := decodeStrict(body, &v); err != nil {
		return nil, err
	}
	switch {
	case v.Driver != "" && v.Driver != "local":
		return nil, deny("a %q volume", v.Driver)
	case len(v.DriverOpts) > 0:
		// local's options mount devices and host paths.
		return nil, deny("volume driver options")
	case !empty(v.ClusterVolumeSpec):
		return nil, deny("a cluster volume")
	}
	v.Labels = f.label(v.Labels)
	return v, nil
}

func (f *Filter) execCreate(body []byte) (any, error) {
	var e execCreateBody
	if err := decodeStrict(body, &e); err != nil {
		return nil, err
	}
	if e.Privileged {
		return nil, deny("a privileged exec")
	}
	return e, nil
}

// connect requires the container a connect or disconnect names to be the
// run's, and one joining an egress network to be a pinned relay.
func (f *Filter) connect(r *http.Request, network string, joining bool) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return deny("connect body: %v", err)
	}
	_ = r.Body.Close()
	var doc struct {
		Container      string
		EndpointConfig *endpointSettings
		Force          bool
	}
	if err := decodeStrict(body, &doc); err != nil {
		return err
	}
	if doc.Container == "" {
		return deny("connect body names no container")
	}
	c, err := f.ownContainer(r.Context(), doc.Container)
	if err != nil {
		return err
	}
	if joining {
		if err := f.egressAllowed(r.Context(), network, c.imageID); err != nil {
			return err
		}
		// The browser's network takes no container after it is made, and
		// the browser server joins no network.
		if f.cfg.BrowserNetwork != "" && network == f.cfg.BrowserNetwork {
			return deny("the browser's network takes no container after it is made")
		}
		if browser, err := f.isBrowser(r.Context(), c.imageID); err != nil || browser {
			return deny("the browser server joins no network after it is made")
		}
		// A container under the run's runtime takes its networks before
		// it starts: a sandboxed runtime's network stack takes none
		// connected once it runs, so the join would
		// succeed and leave it with no way there.
		if c.running && f.cfg.Runtime != "" && c.runtime == f.cfg.Runtime {
			return deny("container %s is running under %s, which takes no network connected after start", short(c.id), f.cfg.Runtime)
		}
		if ep := doc.EndpointConfig; ep != nil && len(ep.DriverOpts) > 0 {
			return deny("network driver options on %s", network)
		}
	}
	doc.Container = c.id
	out, err := json.Marshal(doc)
	if err != nil {
		return deny("connect body: %v", err)
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", fmt.Sprint(len(out)))
	r.Header.Del("Transfer-Encoding")
	r.TransferEncoding = nil
	return nil
}

package main

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	StreamTypeHTTP       = 0x01
	StreamTypeUDP        = 0x02
	StreamTypeTCP        = 0x03
	StreamTypeTCPMapped  = 0x04
	RawUDPMagicHeartbeat = 0x01
	RawUDPMagicData      = 0x02
	tokenSaveFile        = "bedrocknode-token.txt"
	nodeSaveFile         = "bedrocknode-node.txt"
	localLockAddr        = "127.0.0.1:47339"
	maxTokenLength       = 4096
	maxLogLines          = 6
	handshakeHint        = 5
)

var version = "v1.4.0"

var (
	errInvalidToken        = errors.New("invalid auth token rejected by server")
	errTunnelAlreadyActive = errors.New("tunnel is already running on another machine or window")
)

const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cGreen  = "\033[92m"
	cRed    = "\033[91m"
	cYellow = "\033[93m"
	cCyan   = "\033[96m"
	cGray   = "\033[90m"
	cWhite  = "\033[97m"
)

var ansiRegex = regexp.MustCompile(`\x1b\[[0-9;]*m`)

type ExtraTunnelInfo struct {
	Slot       int    `json:"slot"`
	Name       string `json:"name"`
	ServerType string `json:"server_type,omitempty"`
	LocalTCP   int    `json:"local_tcp"`
	TCPPort    int    `json:"tcp_port"`
	UDPStart   int    `json:"udp_start"`
	UDPEnd     int    `json:"udp_end"`
	Disabled   bool   `json:"disabled,omitempty"`
}

type TunnelDisplay struct {
	Slot       int
	RegionCode string
	Name       string
	ServerType string
	PublicPort int
	LocalAddr  string
	UDPStart   int
	UDPEnd     int
	Disabled   bool
	Up         bool
}

type RegionStatus struct {
	Code        string
	Name        string
	ControlAddr string
	State       string
	FastLaneOn  bool
	ConnectedAt time.Time
	Tunnels     []TunnelDisplay
}

type ConsoleDashboard struct {
	mu            sync.Mutex
	State         string
	HubName       string
	HubAddr       string
	AlreadyMsg    string
	LatestVersion string
	FastLaneOn    bool
	LocalTCP      string
	LocalHost     string
	LocalUDP      string
	ServerUp      bool
	ClaimCode     string
	ClaimLinks    []string
	ConnectedAt   time.Time
	Logs          []string
	lastRawLog    string
	Headless      bool
	MultiRegion   bool
	RegionOrder   []string
	Regions       map[string]*RegionStatus
	Tunnels       []TunnelDisplay

	ActiveUDP atomic.Int32
	BytesIn   atomic.Uint64
	BytesOut  atomic.Uint64
}

var dash = &ConsoleDashboard{
	State:     "CONNECTING",
	HubName:   "Auto-Detecting Closest Hub...",
	LocalTCP:  "127.0.0.1:19132",
	LocalHost: "127.0.0.1",
	Regions:   make(map[string]*RegionStatus),
}

var jsonMode bool

type ClientHandshakeReq struct {
	Token    string `json:"token"`
	Version  string `json:"version"`
	Client   string `json:"client"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	WantJSON bool   `json:"want_json"`
}

type ServerHandshakeResp struct {
	OK              bool              `json:"ok"`
	Username        string            `json:"username"`
	RegionCode      string            `json:"region_code"`
	RegionLabel     string            `json:"region_label"`
	PublicIP        string            `json:"public_ip"`
	BaseDomain      string            `json:"base_domain"`
	Subdomain       string            `json:"subdomain"`
	CustomDomain    string            `json:"custom_domain,omitempty"`
	PrimaryName     string            `json:"primary_name,omitempty"`
	LocalTCP        int               `json:"local_tcp,omitempty"`
	PrimaryDisabled bool              `json:"primary_disabled,omitempty"`
	ServerType      string            `json:"server_type"`
	TCPPort         int               `json:"tcp_port"`
	UDPStart        int               `json:"udp_start"`
	UDPEnd          int               `json:"udp_end"`
	ServerProps     string            `json:"server_props"`
	GeyserConfig    string            `json:"geyser_config,omitempty"`
	ExtraTunnels    []ExtraTunnelInfo `json:"extra_tunnels,omitempty"`
}

func isInteractiveTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func enableTerminalUI(title string) {
	if runtime.GOOS == "windows" {
		_ = exec.Command("reg", "add", `HKCU\Console`, "/v", "VirtualTerminalLevel", "/t", "REG_DWORD", "/d", "1", "/f").Run()
		if title != "" {
			_ = exec.Command("cmd", "/c", "title "+title).Run()
		}
	}
	fmt.Print("\033[?1049h\033[2J\033[H\033[?25l")
}

func restoreTerminalUI() {
	fmt.Print("\033[?25h\033[?1049l")
}

func emitJSONEvent(event string, data map[string]any) {
	if !jsonMode {
		return
	}
	if data == nil {
		data = make(map[string]any)
	}
	data["event"] = event
	data["timestamp"] = time.Now().Unix()
	if b, err := json.Marshal(data); err == nil {
		fmt.Println(string(b))
	}
}

func (d *ConsoleDashboard) Init(localTCP, localUDP, webURL string) {
	log.SetOutput(io.Discard)
	host, _, err := net.SplitHostPort(localTCP)
	if err != nil || host == "" {
		host = "127.0.0.1"
	}

	headless := !jsonMode && !isInteractiveTTY()

	d.mu.Lock()
	d.LocalTCP = localTCP
	d.LocalHost = host
	d.LocalUDP = localUDP
	d.Headless = headless
	if d.Regions == nil {
		d.Regions = make(map[string]*RegionStatus)
	}
	d.mu.Unlock()

	if !jsonMode && !headless {
		enableTerminalUI("Bedrock Node Tunnel " + version)
	}
	go d.backgroundLoop()
	go d.checkVPSVersion(webURL)
}

func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(v)), "v")
}

func isNewerVersion(remote, current string) bool {
	rParts := strings.Split(normalizeVersion(remote), ".")
	cParts := strings.Split(normalizeVersion(current), ".")
	maxLen := len(rParts)
	if len(cParts) > maxLen {
		maxLen = len(cParts)
	}
	for i := 0; i < maxLen; i++ {
		var rv, cv int
		if i < len(rParts) {
			_, _ = fmt.Sscanf(rParts[i], "%d", &rv)
		}
		if i < len(cParts) {
			_, _ = fmt.Sscanf(cParts[i], "%d", &cv)
		}
		if rv > cv {
			return true
		}
		if rv < cv {
			return false
		}
	}
	return false
}

func (d *ConsoleDashboard) checkVPSVersion(webURL string) {
	base := strings.TrimRight(webURL, "/")
	if base == "" {
		base = "https://bedrocknode.net"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(base + "/api/version")
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return
	}
	remoteVer := strings.TrimSpace(string(body))
	if remoteVer != "" && len(remoteVer) <= 16 && strings.HasPrefix(strings.ToLower(remoteVer), "v") &&
		isNewerVersion(remoteVer, version) {
		d.mu.Lock()
		d.LatestVersion = remoteVer
		d.mu.Unlock()
		d.Logf("New client version (%s) is available on %s", remoteVer, base)
	}
}

func (d *ConsoleDashboard) SetState(state, hubName, hubAddr string, fastLane bool) {
	d.mu.Lock()
	if state == "ONLINE" && d.State != "ONLINE" {
		d.ConnectedAt = time.Now()
	}
	if state != "ONLINE" {
		d.FastLaneOn = false
	} else {
		d.FastLaneOn = fastLane
	}
	d.State = state
	if hubName != "" {
		d.HubName = hubName
	}
	if hubAddr != "" {
		d.HubAddr = hubAddr
	}
	d.mu.Unlock()
	d.Render()
}

func (d *ConsoleDashboard) UpdateRegionState(code, name, ctrlAddr, state string, fastLane bool) {
	d.mu.Lock()
	if d.Regions == nil {
		d.Regions = make(map[string]*RegionStatus)
	}
	rs, exists := d.Regions[code]
	if !exists {
		rs = &RegionStatus{Code: code, Name: name, ControlAddr: ctrlAddr}
		d.Regions[code] = rs
		d.RegionOrder = append(d.RegionOrder, code)
	}
	if name != "" {
		rs.Name = name
	}
	if ctrlAddr != "" {
		rs.ControlAddr = ctrlAddr
	}
	if state == "ONLINE" && rs.State != "ONLINE" {
		rs.ConnectedAt = time.Now()
	}
	if state != "ONLINE" {
		rs.FastLaneOn = false
	} else {
		rs.FastLaneOn = fastLane
	}
	rs.State = state

	if d.MultiRegion {
		anyOnline := false
		anyFast := false
		for _, r := range d.Regions {
			if r.State == "ONLINE" {
				anyOnline = true
				if d.ConnectedAt.IsZero() {
					d.ConnectedAt = r.ConnectedAt
				}
			}
			if r.FastLaneOn {
				anyFast = true
			}
		}
		if anyOnline {
			d.State = "ONLINE"
		} else {
			d.State = "CONNECTING"
		}
		d.FastLaneOn = anyFast
		d.HubName = fmt.Sprintf("%d Regional Hubs Active", len(d.Regions))
		d.HubAddr = ""
	}
	d.mu.Unlock()
	d.Render()
}

func (d *ConsoleDashboard) SetRegionFastLane(code string, active bool) {
	d.mu.Lock()
	if rs, ok := d.Regions[code]; ok {
		rs.FastLaneOn = active
	}
	if !d.MultiRegion {
		d.FastLaneOn = active
	} else {
		anyFast := false
		for _, r := range d.Regions {
			if r.FastLaneOn {
				anyFast = true
			}
		}
		d.FastLaneOn = anyFast
	}
	d.mu.Unlock()
	d.Render()
}

func (d *ConsoleDashboard) UpdateHandshakeTunnels(regionCode string, hs ServerHandshakeResp) {
	d.mu.Lock()
	host := d.LocalHost
	if host == "" {
		host = "127.0.0.1"
	}
	defaultLocal := d.LocalTCP

	var list []TunnelDisplay
	if hs.TCPPort > 0 {
		localAddr := defaultLocal
		if hs.LocalTCP > 0 {
			localAddr = net.JoinHostPort(host, fmt.Sprintf("%d", hs.LocalTCP))
		}
		pName := hs.PrimaryName
		if pName == "" {
			pName = "Server #1"
		}
		list = append(list, TunnelDisplay{
			Slot:       1,
			RegionCode: regionCode,
			Name:       pName,
			ServerType: hs.ServerType,
			PublicPort: hs.TCPPort,
			LocalAddr:  localAddr,
			UDPStart:   hs.UDPStart,
			UDPEnd:     hs.UDPEnd,
			Disabled:   hs.PrimaryDisabled,
		})
	}
	for _, et := range hs.ExtraTunnels {
		if et.TCPPort <= 0 {
			continue
		}
		lPort := et.LocalTCP
		if lPort <= 0 {
			lPort = 19132 + ((et.Slot - 1) * 2)
		}
		eName := et.Name
		if eName == "" {
			eName = fmt.Sprintf("Server #%d", et.Slot)
		}
		list = append(list, TunnelDisplay{
			Slot:       et.Slot,
			RegionCode: regionCode,
			Name:       eName,
			ServerType: et.ServerType,
			PublicPort: et.TCPPort,
			LocalAddr:  net.JoinHostPort(host, fmt.Sprintf("%d", lPort)),
			UDPStart:   et.UDPStart,
			UDPEnd:     et.UDPEnd,
			Disabled:   et.Disabled,
		})
	}

	if d.MultiRegion {
		if rs, ok := d.Regions[regionCode]; ok {
			rs.Tunnels = list
		}
	} else {
		d.Tunnels = list
	}
	d.mu.Unlock()
	d.probeLocalServers()
	d.Render()
}

func (d *ConsoleDashboard) SetAlreadyActive(hubName, hubAddr, reason string) {
	d.mu.Lock()
	d.State = "ALREADY_ACTIVE"
	d.FastLaneOn = false
	if hubName != "" {
		d.HubName = hubName
	}
	if hubAddr != "" {
		d.HubAddr = hubAddr
	}
	d.AlreadyMsg = reason
	d.mu.Unlock()
	d.Render()
}

func (d *ConsoleDashboard) SetFastLane(active bool) {
	d.mu.Lock()
	d.FastLaneOn = active
	d.mu.Unlock()
	d.Render()
}

func (d *ConsoleDashboard) SetClaimMode(code string, nodes []RegionNode) {
	var links []string
	claimURL := "https://bedrocknode.net/?claim=" + url.QueryEscape(code)
	for i, n := range nodes {
		if n.WebURL != "" {
			u := fmt.Sprintf("%s/?claim=%s", strings.TrimRight(n.WebURL, "/"), url.QueryEscape(code))
			if i == 0 {
				claimURL = u
			}
			links = append(links, fmt.Sprintf("%-30s %s", n.Name+":", u))
		}
	}
	d.mu.Lock()
	d.State = "CLAIM"
	d.ClaimCode = code
	d.ClaimLinks = links
	d.mu.Unlock()
	emitJSONEvent("claim_required", map[string]any{
		"claim_code": code,
		"claim_url":  claimURL,
	})
	if d.Headless && !jsonMode {
		fmt.Printf("[%s] ACTION REQUIRED — Claim Code: %s | Link: %s\n", time.Now().Format("15:04:05"), code, claimURL)
	}
	d.Render()
}

func (d *ConsoleDashboard) Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	msg = strings.TrimPrefix(msg, "[Bedrock Node Tunnel] ")
	msg = strings.TrimPrefix(msg, "[Tunnel Client] ")

	d.mu.Lock()
	if msg == d.lastRawLog {
		d.mu.Unlock()
		return
	}
	d.lastRawLog = msg

	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg)
	d.Logs = append(d.Logs, line)
	if len(d.Logs) > maxLogLines {
		d.Logs = d.Logs[len(d.Logs)-maxLogLines:]
	}
	headless := d.Headless
	d.mu.Unlock()

	if headless && !jsonMode {
		fmt.Println(line)
	}
	d.Render()
}

func checkTCPPortUp(addr string) bool {
	if addr == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (d *ConsoleDashboard) probeLocalServers() bool {
	d.mu.Lock()
	primaryTarget := d.LocalTCP
	prevPrimary := d.ServerUp
	multi := d.MultiRegion
	var addrs []string
	seen := make(map[string]bool)
	addAddr := func(a string) {
		if a != "" && !seen[a] {
			seen[a] = true
			addrs = append(addrs, a)
		}
	}
	addAddr(primaryTarget)
	if multi {
		for _, rs := range d.Regions {
			for _, tn := range rs.Tunnels {
				addAddr(tn.LocalAddr)
			}
		}
	} else {
		for _, tn := range d.Tunnels {
			addAddr(tn.LocalAddr)
		}
	}
	d.mu.Unlock()

	results := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		results[a] = checkTCPPortUp(a)
	}

	changed := false
	d.mu.Lock()
	if up, ok := results[primaryTarget]; ok && up != prevPrimary {
		d.ServerUp = up
		changed = true
	}
	if multi {
		for _, rs := range d.Regions {
			for i := range rs.Tunnels {
				if up, ok := results[rs.Tunnels[i].LocalAddr]; ok && up != rs.Tunnels[i].Up {
					rs.Tunnels[i].Up = up
					changed = true
				}
			}
		}
	} else {
		for i := range d.Tunnels {
			if up, ok := results[d.Tunnels[i].LocalAddr]; ok && up != d.Tunnels[i].Up {
				d.Tunnels[i].Up = up
				changed = true
			}
		}
	}
	d.mu.Unlock()
	return changed
}

func (d *ConsoleDashboard) backgroundLoop() {
	ticker := time.NewTicker(1 * time.Second)
	ticks := 0
	for range ticker.C {
		d.mu.Lock()
		state := d.State
		d.mu.Unlock()

		if state == "ALREADY_ACTIVE" || state == "CLAIM" {
			continue
		}

		ticks++
		statusChanged := false
		if ticks%3 == 1 {
			statusChanged = d.probeLocalServers()
		}

		if state == "ONLINE" || statusChanged {
			d.Render()
		}
	}
}

func runeDisplayWidth(r rune) int {
	if r >= 0x1F300 || r == 0x26A1 {
		return 2
	}
	return 1
}

func stringDisplayWidth(s string) int {
	clean := ansiRegex.ReplaceAllString(s, "")
	w := 0
	for _, r := range clean {
		w += runeDisplayWidth(r)
	}
	return w
}

func padBoxLine(content string, width int) string {
	visW := stringDisplayWidth(content)
	if visW > width {
		clean := ansiRegex.ReplaceAllString(content, "")
		var trimmed strings.Builder
		curW := 0
		for _, r := range clean {
			rw := runeDisplayWidth(r)
			if curW+rw > width-3 {
				break
			}
			trimmed.WriteRune(r)
			curW += rw
		}
		content = trimmed.String() + "..."
		visW = curW + 3
	}
	pad := width - visW
	if pad < 0 {
		pad = 0
	}
	return "  " + cCyan + "║" + cReset + content + strings.Repeat(" ", pad) + cCyan + "║" + cReset + "\n"
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func (d *ConsoleDashboard) BuildFrame(footer string) string {
	d.mu.Lock()
	defer d.mu.Unlock()

	const boxW = 88
	borderTop := "  " + cCyan + "╔" + strings.Repeat("═", boxW) + "╗" + cReset + "\n"
	borderMid := "  " + cCyan + "╠" + strings.Repeat("═", boxW) + "╣" + cReset + "\n"
	borderBot := "  " + cCyan + "╚" + strings.Repeat("═", boxW) + "╝" + cReset + "\n"

	var b strings.Builder
	b.WriteString("\033[H")

	b.WriteString(borderTop)
	titleLeft := fmt.Sprintf("   ⚡ BEDROCK NODE TUNNEL %-8s", version)
	titleRight := "https://bedrocknode.net   "
	spaceBetween := boxW - stringDisplayWidth(titleLeft) - stringDisplayWidth(titleRight)
	if spaceBetween < 1 {
		spaceBetween = 1
	}
	b.WriteString(padBoxLine(cBold+cWhite+titleLeft+strings.Repeat(" ", spaceBetween)+cCyan+titleRight+cReset, boxW))
	b.WriteString(borderMid)

	var statusBadge, uptimeStr string
	switch d.State {
	case "ONLINE":
		statusBadge = cGreen + cBold + "● ONLINE" + cReset
		up := time.Since(d.ConnectedAt).Round(time.Second)
		if d.ConnectedAt.IsZero() {
			up = 0
		}
		uptimeStr = fmt.Sprintf("%02dh %02dm %02ds", int(up.Hours()), int(up.Minutes())%60, int(up.Seconds())%60)
	case "ALREADY_ACTIVE":
		statusBadge = cYellow + cBold + "● ALREADY RUNNING (Active Elsewhere)" + cReset
		uptimeStr = "Active on another machine or window"
	case "CLAIM":
		statusBadge = cYellow + cBold + "◐ ACTION REQUIRED — CLAIM YOUR TUNNEL IN BROWSER" + cReset
		uptimeStr = "Waiting for account link..."
	case "CONNECTING":
		statusBadge = cYellow + cBold + "◐ CONNECTING..." + cReset
		uptimeStr = "--"
	default:
		statusBadge = cRed + cBold + "● OFFLINE (Reconnecting...)" + cReset
		uptimeStr = "--"
	}

	hubDisplay := d.HubName
	if d.HubAddr != "" && !strings.Contains(hubDisplay, d.HubAddr) {
		hubDisplay = fmt.Sprintf("%s (%s)", d.HubName, d.HubAddr)
	}

	b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s", "TUNNEL STATUS", statusBadge), boxW))
	b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s%s%s", "REGIONAL HUB", cWhite, hubDisplay, cReset), boxW))
	b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s", "SESSION UPTIME", uptimeStr), boxW))
	if d.LatestVersion != "" {
		updateLine := fmt.Sprintf("Update %s available at https://bedrocknode.net", d.LatestVersion)
		b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s%s%s", "UPDATE NOTICE", cYellow+cBold, updateLine, cReset), boxW))
	}
	b.WriteString(borderMid)

	switch d.State {
	case "CLAIM":
		b.WriteString(padBoxLine(fmt.Sprintf("   CLAIM CODE       :  %s%s%s", cYellow+cBold, d.ClaimCode, cReset), boxW))
		b.WriteString(padBoxLine("   Open your closest regional dashboard to link this tunnel:", boxW))
		for _, link := range d.ClaimLinks {
			b.WriteString(padBoxLine("   "+cCyan+link+cReset, boxW))
		}

	case "ALREADY_ACTIVE":
		noticeLine := d.AlreadyMsg
		if noticeLine == "" {
			noticeLine = "You already have your Bedrock Node running on another machine"
		}
		b.WriteString(padBoxLine(fmt.Sprintf("   %s%-16s :  %s%s", cYellow+cBold, "NOTICE", noticeLine, cReset), boxW))
		b.WriteString(padBoxLine("                       (or in another open tunnel window).", boxW))
		b.WriteString(padBoxLine(fmt.Sprintf("   %s%-16s :  Close the other tunnel to switch it here, or%s", cWhite, "ACTION", cReset), boxW))
		b.WriteString(padBoxLine(fmt.Sprintf("   %s                    close this window. Standing by quietly...%s", cWhite, cReset), boxW))

	default:
		fastLaneBadge := cYellow + "◐ Connecting UDP Fast-Lane..." + cReset
		if d.State != "ONLINE" {
			fastLaneBadge = cRed + "● Standby" + cReset
		} else if d.FastLaneOn {
			fastLaneBadge = cGreen + cBold + "● ACTIVE" + cReset + " (WebRTC Bypassed TCP)"
		}
		b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s", "UDP FAST-LANE", fastLaneBadge), boxW))

		if d.MultiRegion && len(d.RegionOrder) > 0 {
			for _, code := range d.RegionOrder {
				rs := d.Regions[code]
				if rs == nil {
					continue
				}
				rBadge := cRed + "● OFFLINE" + cReset
				if rs.State == "ONLINE" {
					if rs.FastLaneOn {
						rBadge = cGreen + cBold + "● ONLINE (Fast-Lane)" + cReset
					} else {
						rBadge = cGreen + "● ONLINE" + cReset
					}
				} else if rs.State == "CONNECTING" {
					rBadge = cYellow + "◐ CONNECTING" + cReset
				}
				portNote := ""
				if len(rs.Tunnels) > 0 {
					var ps []string
					for _, tn := range rs.Tunnels {
						ps = append(ps, fmt.Sprintf(":%d->%s", tn.PublicPort, tn.LocalAddr))
					}
					portNote = " " + cGray + "[" + strings.Join(ps, ", ") + "]" + cReset
				}
				b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s%s", "HUB "+rs.Code, rBadge, portNote), boxW))
			}
		} else if len(d.Tunnels) > 0 {
			for _, tn := range d.Tunnels {
				label := fmt.Sprintf("SERVER #%d", tn.Slot)
				var sBadge string
				if tn.Disabled {
					sBadge = cYellow + "⏸ PAUSED" + cReset
				} else if tn.Up {
					sBadge = cGreen + cBold + "● RUNNING" + cReset
				} else {
					sBadge = cRed + "● NOT DETECTED" + cReset
				}
				typeTag := "BDS"
				if strings.EqualFold(tn.ServerType, "geyser") {
					typeTag = "Geyser"
				}
				info := fmt.Sprintf("%s  %s[%s]%s  Public :%d → %s",
					sBadge, cCyan, typeTag, cReset, tn.PublicPort, tn.LocalAddr)
				b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s", label, info), boxW))
			}
		} else {
			serverBadge := cRed + "● NOT DETECTED" + cReset + fmt.Sprintf(" (Start server on %s)", d.LocalTCP)
			if d.ServerUp {
				serverBadge = cGreen + cBold + "● RUNNING" + cReset + fmt.Sprintf(" (%s)", d.LocalTCP)
			}
			b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %s", "BEDROCK SERVER", serverBadge), boxW))
		}

		activePlayers := d.ActiveUDP.Load()
		inStr := formatBytes(d.BytesIn.Load())
		outStr := formatBytes(d.BytesOut.Load())

		b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  %d Active Player UDP Streams", "UDP SESSIONS", activePlayers), boxW))
		b.WriteString(padBoxLine(fmt.Sprintf("   %-16s :  ↓ %-12s In   |   ↑ %-12s Out", "LIVE TRAFFIC", inStr, outStr), boxW))
	}

	b.WriteString(borderMid)
	b.WriteString(padBoxLine(cBold+"   RECENT ACTIVITY"+cReset, boxW))
	for i := 0; i < maxLogLines; i++ {
		if i < len(d.Logs) {
			b.WriteString(padBoxLine("   "+cGray+d.Logs[i]+cReset, boxW))
		} else {
			b.WriteString(padBoxLine("", boxW))
		}
	}
	b.WriteString(borderBot)
	if footer == "" {
		footer = "Keep this window open while hosting. Press Ctrl+C to stop the tunnel."
	}
	b.WriteString("   " + footer + "\033[J")
	return b.String()
}

func (d *ConsoleDashboard) Render() {
	if jsonMode {
		return
	}
	d.mu.Lock()
	headless := d.Headless
	d.mu.Unlock()
	if headless {
		return
	}
	_, _ = os.Stdout.WriteString(d.BuildFrame(""))
}

// ============================================================================
// LOCAL INSTANCE LOCK + LIVE "screen -r" STYLE ATTACH SERVER (127.0.0.1:47339)
// ============================================================================

func acquireLocalInstanceLock() net.Listener {
	ln, err := net.Listen("tcp", localLockAddr)
	if err != nil {
		return nil
	}
	return ln
}

func serveLocalAttachViewers(ln net.Listener) {
	const attachedFooter = "Attached to running background tunnel. Press Ctrl+C to detach (tunnel keeps running)."
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()

			sendFrame := func() error {
				frame := dash.BuildFrame(attachedFooter)
				_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
				return writeFramedPacket(c, []byte(frame))
			}

			if err := sendFrame(); err != nil {
				return
			}
			for range ticker.C {
				if err := sendFrame(); err != nil {
					return
				}
			}
		}(conn)
	}
}

func attachToRunningClient() bool {
	conn, err := net.DialTimeout("tcp", localLockAddr, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	interactive := isInteractiveTTY()
	if interactive {
		enableTerminalUI("Bedrock Node Tunnel " + version + " (Attached)")
		defer restoreTerminalUI()
	}

	doneCh := make(chan error, 1)
	go func() {
		for {
			frame, err := readFramedPacket(conn)
			if err != nil {
				doneCh <- err
				return
			}
			_, _ = os.Stdout.Write(frame)
		}
	}()

	select {
	case <-sigCh:
		_ = conn.Close()
		if interactive {
			restoreTerminalUI()
		}
		fmt.Println("Detached from Bedrock Node Tunnel (background tunnel is still running).")
		return true
	case <-doneCh:
		if interactive {
			restoreTerminalUI()
		}
		fmt.Println("Running Bedrock Node Tunnel session closed.")
		return true
	}
}

type RegionNode struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	WebURL      string `json:"webUrl"`
	ControlAddr string `json:"controlAddr"`
}

var defaultRegionNodes = []RegionNode{
	{
		Code:        "US",
		Name:        "North America (US East Hub)",
		WebURL:      "https://bedrocknode.net",
		ControlAddr: "51.222.142.179:7000",
	},
	{
		Code:        "TX",
		Name:        "North America (US Texas Hub)",
		WebURL:      "https://texas.bedrocknode.net",
		ControlAddr: "216.128.129.60:7000",
	},
	{
		Code:        "UK",
		Name:        "Europe (London UK Hub)",
		WebURL:      "https://uk.bedrocknode.net",
		ControlAddr: "104.238.173.37:7000",
	},
	{
		Code:        "BR",
		Name:        "South America (Brazil Hub)",
		WebURL:      "https://southa.bedrocknode.net",
		ControlAddr: "216.128.170.42:7000",
	},
	{
		Code:        "ASIA",
		Name:        "Asia-Pacific (Singapore Hub)",
		WebURL:      "https://asia.bedrocknode.net",
		ControlAddr: "45.32.121.108:7000",
	},
	{
		Code:        "OC",
		Name:        "Oceania (Sydney Hub)",
		WebURL:      "https://oceania.bedrocknode.net",
		ControlAddr: "139.180.162.173:7000",
	},
}

type rawPlayerKey struct {
	pubPort    uint16
	playerIP   [4]byte
	playerPort uint16
}

type candConn struct {
	label string
	conn  *net.UDPConn
}

type rawLocalSession struct {
	conns      []candConn
	winnerIdx  atomic.Int32
	lastActive atomic.Int64
}

func (s *rawLocalSession) Close() {
	for _, c := range s.conns {
		if c.conn != nil {
			_ = c.conn.Close()
		}
	}
}

type TokenTarget struct {
	ControlAddr string
	Token       string
	Label       string
	Code        string
}

func main() {
	var (
		nodeFlag        string
		nodesFlag       string
		controlAddr     string
		webUrl          string
		token           string
		localSignaling  string
		localUDPIP      string
		localUDPDefault int
		resetToken      bool
		showVersion     bool
		handshakeJSON   string
	)

	flag.StringVar(&nodeFlag, "node", "", "Region code (US, TX, UK, BR, ASIA, OC) or custom VPS domain/IP")
	flag.StringVar(&nodesFlag, "nodes", "", "Comma-separated regions to connect simultaneously (e.g. US,TX,UK or all)")
	flag.StringVar(&controlAddr, "control", "", "Bedrock Node Tunnel VPS control address (auto-detected across regions if empty)")
	flag.StringVar(&webUrl, "web", "https://bedrocknode.net", "Bedrock Node Tunnel Web Dashboard URL")
	flag.StringVar(&token, "token", "", "Tunnel token (leave empty for automatic browser claim)")
	flag.StringVar(&localSignaling, "local-http", "127.0.0.1:19132", "Local Bedrock TCP target on Home PC")
	flag.StringVar(&localUDPIP, "local-udp-ip", "", "Local LAN IP for Bedrock UDP (auto-detected if empty)")
	flag.IntVar(&localUDPDefault, "local-udp-port", 47334, "Fallback local UDP port on Home PC")
	flag.BoolVar(&resetToken, "reset-token", false, "Delete the saved tunnel token and claim a new one")
	flag.BoolVar(&showVersion, "version", false, "Print version and exit")
	flag.BoolVar(&jsonMode, "json", false, "Output machine-readable JSON events to stdout (for CraftDroid / GUIs)")
	flag.StringVar(&handshakeJSON, "handshake-json", "", "Optional JSON config payload (token, node, local_http, local_udp_port)")
	flag.Parse()

	if showVersion {
		fmt.Println(version)
		return
	}

	explicitFlags := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) {
		explicitFlags[f.Name] = true
	})

	if handshakeJSON != "" {
		jsonMode = true
		var cfg struct {
			Token        string `json:"token"`
			Node         string `json:"node"`
			Control      string `json:"control"`
			Web          string `json:"web"`
			LocalHTTP    string `json:"local_http"`
			LocalUDPIP   string `json:"local_udp_ip"`
			LocalUDPPort int    `json:"local_udp_port"`
		}
		if err := json.Unmarshal([]byte(handshakeJSON), &cfg); err == nil {
			if cfg.Token != "" {
				token = cfg.Token
			}
			if cfg.Node != "" {
				nodeFlag = cfg.Node
			}
			if cfg.Control != "" {
				controlAddr = cfg.Control
				explicitFlags["control"] = true
			}
			if cfg.Web != "" {
				webUrl = cfg.Web
				explicitFlags["web"] = true
			}
			if cfg.LocalHTTP != "" {
				localSignaling = cfg.LocalHTTP
			}
			if cfg.LocalUDPIP != "" {
				localUDPIP = cfg.LocalUDPIP
			}
			if cfg.LocalUDPPort > 0 {
				localUDPDefault = cfg.LocalUDPPort
			}
		}
	}

	token = strings.TrimSpace(token)
	if token != "" && !isValidToken(token) {
		log.Fatalf("[Bedrock Node Tunnel] Invalid -token value: must be 1-%d printable ASCII characters without spaces", maxTokenLength)
	}
	if resetToken {
		clearSavedTokens()
	}

	cleanWebURL, err := validateWebURL(webUrl)
	if err != nil {
		log.Fatalf("[Bedrock Node Tunnel] Invalid web URL: %v", err)
	}

	pollWebOverride := ""

	if nodeFlag != "" {
		n := resolveNodeInput(nodeFlag)
		if !explicitFlags["control"] {
			controlAddr = n.ControlAddr
		}
		if !explicitFlags["web"] && n.WebURL != "" {
			cleanWebURL = n.WebURL
		}
		pollWebOverride = cleanWebURL
		_ = saveNodeAddr(controlAddr)
	} else if explicitFlags["control"] && !explicitFlags["web"] {
		n := resolveNodeInput(controlAddr)
		controlAddr = n.ControlAddr
		if n.WebURL != "" {
			cleanWebURL = n.WebURL
		}
		pollWebOverride = cleanWebURL
		_ = saveNodeAddr(controlAddr)
	} else if explicitFlags["web"] && !explicitFlags["control"] {
		n := resolveNodeInput(cleanWebURL)
		controlAddr = n.ControlAddr
		pollWebOverride = cleanWebURL
		_ = saveNodeAddr(controlAddr)
	} else if explicitFlags["web"] && explicitFlags["control"] {
		pollWebOverride = cleanWebURL
		_ = saveNodeAddr(controlAddr)
	}

	// Check if another instance/service is already running on this PC BEFORE taking over the screen
	lockLn := acquireLocalInstanceLock()
	if lockLn == nil {
		if !jsonMode && !resetToken && attachToRunningClient() {
			return
		}
		if localUDPIP == "" {
			localUDPIP = detectOutboundLANIP()
		}
		dash.Init(localSignaling, localUDPIP, cleanWebURL)
		savedHub := loadSavedNodeAddr()
		hubInfo := resolveNodeInput(savedHub)
		dash.SetAlreadyActive(hubInfo.Name, savedHub, "Bedrock Node Tunnel is already open on this PC")
		dash.Logf("Another Bedrock Node Tunnel window is already running on this PC.")
		for {
			time.Sleep(5 * time.Second)
			if lockLn = acquireLocalInstanceLock(); lockLn != nil {
				dash.Logf("Previous local window closed — starting tunnel now...")
				break
			}
		}
	} else {
		if localUDPIP == "" {
			localUDPIP = detectOutboundLANIP()
		}
		dash.Init(localSignaling, localUDPIP, cleanWebURL)
	}
	defer func() {
		if lockLn != nil {
			_ = lockLn.Close()
		}
		if !jsonMode && !dash.Headless {
			restoreTerminalUI()
		}
	}()

	// Restore terminal cleanly on Ctrl+C / SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		if lockLn != nil {
			_ = lockLn.Close()
		}
		if !jsonMode && !dash.Headless {
			restoreTerminalUI()
		}
		os.Exit(0)
	}()

	go serveLocalAttachViewers(lockLn)

	dash.Logf("Starting Bedrock Node Tunnel %s (Local UDP IP: %s)", version, localUDPIP)

	// Multi-region mode: either via multi-line bedrocknode-token.txt or -nodes flag
	if targets := resolveMultiRegionTargets(nodesFlag, token); len(targets) > 1 {
		dash.mu.Lock()
		dash.MultiRegion = true
		dash.mu.Unlock()
		dash.Logf("Multi-Region Mode: connecting to %d hubs simultaneously...", len(targets))
		runMultiRegionClients(targets, localSignaling, localUDPIP, localUDPDefault)
		return
	}

	runClient(controlAddr, pollWebOverride, token, localSignaling, localUDPIP, localUDPDefault)
}

func resolveMultiRegionTargets(nodesFlag, flagToken string) []TokenTarget {
	// 1. Check multi-line token file (e.g. one "US|bnode_xxx" per line)
	if flagToken == "" {
		if multi := loadSavedMultiTokens(); len(multi) > 1 {
			return multi
		}
	}
	// 2. Check explicit -nodes flag (e.g. -nodes US,TX,UK or -nodes all)
	nodesFlag = strings.TrimSpace(nodesFlag)
	if nodesFlag == "" {
		return nil
	}
	tok := flagToken
	if tok == "" {
		saved, _ := loadSavedToken()
		_, tok = splitOptionalNodeToken(saved)
	}
	if !isValidToken(tok) {
		return nil
	}

	var targets []TokenTarget
	seen := make(map[string]bool)
	if strings.EqualFold(nodesFlag, "all") {
		for _, n := range getActiveNodeList("", "") {
			if seen[n.ControlAddr] {
				continue
			}
			seen[n.ControlAddr] = true
			targets = append(targets, TokenTarget{
				ControlAddr: n.ControlAddr,
				Token:       tok,
				Label:       n.Name,
				Code:        n.Code,
			})
		}
		return targets
	}
	for _, part := range strings.Split(nodesFlag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n := resolveNodeInput(part)
		if n.ControlAddr == "" || seen[n.ControlAddr] {
			continue
		}
		seen[n.ControlAddr] = true
		targets = append(targets, TokenTarget{
			ControlAddr: n.ControlAddr,
			Token:       tok,
			Label:       n.Name,
			Code:        n.Code,
		})
	}
	return targets
}

func loadSavedMultiTokens() []TokenTarget {
	for _, p := range candidateTokenPaths() {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var targets []TokenTarget
		seen := make(map[string]bool)
		for _, rawLine := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(rawLine)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			ctrl, tok := splitOptionalNodeToken(line)
			if ctrl == "" || !isValidToken(tok) || seen[ctrl] {
				continue
			}
			seen[ctrl] = true
			info := resolveNodeInput(ctrl)
			targets = append(targets, TokenTarget{
				ControlAddr: ctrl,
				Token:       tok,
				Label:       info.Name,
				Code:        info.Code,
			})
		}
		if len(targets) > 1 {
			return targets
		}
	}
	return nil
}

func resolveNodeInput(input string) RegionNode {
	clean := strings.TrimSpace(input)
	if clean == "" {
		return RegionNode{Code: "AUTO", Name: "Auto-Detecting Closest Hub"}
	}
	upper := strings.ToUpper(clean)

	switch upper {
	case "DALLAS", "TEXAS", "US-CENTRAL", "US-SOUTH":
		upper = "TX"
	case "LONDON", "EU", "EUROPE", "GB":
		upper = "UK"
	case "BRAZIL", "SA", "SOUTHA":
		upper = "BR"
	case "SG", "SINGAPORE", "AP":
		upper = "ASIA"
	case "OCEANIA", "SYDNEY", "AU", "AUS", "NZ":
		upper = "OC"
	case "USEAST", "US-EAST":
		upper = "US"
	}

	for _, n := range defaultRegionNodes {
		hostOnly := strings.TrimPrefix(strings.TrimPrefix(n.WebURL, "https://"), "http://")
		ipOnly := strings.TrimSuffix(n.ControlAddr, ":7000")
		if upper == n.Code || strings.EqualFold(clean, n.WebURL) || strings.EqualFold(clean, n.ControlAddr) ||
			strings.EqualFold(clean, hostOnly) || strings.EqualFold(clean, ipOnly) {
			return n
		}
	}

	clean = strings.TrimPrefix(clean, "https://")
	clean = strings.TrimPrefix(clean, "http://")
	clean = strings.TrimRight(clean, "/")

	host := clean
	ctrl := clean
	if strings.Contains(clean, ":") {
		h, p, err := net.SplitHostPort(clean)
		if err == nil {
			host = h
			if p == "8080" || p == "443" || p == "80" {
				ctrl = net.JoinHostPort(h, "7000")
			} else {
				ctrl = clean
			}
		}
	} else {
		ctrl = net.JoinHostPort(clean, "7000")
	}

	scheme := "https://"
	if net.ParseIP(host) != nil {
		scheme = "http://"
		return RegionNode{
			Code:        "CUSTOM",
			Name:        "Custom Node (" + host + ")",
			WebURL:      scheme + net.JoinHostPort(host, "8080"),
			ControlAddr: ctrl,
		}
	}

	return RegionNode{
		Code:        "CUSTOM",
		Name:        "Custom Node (" + host + ")",
		WebURL:      scheme + host,
		ControlAddr: ctrl,
	}
}

func getActiveNodeList(customControl, customWeb string) []RegionNode {
	if customControl != "" || customWeb != "" {
		cAddr := customControl
		wURL := customWeb
		if cAddr == "" && wURL != "" {
			cAddr = resolveNodeInput(wURL).ControlAddr
		}
		if wURL == "" && cAddr != "" {
			wURL = resolveNodeInput(cAddr).WebURL
		}
		return []RegionNode{
			{
				Code:        "CUSTOM",
				Name:        resolveNodeInput(cAddr).Name,
				WebURL:      strings.TrimRight(wURL, "/"),
				ControlAddr: cAddr,
			},
		}
	}

	var nodes []RegionNode
	seenCtrl := make(map[string]bool)
	addNode := func(n RegionNode) {
		if n.ControlAddr == "" || seenCtrl[n.ControlAddr] {
			return
		}
		seenCtrl[n.ControlAddr] = true
		nodes = append(nodes, n)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	if resp, err := client.Get("https://bedrocknode.net/api/nodes"); err == nil {
		if resp.StatusCode == http.StatusOK {
			var remoteNodes []RegionNode
			if json.NewDecoder(resp.Body).Decode(&remoteNodes) == nil {
				for _, rn := range remoteNodes {
					if rn.ControlAddr == "" && rn.WebURL != "" {
						rn = resolveNodeInput(rn.WebURL)
					}
					addNode(rn)
				}
			}
		}
		resp.Body.Close()
	}

	for _, n := range defaultRegionNodes {
		addNode(n)
	}
	return nodes
}

func validateWebURL(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", errors.New("URL scheme must be http or https")
	}
	if u.Host == "" {
		return "", errors.New("URL host cannot be empty")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func candidateTokenPaths() []string {
	var paths []string
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		paths = append(paths, filepath.Join(configDir, "bedrocknode-tunnel", tokenSaveFile))
	} else if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		paths = append(paths, filepath.Join(homeDir, ".config", "bedrocknode-tunnel", tokenSaveFile))
	}
	return append(paths, tokenSaveFile)
}

func candidateNodePaths() []string {
	var paths []string
	if configDir, err := os.UserConfigDir(); err == nil && configDir != "" {
		paths = append(paths, filepath.Join(configDir, "bedrocknode-tunnel", nodeSaveFile))
	} else if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		paths = append(paths, filepath.Join(homeDir, ".config", "bedrocknode-tunnel", nodeSaveFile))
	}
	return append(paths, nodeSaveFile)
}

func loadSavedToken() (string, string) {
	for _, p := range candidateTokenPaths() {
		if data, err := os.ReadFile(p); err == nil {
			for _, rawLine := range strings.Split(string(data), "\n") {
				saved := strings.TrimSpace(rawLine)
				if saved == "" || strings.HasPrefix(saved, "#") {
					continue
				}
				if isValidToken(saved) {
					return saved, p
				}
			}
		}
	}
	return "", ""
}

func saveToken(tok string) string {
	for _, p := range candidateTokenPaths() {
		dir := filepath.Dir(p)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				continue
			}
		}
		if err := os.WriteFile(p, []byte(tok), 0600); err == nil {
			return p
		}
	}
	return ""
}

func loadSavedNodeAddr() string {
	for _, p := range candidateNodePaths() {
		if data, err := os.ReadFile(p); err == nil {
			raw := strings.TrimSpace(string(data))
			if raw != "" {
				return resolveNodeInput(raw).ControlAddr
			}
		}
	}
	return ""
}

func saveNodeAddr(ctrlAddr string) string {
	ctrlAddr = strings.TrimSpace(ctrlAddr)
	if ctrlAddr == "" {
		return ""
	}
	for _, p := range candidateNodePaths() {
		dir := filepath.Dir(p)
		if dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				continue
			}
		}
		if err := os.WriteFile(p, []byte(ctrlAddr), 0600); err == nil {
			return p
		}
	}
	return ""
}

func clearSavedTokens() {
	for _, p := range candidateTokenPaths() {
		_ = os.Remove(p)
	}
	for _, p := range candidateNodePaths() {
		_ = os.Remove(p)
	}
}

func splitOptionalNodeToken(raw string) (string, string) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "|") {
		parts := strings.SplitN(s, "|", 2)
		nPart := strings.TrimSpace(parts[0])
		tPart := strings.TrimSpace(parts[1])
		if nPart != "" && isValidToken(tPart) {
			return resolveNodeInput(nPart).ControlAddr, tPart
		}
	}
	return "", s
}

func openBrowser(targetURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", targetURL)
	case "darwin":
		cmd = exec.Command("open", targetURL)
	case "linux":
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		path, err := exec.LookPath("xdg-open")
		if err != nil {
			return
		}
		cmd = exec.Command(path, targetURL)
	default:
		return
	}
	if cmd.Start() == nil {
		go func() { _ = cmd.Wait() }()
	}
}

func detectOutboundLANIP() string {
	conn, err := net.DialTimeout("udp", "8.8.8.8:80", 3*time.Second)
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if udpAddr, ok := conn.LocalAddr().(*net.UDPAddr); ok && !udpAddr.IP.IsLoopback() {
		return udpAddr.IP.String()
	}
	return "127.0.0.1"
}

func tuneTCPConn(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetNoDelay(true)
		_ = tcpConn.SetKeepAlive(true)
		_ = tcpConn.SetKeepAlivePeriod(15 * time.Second)
		_ = tcpConn.SetReadBuffer(4 * 1024 * 1024)
		_ = tcpConn.SetWriteBuffer(4 * 1024 * 1024)
	}
}

func tuneUDPConn(conn *net.UDPConn, bufBytes int) {
	if conn != nil {
		_ = conn.SetReadBuffer(bufBytes)
		_ = conn.SetWriteBuffer(bufBytes)
	}
}

func newYamuxConfig() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 35 * time.Second
	cfg.MaxStreamWindowSize = 4 * 1024 * 1024
	cfg.LogOutput = io.Discard
	return cfg
}

func buildLocalUDPCandidates(localUDPIP string, targetPort int) []candConn {
	type targetSpec struct {
		netw string
		addr string
	}
	specs := []targetSpec{
		{"udp4", net.JoinHostPort(localUDPIP, fmt.Sprintf("%d", targetPort))},
		{"udp4", net.JoinHostPort("127.0.0.1", fmt.Sprintf("%d", targetPort))},
		{"udp6", net.JoinHostPort("::1", fmt.Sprintf("%d", targetPort))},
	}

	var conns []candConn
	seen := make(map[string]bool)
	for _, s := range specs {
		if seen[s.addr] {
			continue
		}
		seen[s.addr] = true
		rAddr, err := net.ResolveUDPAddr(s.netw, s.addr)
		if err != nil {
			continue
		}
		c, err := net.DialUDP(s.netw, nil, rAddr)
		if err != nil {
			continue
		}
		tuneUDPConn(c, 4*1024*1024)
		conns = append(conns, candConn{label: s.addr, conn: c})
	}
	return conns
}

func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isValidToken(tok string) bool {
	if len(tok) == 0 || len(tok) > maxTokenLength {
		return false
	}
	for _, r := range tok {
		if r <= 0x20 || r >= 0x7f {
			return false
		}
	}
	return true
}

func readToken(r io.Reader) (string, bool) {
	body, err := io.ReadAll(io.LimitReader(r, maxTokenLength+1))
	if err != nil || len(body) > maxTokenLength {
		return "", false
	}
	tok := strings.TrimSpace(string(body))
	return tok, isValidToken(tok)
}

func obtainOrClaimToken(customControl, customWeb, flagToken string) (string, string) {
	if isValidToken(flagToken) {
		embCtrl, cleanTok := splitOptionalNodeToken(flagToken)
		if embCtrl == "" {
			embCtrl = customControl
		}
		if embCtrl == "" {
			embCtrl = loadSavedNodeAddr()
		}
		return embCtrl, cleanTok
	}

	if saved, path := loadSavedToken(); saved != "" {
		embCtrl, cleanTok := splitOptionalNodeToken(saved)
		if embCtrl == "" {
			embCtrl = customControl
		}
		if embCtrl == "" {
			embCtrl = loadSavedNodeAddr()
		}
		dash.Logf("Loaded saved tunnel token from %s", filepath.Base(path))
		return embCtrl, cleanTok
	}

	nodes := getActiveNodeList(customControl, customWeb)
	claimCode := strings.ToUpper(randomHex(3))

	dash.SetClaimMode(claimCode, nodes)
	dash.Logf("Generated claim code %s — waiting for browser claim...", claimCode)

	if len(nodes) > 0 && nodes[0].WebURL != "" {
		firstLink := fmt.Sprintf("%s/?claim=%s", strings.TrimRight(nodes[0].WebURL, "/"), url.QueryEscape(claimCode))
		openBrowser(firstLink)
	}

	client := &http.Client{Timeout: 4 * time.Second}

	for {
		for _, n := range nodes {
			if n.WebURL == "" {
				continue
			}
			pollUrl := fmt.Sprintf("%s/api/poll-claim?code=%s", strings.TrimRight(n.WebURL, "/"), url.QueryEscape(claimCode))
			resp, err := client.Get(pollUrl)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					tok, ok := readToken(resp.Body)
					resp.Body.Close()
					if ok {
						embCtrl, cleanTok := splitOptionalNodeToken(tok)
						chosenCtrl := embCtrl
						if chosenCtrl == "" {
							chosenCtrl = n.ControlAddr
						}
						if savedPath := saveToken(cleanTok); savedPath != "" {
							dash.Logf("Tunnel claimed on %s!", n.Name)
						}
						_ = saveNodeAddr(chosenCtrl)
						return chosenCtrl, cleanTok
					}
				} else {
					resp.Body.Close()
				}
			}
		}
		time.Sleep(2500 * time.Millisecond)
	}
}

var useLegacyAuth atomic.Bool

func sendTunnelToken(conn net.Conn, token string) (*ServerHandshakeResp, error) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetWriteDeadline(time.Time{})

	var payload []byte
	sentJSON := false
	if !useLegacyAuth.Load() {
		req := ClientHandshakeReq{
			Token:    token,
			Version:  version,
			Client:   "nethernet-client",
			OS:       runtime.GOOS,
			Arch:     runtime.GOARCH,
			WantJSON: true,
		}
		if b, err := json.Marshal(req); err == nil {
			payload = b
			sentJSON = true
		}
	}
	if len(payload) == 0 {
		payload = []byte(token)
	}

	if err := writeFramedPacket(conn, payload); err != nil {
		return nil, err
	}
	var ack [1]byte
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	if _, err := io.ReadFull(conn, ack[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errTunnelAlreadyActive
		}
		return nil, err
	}
	switch ack[0] {
	case 0x01:
		emitJSONEvent("connected", map[string]any{"status": "online", "version": version})
		return nil, nil
	case 0x03:
		respBytes, err := readFramedPacket(conn)
		if err != nil {
			return nil, err
		}
		var hs ServerHandshakeResp
		if json.Unmarshal(respBytes, &hs) == nil {
			extraNote := ""
			if len(hs.ExtraTunnels) > 0 {
				extraNote = fmt.Sprintf(" (+%d extra)", len(hs.ExtraTunnels))
			}
			dash.Logf("[%s] Assigned TCP %s:%d | UDP %d-%d%s", hs.RegionCode, hs.PublicIP, hs.TCPPort, hs.UDPStart, hs.UDPEnd, extraNote)
			emitJSONEvent("handshake", map[string]any{
				"status":        "online",
				"version":       version,
				"username":      hs.Username,
				"region_code":   hs.RegionCode,
				"public_ip":     hs.PublicIP,
				"subdomain":     hs.Subdomain,
				"server_type":   hs.ServerType,
				"tcp_port":      hs.TCPPort,
				"udp_start":     hs.UDPStart,
				"udp_end":       hs.UDPEnd,
				"server_props":  hs.ServerProps,
				"geyser_config": hs.GeyserConfig,
				"extra_tunnels": hs.ExtraTunnels,
			})
			return &hs, nil
		}
		return nil, nil
	case 0x02:
		return nil, errTunnelAlreadyActive
	case 0x04:
		return nil, errInvalidToken
	default:
		if sentJSON {
			useLegacyAuth.Store(true)
			return nil, errors.New("retrying with legacy token format")
		}
		return nil, errInvalidToken
	}
}

func writeFramedPacket(w io.Writer, payload []byte) error {
	if len(payload) > 65535 {
		return fmt.Errorf("packet too large: %d", len(payload))
	}
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readFramedPacket(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(hdr[:])
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func runMultiRegionClients(targets []TokenTarget, localSignaling, localUDPIP string, localUDPDefault int) {
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(tgt TokenTarget) {
			defer wg.Done()
			runRegionWorker(tgt, localSignaling, localUDPIP, localUDPDefault)
		}(t)
	}
	wg.Wait()
}

func runRegionWorker(tgt TokenTarget, localSignaling, localUDPIP string, localUDPDefault int) {
	for {
		dash.UpdateRegionState(tgt.Code, tgt.Label, tgt.ControlAddr, "CONNECTING", false)
		conn, err := net.DialTimeout("tcp", tgt.ControlAddr, 10*time.Second)
		if err != nil {
			dash.UpdateRegionState(tgt.Code, tgt.Label, tgt.ControlAddr, "OFFLINE", false)
			dash.Logf("[%s] Dial failed (%s). Retrying in 5s...", tgt.Code, tgt.ControlAddr)
			time.Sleep(5 * time.Second)
			continue
		}
		tuneTCPConn(conn)

		hs, err := sendTunnelToken(conn, tgt.Token)
		if err != nil {
			conn.Close()
			dash.UpdateRegionState(tgt.Code, tgt.Label, tgt.ControlAddr, "OFFLINE", false)
			if errors.Is(err, errInvalidToken) {
				dash.Logf("[%s] Token not registered on %s (retrying in 30s)", tgt.Code, tgt.Label)
				time.Sleep(30 * time.Second)
			} else {
				dash.Logf("[%s] Handshake error: %v (retrying in 5s)", tgt.Code, err)
				time.Sleep(5 * time.Second)
			}
			continue
		}

		if hs != nil {
			dash.UpdateHandshakeTunnels(tgt.Code, *hs)
		}

		sess, err := yamux.Client(conn, newYamuxConfig())
		if err != nil {
			conn.Close()
			dash.UpdateRegionState(tgt.Code, tgt.Label, tgt.ControlAddr, "OFFLINE", false)
			time.Sleep(3 * time.Second)
			continue
		}

		stopRawUDP := make(chan struct{})
		go func() {
			select {
			case <-stopRawUDP:
				return
			case <-time.After(1500 * time.Millisecond):
				if !sess.IsClosed() {
					dash.UpdateRegionState(tgt.Code, tgt.Label, tgt.ControlAddr, "ONLINE", false)
					dash.Logf("[%s] Tunnel ONLINE via %s!", tgt.Code, tgt.Label)
					runRawUDPFastLane(stopRawUDP, tgt.Code, tgt.ControlAddr, tgt.Token, localUDPIP, localUDPDefault)
				}
			}
		}()

		for {
			stream, err := sess.Accept()
			if err != nil {
				dash.UpdateRegionState(tgt.Code, tgt.Label, tgt.ControlAddr, "OFFLINE", false)
				dash.Logf("[%s] Disconnected. Reconnecting...", tgt.Code)
				break
			}
			go handleIncomingStream(stream, localSignaling, localUDPIP, localUDPDefault)
		}

		close(stopRawUDP)
		_ = sess.Close()
		time.Sleep(2 * time.Second)
	}
}

func runClient(customControl, customWeb, flagToken, localSignaling, localUDPIP string, localUDPDefault int) {
	targetControl, token := obtainOrClaimToken(customControl, customWeb, flagToken)
	handshakeFailures := 0

	for {
		var authConn net.Conn
		var activeControl string
		var activeHS *ServerHandshakeResp

		if targetControl != "" {
			hubInfo := resolveNodeInput(targetControl)
			if dash.State != "ALREADY_ACTIVE" {
				dash.SetState("CONNECTING", hubInfo.Name, targetControl, false)
				dash.Logf("Dialing %s (%s)...", hubInfo.Name, targetControl)
			}

			conn, err := net.DialTimeout("tcp", targetControl, 10*time.Second)
			if err != nil {
				dash.SetState("OFFLINE", hubInfo.Name, targetControl, false)
				dash.Logf("Connection to %s failed. Retrying in 5s...", targetControl)
				time.Sleep(5 * time.Second)
				continue
			}
			tuneTCPConn(conn)

			hs, err := sendTunnelToken(conn, token)
			if err != nil {
				conn.Close()
				if errors.Is(err, errTunnelAlreadyActive) {
					dash.SetAlreadyActive(hubInfo.Name, targetControl, "You already have your Bedrock Node running on another machine")
					dash.Logf("Tunnel is already active on another machine. Standing by.")
					time.Sleep(30 * time.Second)
					continue
				}
				if errors.Is(err, errInvalidToken) {
					if customControl != "" {
						dash.Logf("Auth rejected on %s — resetting token...", targetControl)
						clearSavedTokens()
						targetControl, token = obtainOrClaimToken(customControl, customWeb, "")
						handshakeFailures = 0
						time.Sleep(3 * time.Second)
						continue
					}
					dash.Logf("Token not on %s — checking other regional hubs...", targetControl)
					targetControl = ""
				} else {
					handshakeFailures++
					dash.SetState("OFFLINE", hubInfo.Name, targetControl, false)
					dash.Logf("Handshake failed (%v). Retrying in 5s...", err)
					if handshakeFailures%handshakeHint == 0 {
						dash.Logf("Tip: Run with -reset-token if you need to claim a new tunnel.")
					}
					time.Sleep(5 * time.Second)
					continue
				}
			} else {
				authConn = conn
				activeControl = targetControl
				activeHS = hs
				handshakeFailures = 0
			}
		}

		if authConn == nil {
			nodes := getActiveNodeList(customControl, customWeb)
			reachable := 0
			rejected := 0
			alreadyRunningNode := ""

			for _, n := range nodes {
				if dash.State != "ALREADY_ACTIVE" {
					dash.SetState("CONNECTING", n.Name, n.ControlAddr, false)
					dash.Logf("Checking regional hub %s...", n.Name)
				}
				conn, err := net.DialTimeout("tcp", n.ControlAddr, 6*time.Second)
				if err != nil {
					continue
				}
				tuneTCPConn(conn)
				reachable++

				hs, err := sendTunnelToken(conn, token)
				if err == nil {
					authConn = conn
					activeControl = n.ControlAddr
					targetControl = n.ControlAddr
					activeHS = hs
					_ = saveNodeAddr(targetControl)
					handshakeFailures = 0
					break
				}
				conn.Close()
				if errors.Is(err, errTunnelAlreadyActive) {
					alreadyRunningNode = n.ControlAddr
					targetControl = n.ControlAddr
					_ = saveNodeAddr(targetControl)
					break
				}
				if errors.Is(err, errInvalidToken) {
					rejected++
				}
			}

			if alreadyRunningNode != "" {
				hubInfo := resolveNodeInput(alreadyRunningNode)
				dash.SetAlreadyActive(hubInfo.Name, alreadyRunningNode, "You already have your Bedrock Node running on another machine")
				dash.Logf("Tunnel is already active on %s. Standing by.", hubInfo.Name)
				time.Sleep(30 * time.Second)
				continue
			}

			if authConn == nil {
				if reachable > 0 && rejected == reachable {
					dash.Logf("Token rejected by all hubs — claiming a fresh tunnel...")
					clearSavedTokens()
					targetControl, token = obtainOrClaimToken(customControl, customWeb, "")
					handshakeFailures = 0
					time.Sleep(3 * time.Second)
					continue
				}
				handshakeFailures++
				dash.SetState("OFFLINE", "Searching Regional Hubs...", "", false)
				dash.Logf("Could not reach regional hubs yet. Retrying in 5s...")
				time.Sleep(5 * time.Second)
				continue
			}
		}

		hubInfo := resolveNodeInput(activeControl)
		if activeHS != nil {
			dash.UpdateHandshakeTunnels(hubInfo.Code, *activeHS)
		}

		sess, err := yamux.Client(authConn, newYamuxConfig())
		if err != nil {
			dash.SetState("OFFLINE", "", activeControl, false)
			dash.Logf("Yamux session error: %v", err)
			authConn.Close()
			time.Sleep(3 * time.Second)
			continue
		}

		connectedStart := time.Now()
		stopRawUDP := make(chan struct{})
		var rawUDPStarted atomic.Bool

		go func() {
			select {
			case <-stopRawUDP:
				return
			case <-time.After(1500 * time.Millisecond):
				if !sess.IsClosed() {
					rawUDPStarted.Store(true)
					dash.SetState("ONLINE", hubInfo.Name, activeControl, false)
					dash.Logf("Tunnel ONLINE via %s!", hubInfo.Name)
					runRawUDPFastLane(stopRawUDP, hubInfo.Code, activeControl, token, localUDPIP, localUDPDefault)
				}
			}
		}()

		for {
			stream, err := sess.Accept()
			if err != nil {
				if time.Since(connectedStart) < 3*time.Second && !rawUDPStarted.Load() {
					dash.SetAlreadyActive(hubInfo.Name, activeControl, "You already have your Bedrock Node running on another machine")
					dash.Logf("Tunnel is already active on another machine. Standing by.")
					close(stopRawUDP)
					_ = sess.Close()
					time.Sleep(30 * time.Second)
					goto nextAttempt
				}
				dash.SetState("OFFLINE", hubInfo.Name, activeControl, false)
				dash.Logf("Tunnel disconnected (%v). Reconnecting...", err)
				break
			}
			go handleIncomingStream(stream, localSignaling, localUDPIP, localUDPDefault)
		}

		close(stopRawUDP)
		_ = sess.Close()
	nextAttempt:
	}
}

func runRawUDPFastLane(stopCh <-chan struct{}, regionCode, controlAddr, token, localUDPIP string, localUDPDefault int) {
	rAddr, err := net.ResolveUDPAddr("udp4", controlAddr)
	if err != nil {
		return
	}
	rawConn, err := net.DialUDP("udp4", nil, rAddr)
	if err != nil {
		return
	}
	tuneUDPConn(rawConn, 8*1024*1024)

	var (
		sessionsMu sync.Mutex
		sessions   = make(map[rawPlayerKey]*rawLocalSession)
		activeFlag atomic.Bool
	)

	defer func() {
		_ = rawConn.Close()
		sessionsMu.Lock()
		nRemoved := int32(len(sessions))
		for k, s := range sessions {
			s.Close()
			delete(sessions, k)
		}
		if nRemoved > 0 {
			if dash.ActiveUDP.Add(-nRemoved) < 0 {
				dash.ActiveUDP.Store(0)
			}
		}
		sessionsMu.Unlock()
	}()

	go func() {
		<-stopCh
		_ = rawConn.Close()
	}()

	hbPkt := append([]byte{RawUDPMagicHeartbeat}, []byte(token)...)
	_, _ = rawConn.Write(hbPkt)

	go func() {
		ticker := time.NewTicker(8 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				_, _ = rawConn.Write(hbPkt)
			}
		}
	}()

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				now := time.Now().UnixNano()
				sessionsMu.Lock()
				var expired int32
				for k, s := range sessions {
					if now-s.lastActive.Load() > int64(2*time.Minute) {
						s.Close()
						delete(sessions, k)
						expired++
					}
				}
				if expired > 0 {
					if dash.ActiveUDP.Add(-expired) < 0 {
						dash.ActiveUDP.Store(0)
					}
				}
				sessionsMu.Unlock()
			}
		}
	}()

	buf := make([]byte, 65535)
	for {
		n, err := rawConn.Read(buf)
		if err != nil {
			select {
			case <-stopCh:
				return
			default:
				time.Sleep(50 * time.Millisecond)
				continue
			}
		}
		if n < 2 {
			continue
		}

		switch buf[0] {
		case RawUDPMagicHeartbeat:
			if n >= 2 && buf[1] == 0x01 && !activeFlag.Swap(true) {
				dash.SetRegionFastLane(regionCode, true)
				dash.Logf("[%s] RAW UDP FAST-LANE ACTIVE on %s", regionCode, controlAddr)
			}

		case RawUDPMagicData:
			if n <= 9 {
				continue
			}
			pubPort := binary.BigEndian.Uint16(buf[1:3])
			var key rawPlayerKey
			key.pubPort = pubPort
			copy(key.playerIP[:], buf[3:7])
			key.playerPort = binary.BigEndian.Uint16(buf[7:9])
			payload := buf[9:n]
			dash.BytesIn.Add(uint64(len(payload)))

			targetPort := int(pubPort)
			if targetPort == 0 {
				targetPort = localUDPDefault
			}

			nowNano := time.Now().UnixNano()
			sessionsMu.Lock()
			sess, exists := sessions[key]
			if !exists {
				conns := buildLocalUDPCandidates(localUDPIP, targetPort)
				if len(conns) == 0 {
					sessionsMu.Unlock()
					continue
				}
				sess = &rawLocalSession{conns: conns}
				sess.winnerIdx.Store(-1)
				sess.lastActive.Store(nowNano)
				sessions[key] = sess
				dash.ActiveUDP.Add(1)

				for idx, cc := range conns {
					go func(i int, c candConn, s *rawLocalSession, k rawPlayerKey) {
						replyBuf := make([]byte, 65535)
						outFrame := make([]byte, 65535+9)
						outFrame[0] = RawUDPMagicData
						binary.BigEndian.PutUint16(outFrame[1:3], k.pubPort)
						copy(outFrame[3:7], k.playerIP[:])
						binary.BigEndian.PutUint16(outFrame[7:9], k.playerPort)

						for {
							_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
							rn, rErr := c.conn.Read(replyBuf)
							if rErr != nil {
								errStr := rErr.Error()
								if strings.Contains(errStr, "forcibly closed") || strings.Contains(errStr, "connection refused") {
									continue
								}
								return
							}
							s.lastActive.Store(time.Now().UnixNano())
							if s.winnerIdx.Swap(int32(i)) == -1 {
								dash.Logf("[%s] WebRTC UDP connected to Bedrock at %s", regionCode, c.label)
							}
							dash.BytesOut.Add(uint64(rn))
							copy(outFrame[9:9+rn], replyBuf[:rn])
							_, _ = rawConn.Write(outFrame[:9+rn])
						}
					}(idx, cc, sess, key)
				}
			}
			sess.lastActive.Store(nowNano)
			sessionsMu.Unlock()

			wIdx := int(sess.winnerIdx.Load())
			if wIdx >= 0 && wIdx < len(sess.conns) {
				_, _ = sess.conns[wIdx].conn.Write(payload)
			} else {
				for _, c := range sess.conns {
					_, _ = c.conn.Write(payload)
				}
			}
		}
	}
}

func handleIncomingStream(stream net.Conn, localSignaling, localUDPIP string, localUDPDefault int) {
	defer stream.Close()

	typeBuf := make([]byte, 1)
	if _, err := io.ReadFull(stream, typeBuf); err != nil {
		return
	}

	switch typeBuf[0] {
	case StreamTypeHTTP, StreamTypeTCP, StreamTypeTCPMapped:
		targetSignaling := localSignaling
		if typeBuf[0] == StreamTypeTCPMapped {
			var portBuf [2]byte
			if _, err := io.ReadFull(stream, portBuf[:]); err != nil {
				return
			}
			mappedPort := int(binary.BigEndian.Uint16(portBuf[:]))
			if mappedPort > 0 {
				host, _, err := net.SplitHostPort(localSignaling)
				if err != nil || host == "" {
					host = "127.0.0.1"
				}
				targetSignaling = net.JoinHostPort(host, fmt.Sprintf("%d", mappedPort))
			}
		}

		localConn, err := net.DialTimeout("tcp", targetSignaling, 5*time.Second)
		if err != nil {
			dash.Logf("Local Bedrock server unreachable at %s", targetSignaling)
			return
		}
		defer localConn.Close()
		tuneTCPConn(localConn)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			buf := make([]byte, 65536)
			n, _ := io.CopyBuffer(localConn, stream, buf)
			if n > 0 {
				dash.BytesIn.Add(uint64(n))
			}
			if tc, ok := localConn.(*net.TCPConn); ok {
				_ = tc.CloseWrite()
			}
		}()
		go func() {
			defer wg.Done()
			buf := make([]byte, 65536)
			n, _ := io.CopyBuffer(stream, localConn, buf)
			if n > 0 {
				dash.BytesOut.Add(uint64(n))
			}
			_ = stream.Close()
		}()
		wg.Wait()

	case StreamTypeUDP:
		var portBuf [2]byte
		if _, err := io.ReadFull(stream, portBuf[:]); err != nil {
			return
		}
		targetPort := int(binary.BigEndian.Uint16(portBuf[:]))
		if targetPort == 0 {
			targetPort = localUDPDefault
		}

		conns := buildLocalUDPCandidates(localUDPIP, targetPort)
		if len(conns) == 0 {
			return
		}
		defer func() {
			for _, c := range conns {
				_ = c.conn.Close()
			}
		}()

		var (
			writeMu   sync.Mutex
			winnerIdx atomic.Int32
		)
		winnerIdx.Store(-1)

		for idx, cc := range conns {
			go func(i int, c candConn) {
				buf := make([]byte, 65535)
				for {
					_ = c.conn.SetReadDeadline(time.Now().Add(2 * time.Minute))
					n, err := c.conn.Read(buf)
					if err != nil {
						errStr := err.Error()
						if strings.Contains(errStr, "forcibly closed") || strings.Contains(errStr, "connection refused") {
							time.Sleep(50 * time.Millisecond)
							continue
						}
						return
					}
					if winnerIdx.Swap(int32(i)) == -1 {
						dash.Logf("Yamux UDP fallback active to %s", c.label)
					}
					dash.BytesOut.Add(uint64(n))
					writeMu.Lock()
					wErr := writeFramedPacket(stream, buf[:n])
					writeMu.Unlock()
					if wErr != nil {
						_ = stream.Close()
						return
					}
				}
			}(idx, cc)
		}

		for {
			pkt, err := readFramedPacket(stream)
			if err != nil {
				break
			}
			dash.BytesIn.Add(uint64(len(pkt)))
			wIdx := int(winnerIdx.Load())
			if wIdx >= 0 && wIdx < len(conns) {
				_ = conns[wIdx].conn.SetWriteDeadline(time.Now().Add(2 * time.Minute))
				_, _ = conns[wIdx].conn.Write(pkt)
				continue
			}
			for _, c := range sessConns(conns) {
				_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Minute))
				_, _ = c.conn.Write(pkt)
			}
		}
	}
}

func sessConns(conns []candConn) []candConn {
	return conns
}

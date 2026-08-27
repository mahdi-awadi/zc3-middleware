package gym

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/awadi/zk-c3-middleware/internal/c3"
)

// Device is one ZKAccess C3 candidate found on the local network.
type Device struct {
	Addr      string `json:"addr"` // "192.168.0.102:4370"
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	SN        string `json:"sn,omitempty"`
	Model     string `json:"model,omitempty"`
	Firmware  string `json:"firmware,omitempty"`
	MAC       string `json:"mac,omitempty"`
	Doors     int    `json:"doors,omitempty"`
	Confirmed bool   `json:"confirmed"` // C3 handshake + device info succeeded
	LatencyMS int64  `json:"latency_ms"`
}

const (
	c3Port       = 4370
	dialTimeout  = 450 * time.Millisecond
	probeTimeout = 2 * time.Second
	maxHosts     = 1024 // total sweep cap across all interfaces
	scanWorkers  = 128
)

// ScanNetwork sweeps the machine's local IPv4 subnets for ZKAccess C3 panels:
// every host with TCP 4370 open is reported, and each one is probed with the
// C3 protocol (connect + device info) to confirm it and read its identity.
// skipProbe lists addresses NOT to probe (the live panel connection — the C3
// is single-session for control, so we must not steal its session); they are
// still reported when the port answers.
func ScanNetwork(ctx context.Context, skipProbe ...string) []Device {
	skip := map[string]bool{}
	for _, a := range skipProbe {
		skip[a] = true
	}

	hosts := localSweepTargets()
	type hit struct {
		ip      string
		latency time.Duration
	}
	var (
		mu   sync.Mutex
		hits []hit
		wg   sync.WaitGroup
		sem  = make(chan struct{}, scanWorkers)
	)
	d := net.Dialer{Timeout: dialTimeout}
	for _, ip := range hosts {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", ip, c3Port))
			if err != nil {
				return
			}
			conn.Close()
			mu.Lock()
			hits = append(hits, hit{ip: ip, latency: time.Since(start)})
			mu.Unlock()
		}(ip)
	}
	wg.Wait()

	// Probe each hit with the real C3 protocol to confirm and identify it.
	out := make([]Device, 0, len(hits))
	for _, h := range hits {
		dev := Device{
			Addr: fmt.Sprintf("%s:%d", h.ip, c3Port), IP: h.ip, Port: c3Port,
			LatencyMS: h.latency.Milliseconds(),
		}
		if !skip[dev.Addr] {
			if info, err := probeC3(ctx, dev.Addr); err == nil {
				dev.Confirmed = true
				dev.SN = info.SerialNumber
				dev.Model = info.DeviceName
				dev.Firmware = info.Firmware
				dev.MAC = info.MAC
				dev.Doors = info.Doors
			}
		} else {
			dev.Confirmed = true // it's the live panel we're connected to
		}
		out = append(out, dev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}

// probeC3 performs a short C3 handshake + device-info read.
func probeC3(ctx context.Context, addr string) (c3.DeviceInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cl := c3.New(addr, c3.WithTimeout(probeTimeout))
	if err := cl.Connect(cctx); err != nil {
		return c3.DeviceInfo{}, err
	}
	defer cl.Close()
	return cl.DeviceInfo(cctx)
}

// localSweepTargets enumerates host IPs to try: every address in the /24
// around each non-loopback IPv4 interface (narrower masks are honored),
// capped at maxHosts total.
func localSweepTargets() []string {
	seen := map[string]bool{}
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			// Sweep at most a /24 around this interface address.
			mask := ipn.Mask
			if ones, _ := mask.Size(); ones < 24 {
				mask = net.CIDRMask(24, 32)
			}
			network := ip4.Mask(mask)
			ones, _ := mask.Size()
			count := 1 << (32 - ones)
			for i := 1; i < count-1; i++ { // skip network + broadcast
				host := make(net.IP, 4)
				copy(host, network)
				host[3] = network[3] + byte(i&0xff)
				host[2] = network[2] + byte((i>>8)&0xff)
				h := host.String()
				if h == ip4.String() || seen[h] {
					continue
				}
				seen[h] = true
				out = append(out, h)
				if len(out) >= maxHosts {
					return out
				}
			}
		}
	}
	return out
}

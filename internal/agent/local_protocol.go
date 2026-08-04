package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxLocalGameEndpointHosts  = 16
	localHostnameLookupTimeout = 500 * time.Millisecond
)

var cachedLocalHostnameHosts = sync.OnceValue(func() []string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), localHostnameLookupTimeout)
	defer cancel()
	return resolveLocalHostnameHosts(ctx, hostname, net.DefaultResolver.LookupIPAddr)
})

func localGameEndpointHosts(candidates ...string) []string {
	assigned := localInterfaceHosts()
	allCandidates := make([]string, 0, len(candidates)+len(cachedLocalHostnameHosts()))
	allCandidates = append(allCandidates, candidates...)
	allCandidates = append(allCandidates, cachedLocalHostnameHosts()...)
	return localGameEndpointHostsWithAssigned(assigned, allCandidates...)
}

func resolveLocalHostnameHosts(ctx context.Context, hostname string, lookup func(context.Context, string) ([]net.IPAddr, error)) []string {
	addresses, err := lookup(ctx, strings.TrimSpace(hostname))
	if err != nil {
		return nil
	}
	hosts := make([]string, 0, len(addresses))
	seen := make(map[string]bool, len(addresses))
	for _, address := range addresses {
		host := normalizedLocalInterfaceHost(address.IP)
		if host == "" || seen[host] || len(hosts) >= maxLocalGameEndpointHosts {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	return hosts
}

func localInterfaceHosts() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	hosts := make([]string, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			var ip net.IP
			switch value := address.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			default:
				parsed, _, parseErr := net.ParseCIDR(address.String())
				if parseErr == nil {
					ip = parsed
				}
			}
			if host := normalizedLocalInterfaceHost(ip); host != "" {
				hosts = append(hosts, host)
			}
		}
	}
	return hosts
}

func normalizedLocalInterfaceHost(ip net.IP) string {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return ""
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String()
	}
	if ip.IsLoopback() || ip.IsGlobalUnicast() {
		return ip.String()
	}
	return ""
}

func localGameEndpointHostsWithAssigned(assigned []string, candidates ...string) []string {
	assignedSet := make(map[string]bool, len(assigned))
	normalizedAssigned := make([]string, 0, len(assigned))
	for _, candidate := range assigned {
		ip := net.ParseIP(strings.Trim(strings.TrimSpace(candidate), "[]"))
		host := normalizedLocalInterfaceHost(ip)
		if host == "" || assignedSet[host] {
			continue
		}
		assignedSet[host] = true
		normalizedAssigned = append(normalizedAssigned, host)
	}
	sort.Slice(normalizedAssigned, func(i, j int) bool {
		i4 := net.ParseIP(normalizedAssigned[i]).To4() != nil
		j4 := net.ParseIP(normalizedAssigned[j]).To4() != nil
		if i4 != j4 {
			return i4
		}
		return normalizedAssigned[i] < normalizedAssigned[j]
	})

	hosts := make([]string, 0, len(normalizedAssigned)+2)
	seen := make(map[string]bool, len(normalizedAssigned)+2)
	add := func(host string) {
		if host == "" || seen[host] || len(hosts) >= maxLocalGameEndpointHosts {
			return
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	for _, candidate := range candidates {
		ip := net.ParseIP(strings.Trim(strings.TrimSpace(candidate), "[]"))
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		host := ip.String()
		if ipv4 := ip.To4(); ipv4 != nil {
			host = ipv4.String()
		}
		if ip.IsLoopback() || assignedSet[host] {
			add(host)
		}
	}
	for _, host := range normalizedAssigned {
		if !net.ParseIP(host).IsLoopback() {
			add(host)
		}
	}
	add("127.0.0.1")
	add("::1")
	return hosts
}

func gameEndpointPort(record InstanceRecord, payload operationPayload) int {
	port := payload.RCONPort
	if port == 0 {
		port = payload.Port
	}
	if port == 0 {
		port = record.RCONPort
	}
	return port
}

func executeLocalRCON(ctx context.Context, record InstanceRecord, payload operationPayload, password, command string, timeout time.Duration) (string, error) {
	port := gameEndpointPort(record, payload)
	if port <= 0 || port > 65535 {
		return "", errors.New("invalid RCON port")
	}
	return executeRCONOnLocalHosts(ctx, localGameEndpointHosts(payload.RCONHost, record.RCONHost), port, password, command, timeout)
}

func executeRCONOnLocalHosts(ctx context.Context, hosts []string, port int, password, command string, timeout time.Duration) (string, error) {
	var lastErr error
	for _, host := range hosts {
		response, err := (RCONClient{Timeout: timeout}).Execute(ctx, net.JoinHostPort(host, strconv.Itoa(port)), password, command)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if ctx.Err() != nil || !isRCONDialError(err) {
			return "", err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no safe local address is available")
	}
	return "", fmt.Errorf("connect RCON on Agent local addresses %s: %w", strings.Join(hosts, ", "), lastErr)
}

func isRCONDialError(err error) bool {
	var netErr *net.OpError
	return errors.As(err, &netErr) && netErr.Op == "dial"
}

type a2sInfoResult struct {
	info A2SInfo
	err  error
}

func queryLocalA2SInfo(ctx context.Context, record InstanceRecord, payload operationPayload, timeout time.Duration) (A2SInfo, error) {
	port := gameEndpointPort(record, payload)
	if port <= 0 || port > 65535 {
		return A2SInfo{}, errors.New("invalid A2S port")
	}
	hosts := localGameEndpointHosts(payload.RCONHost, record.RCONHost)
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan a2sInfoResult, len(hosts))
	for _, host := range hosts {
		address := net.JoinHostPort(host, strconv.Itoa(port))
		go func() {
			info, err := (A2SClient{Timeout: timeout}).Info(queryCtx, address)
			results <- a2sInfoResult{info: info, err: err}
		}()
	}
	var lastErr error
	for range hosts {
		select {
		case result := <-results:
			if result.err == nil {
				return result.info, nil
			}
			lastErr = result.err
		case <-ctx.Done():
			return A2SInfo{}, ctx.Err()
		}
	}
	return A2SInfo{}, fmt.Errorf("query A2S on Agent local addresses %s: %w", strings.Join(hosts, ", "), lastErr)
}

type a2sPlayersResult struct {
	players []A2SPlayer
	err     error
}

func queryLocalA2SPlayers(ctx context.Context, record InstanceRecord, payload operationPayload, timeout time.Duration) ([]A2SPlayer, error) {
	port := gameEndpointPort(record, payload)
	if port <= 0 || port > 65535 {
		return nil, errors.New("invalid A2S port")
	}
	hosts := localGameEndpointHosts(payload.RCONHost, record.RCONHost)
	queryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan a2sPlayersResult, len(hosts))
	for _, host := range hosts {
		address := net.JoinHostPort(host, strconv.Itoa(port))
		go func() {
			players, err := (A2SClient{Timeout: timeout}).Players(queryCtx, address)
			results <- a2sPlayersResult{players: players, err: err}
		}()
	}
	var lastErr error
	for range hosts {
		select {
		case result := <-results:
			if result.err == nil {
				return result.players, nil
			}
			lastErr = result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("query A2S players on Agent local addresses %s: %w", strings.Join(hosts, ", "), lastErr)
}

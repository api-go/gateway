package gateway

import (
	"apigo.cc/go/encoding"
	"apigo.cc/go/file"
	"apigo.cc/go/safe"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultAdminListen prefers an active private IPv4 address on a non-tunnel interface.
// Enumeration cannot prove an interface is the intended LAN; explicit AdminListen wins.
func DefaultAdminListen() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1:81", nil
	}
	sort.Slice(interfaces, func(i, j int) bool { return interfaces[i].Index < interfaces[j].Index })
	ipv6 := ""
	for _, network := range interfaces {
		if network.Flags&net.FlagUp == 0 || network.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(network.Name)
		excluded := false
		for _, prefix := range []string{"docker", "veth", "br-", "virbr", "tun", "tap", "utun", "wg", "tailscale", "zt"} {
			if strings.HasPrefix(name, prefix) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		addresses, err := network.Addrs()
		if err != nil {
			continue
		}
		sort.Slice(addresses, func(i, j int) bool { return addresses[i].String() < addresses[j].String() })
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil {
				continue
			}
			ip := prefix.Addr().Unmap()
			if !ip.IsPrivate() || ip.IsLoopback() {
				continue
			}
			if ip.Is4() {
				return net.JoinHostPort(ip.String(), "81"), nil
			}
			if ipv6 == "" {
				ipv6 = net.JoinHostPort(ip.String(), "81")
			}
		}
	}
	if ipv6 != "" {
		return ipv6, nil
	}
	return "127.0.0.1:81", nil
}

func resolveAdminConfig(config *Config) error {
	if config.AdminDisabled {
		return nil
	}
	if config.AdminListen == "" {
		listen, err := DefaultAdminListen()
		if err != nil {
			return err
		}
		config.AdminListen = listen
	}
	_, port, err := net.SplitHostPort(config.AdminListen)
	if err != nil || port == "" {
		return fmt.Errorf("invalid AdminListen %q; expected IP:port", config.AdminListen)
	}
	return nil
}

// WriteDefaultAdminConfig creates a small first-start template without replacing existing files.
func WriteDefaultAdminConfig(name string, config Config) error {
	if file.Exists(name) {
		return nil
	}
	if err := resolveAdminConfig(&config); err != nil {
		return err
	}
	value := fmt.Sprintf("# Gateway management; explicit listen avoids reselecting after network changes.\nAdminDisabled: %t\nAdminListen: %q\n", config.AdminDisabled, config.AdminListen)
	temporary := filepath.Join(filepath.Dir(name), ".go-gateway-config-"+encoding.Hex(safe.MakeSafeToken(6)))
	if err := privateWrite(temporary, []byte(value)); err != nil {
		return err
	}
	defer file.Remove(temporary)
	if err := os.Link(temporary, name); err != nil && !os.IsExist(err) {
		return err
	}
	return nil
}

// Match config.Load's search locations so first-start generation cannot shadow
// an existing executable-directory or home-directory Gateway configuration.
func hasGatewayConfig() bool {
	directories := []string{"."}
	if executable, err := os.Executable(); err == nil {
		directories = append(directories, filepath.Dir(executable))
	}
	if home, err := os.UserHomeDir(); err == nil {
		directories = append(directories, home)
	}
	for _, directory := range directories {
		for _, extension := range []string{"yaml", "yml", "json"} {
			if file.Exists(filepath.Join(directory, "gateway."+extension)) {
				return true
			}
		}
	}
	return false
}

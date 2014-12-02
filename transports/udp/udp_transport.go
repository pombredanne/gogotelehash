// Package udp implements the UDP transport.
//
// The UDP transport is NAT-able.
package udp

import (
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/telehash/gogotelehash/hashname"
	"github.com/telehash/gogotelehash/transports"
	"github.com/telehash/gogotelehash/transports/nat"
)

func init() {
	transports.RegisterAddrDecoder("udp4", decodeAddress)
	transports.RegisterAddrDecoder("udp6", decodeAddress)
}

// Config for the UDP transport. Typically the zero value is sufficient to get started.
//
//   e3x.New(keys, udp.Config{})
type Config struct {
	// Can be set to UDPv4, UDPv6 or can be left blank.
	// Defaults to UDPv4
	Network string

	// Can be set to an address and/or port.
	// The zero value will bind it to a random port while listening on all interfaces.
	// When port is unspecified ("127.0.0.1") a random port will be chosen.
	// When ip is unspecified (":3000") the transport will listen on all interfaces.
	Addr string

	// Can be set to an ip network (in CIDR format).
	// Setting this value will restrict all outbound traffic on this transport to the
	// specified network.
	Dest string
}

type addr struct {
	hn  hashname.H
	net string
	net.UDPAddr
}

func NewAddr(ip net.IP, port uint16) (transports.Addr, error) {
	if ip == nil || port == 0 {
		return nil, errors.New("udp: invalid address")
	}

	a := &addr{}

	a.IP = ip
	a.Port = int(port)

	if ip.To4() == nil {
		a.net = UDPv6
	} else {
		a.net = UDPv4
	}

	return a, nil
}

type transport struct {
	net   string
	laddr *net.UDPAddr
	dest  *net.IPNet
	c     *net.UDPConn
}

var (
	_ transports.Addr      = (*addr)(nil)
	_ nat.NATableAddr      = (*addr)(nil)
	_ transports.Transport = (*transport)(nil)
	_ transports.Config    = Config{}
)

const (
	// UDPv4 is used for IPv4 UDP networks
	UDPv4 = "udp4"
	// UDPv6 is used for IPv6 UDP networks
	UDPv6 = "udp6"
)

// Open opens the transport.
func (c Config) Open() (transports.Transport, error) {
	var (
		ipnet *net.IPNet
		addr  *net.UDPAddr
		err   error
	)

	if c.Network == "" {
		c.Network = UDPv4
	}
	if c.Addr == "" {
		c.Addr = ":0"
	}
	if c.Dest == "" {
		if c.Network == UDPv4 {
			c.Dest = "0.0.0.0/0"
		} else {
			c.Dest = "::0/0"
		}
	}

	if c.Network != UDPv4 && c.Network != UDPv6 {
		return nil, errors.New("udp: Network must be either `udp4` or `udp6`")
	}

	{ // parse and verify source address
		addr, err = net.ResolveUDPAddr(c.Network, c.Addr)
		if err != nil {
			return nil, err
		}

		if c.Network == UDPv4 && addr.IP != nil && addr.IP.To4() == nil {
			return nil, errors.New("udp: expected a IPv4 address")
		}

		if c.Network == UDPv6 && addr.IP != nil && addr.IP.To4() != nil {
			return nil, errors.New("udp: expected a IPv6 address")
		}
	}

	{ // parse and verify destination network
		_, ipnet, err = net.ParseCIDR(c.Dest)
		if err != nil {
			return nil, err
		}

		if c.Network == UDPv4 && ipnet.IP != nil && ipnet.IP.To4() == nil {
			return nil, errors.New("udp: expected a IPv4 network")
		}

		if c.Network == UDPv6 && ipnet.IP != nil && ipnet.IP.To4() != nil {
			return nil, errors.New("udp: expected a IPv6 network")
		}
	}

	conn, err := net.ListenUDP(c.Network, addr)
	if err != nil {
		return nil, err
	}

	addr = conn.LocalAddr().(*net.UDPAddr)

	return &transport{c.Network, addr, ipnet, conn}, nil
}

func (t *transport) ReadMessage(p []byte) (int, transports.Addr, error) {
	const errUseOfClosedNet = "use of closed network connection"

	n, a, err := t.c.ReadFromUDP(p)
	if err != nil {
		if err.Error() == errUseOfClosedNet {
			err = transports.ErrClosed
		}
		return 0, nil, err
	}

	return n, &addr{net: t.net, UDPAddr: *a}, nil
}

func (t *transport) WriteMessage(p []byte, dst transports.Addr) error {
	a, ok := dst.(*addr)
	if !ok || a == nil {
		return transports.ErrInvalidAddr
	}

	if a.net != t.net {
		return transports.ErrInvalidAddr
	}

	if !t.dest.Contains(a.IP) {
		return transports.ErrInvalidAddr
	}

	n, err := t.c.WriteToUDP(p, &a.UDPAddr)
	if err != nil {
		return err
	}

	if n != len(p) {
		return io.ErrShortWrite
	}

	return nil
}

func (t *transport) LocalAddresses() []transports.Addr {
	var (
		port  int
		addrs []transports.Addr
	)

	{
		a := t.laddr
		port = a.Port
		if !a.IP.IsUnspecified() {
			switch t.net {

			case UDPv4:
				if v4 := a.IP.To4(); v4 != nil {
					addrs = append(addrs, &addr{
						net: t.net,
						UDPAddr: net.UDPAddr{
							IP:   v4,
							Port: a.Port,
							Zone: a.Zone,
						},
					})
				}

			case UDPv6:
				if v4 := a.IP.To4(); v4 == nil {
					addrs = append(addrs, &addr{
						net: t.net,
						UDPAddr: net.UDPAddr{
							IP:   a.IP.To16(),
							Port: a.Port,
							Zone: a.Zone,
						},
					})
				}

			}
			return addrs
		}
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return addrs
	}
	for _, iface := range ifaces {
		iaddrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, iaddr := range iaddrs {
			var (
				ip   net.IP
				zone string
			)

			switch x := iaddr.(type) {
			case *net.IPAddr:
				ip = x.IP
				zone = x.Zone
			case *net.IPNet:
				ip = x.IP
				zone = ""
			}

			if ip.IsMulticast() ||
				ip.IsUnspecified() ||
				ip.IsInterfaceLocalMulticast() ||
				ip.IsLinkLocalMulticast() {
				continue
			}

			switch t.net {

			case UDPv4:
				if v4 := ip.To4(); v4 != nil {
					addrs = append(addrs, &addr{
						net: t.net,
						UDPAddr: net.UDPAddr{
							IP:   ip.To4(),
							Port: port,
							Zone: zone,
						},
					})
				}

			case UDPv6:
				if v4 := ip.To4(); v4 == nil {
					addrs = append(addrs, &addr{
						net: t.net,
						UDPAddr: net.UDPAddr{
							IP:   ip.To16(),
							Port: port,
							Zone: zone,
						},
					})
				}

			}
		}
	}

	return addrs
}

func (t *transport) Close() error {
	return t.c.Close()
}

func (a *addr) Network() string {
	return a.net
}

func decodeAddress(data []byte) (transports.Addr, error) {
	var desc struct {
		Type string `json:"type"`
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}

	err := json.Unmarshal(data, &desc)
	if err != nil {
		return nil, transports.ErrInvalidAddr
	}

	ip := net.ParseIP(desc.IP)
	if ip == nil || ip.IsUnspecified() {
		return nil, transports.ErrInvalidAddr
	}

	if desc.Port <= 0 || desc.Port >= 65535 {
		return nil, transports.ErrInvalidAddr
	}

	return &addr{net: desc.Type, UDPAddr: net.UDPAddr{IP: ip, Port: desc.Port}}, nil
}

func (a *addr) MarshalJSON() ([]byte, error) {
	var desc = struct {
		Type string `json:"type"`
		IP   string `json:"ip"`
		Port int    `json:"port"`
	}{
		Type: a.net,
		IP:   a.IP.String(),
		Port: a.Port,
	}
	return json.Marshal(&desc)
}

func (a *addr) Equal(x transports.Addr) bool {
	b := x.(*addr)

	if !a.IP.Equal(b.IP) {
		return false
	}

	if a.Port != b.Port {
		return false
	}

	return true
}

func (a *addr) String() string {
	data, err := a.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return string(data)
}

func (a *addr) Associate(hn hashname.H) transports.Addr {
	b := new(addr)
	*b = *a
	b.hn = hn
	return b
}

func (a *addr) Hashname() hashname.H {
	return a.hn
}

func (a *addr) InternalAddr() (proto string, ip net.IP, port int) {
	if a == nil ||
		a.IP.IsLoopback() ||
		a.IP.IsMulticast() ||
		a.IP.IsUnspecified() ||
		a.IP.IsInterfaceLocalMulticast() ||
		a.IP.IsLinkLocalMulticast() {
		return "", nil, -1
	}
	return "udp", a.IP, a.Port
}

func (a *addr) MakeGlobal(ip net.IP, port int) transports.Addr {
	if a == nil {
		return nil
	}

	return &addr{"", a.net, net.UDPAddr{IP: ip, Port: port}}
}

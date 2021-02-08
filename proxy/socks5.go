package proxy

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net"

	"github.com/xjasonlyu/tun2socks/component/dialer"
	"github.com/xjasonlyu/tun2socks/component/socks5"
	M "github.com/xjasonlyu/tun2socks/constant"
	"github.com/xjasonlyu/tun2socks/proxy/proto"
)

var _ Proxy = (*Socks5)(nil)

type Socks5 struct {
	*Base

	user string
	pass string
}

func NewSocks5(addr, user, pass string) (*Socks5, error) {
	return &Socks5{
		Base: &Base{
			addr:  addr,
			proto: proto.Socks5,
		},
		user: user,
		pass: pass,
	}, nil
}

func (ss *Socks5) DialContext(ctx context.Context, metadata *M.Metadata) (c net.Conn, err error) {
	c, err = dialer.DialContext(ctx, "tcp", ss.Addr())
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", ss.Addr(), err)
	}
	setKeepAlive(c)

	defer func() {
		if err != nil && c != nil {
			c.Close()
		}
	}()

	var user *socks5.User
	if ss.user != "" {
		user = &socks5.User{
			Username: ss.user,
			Password: ss.pass,
		}
	}

	_, err = socks5.ClientHandshake(c, metadata.SerializesSocksAddr(), socks5.CmdConnect, user)
	return
}

func (ss *Socks5) DialUDP(*M.Metadata) (_ net.PacketConn, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), tcpConnectTimeout)
	defer cancel()

	c, err := dialer.DialContext(ctx, "tcp", ss.Addr())
	if err != nil {
		err = fmt.Errorf("connect to %s: %w", ss.Addr(), err)
		return
	}
	setKeepAlive(c)

	defer func() {
		if err != nil && c != nil {
			c.Close()
		}
	}()

	var user *socks5.User
	if ss.user != "" {
		user = &socks5.User{
			Username: ss.user,
			Password: ss.pass,
		}
	}

	// The UDP ASSOCIATE request is used to establish an association within
	// the UDP relay process to handle UDP datagrams.  The DST.ADDR and
	// DST.PORT fields contain the address and port that the client expects
	// to use to send UDP datagrams on for the association.  The server MAY
	// use this information to limit access to the association.  If the
	// client is not in possession of the information at the time of the UDP
	// ASSOCIATE, the client MUST use a port number and address of all
	// zeros. RFC1928
	var targetAddr socks5.Addr = []byte{socks5.AtypIPv4, 0, 0, 0, 0, 0, 0}

	addr, err := socks5.ClientHandshake(c, targetAddr, socks5.CmdUDPAssociate, user)
	if err != nil {
		return nil, fmt.Errorf("client hanshake: %w", err)
	}

	pc, err := dialer.ListenPacket("udp", "")
	if err != nil {
		return nil, fmt.Errorf("listen packet: %w", err)
	}

	go func() {
		io.Copy(ioutil.Discard, c)
		c.Close()
		// A UDP association terminates when the TCP connection that the UDP
		// ASSOCIATE request arrived on terminates. RFC1928
		pc.Close()
	}()

	bindAddr := addr.UDPAddr()
	if bindAddr.IP.IsUnspecified() { /* e.g. "0.0.0.0" or "::" */
		udpAddr, err := net.ResolveUDPAddr("udp", ss.Addr())
		if err != nil {
			return nil, fmt.Errorf("resolve udp address %s: %w", ss.Addr(), err)
		}
		bindAddr.IP = udpAddr.IP
	}

	return &socksPacketConn{PacketConn: pc, rAddr: bindAddr, tcpConn: c}, nil
}

type socksPacketConn struct {
	net.PacketConn

	rAddr   net.Addr
	tcpConn net.Conn
}

func (pc *socksPacketConn) WriteTo(b []byte, addr net.Addr) (n int, err error) {
	var packet []byte
	if m, ok := addr.(*M.Metadata); ok {
		packet, err = socks5.EncodeUDPPacket(m.SerializesSocksAddr(), b)
	} else {
		packet, err = socks5.EncodeUDPPacket(socks5.ParseAddrToSocksAddr(addr), b)
	}

	if err != nil {
		return
	}
	return pc.PacketConn.WriteTo(packet, pc.rAddr)
}

func (pc *socksPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, _, err := pc.PacketConn.ReadFrom(b)
	if err != nil {
		return 0, nil, err
	}

	addr, payload, err := socks5.DecodeUDPPacket(b)
	if err != nil {
		return 0, nil, err
	}

	udpAddr := addr.UDPAddr()
	if udpAddr == nil {
		return 0, nil, fmt.Errorf("convert %s to UDPAddr is nil", addr)
	}

	// due to DecodeUDPPacket is mutable, record addr length
	copy(b, payload)
	return n - len(addr) - 3, udpAddr, nil
}

func (pc *socksPacketConn) Close() error {
	pc.tcpConn.Close()
	return pc.PacketConn.Close()
}

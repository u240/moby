// Package netutils provides network utility functions.
package netutils

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/containerd/log"
)

// GenerateMACFromIP returns a locally administered MAC address where the 4 least
// significant bytes are derived from the IPv4 address.
func GenerateMACFromIP(ip net.IP) net.HardwareAddr {
	hw := make(net.HardwareAddr, 6)
	// The first byte of the MAC address has to comply with these rules:
	// 1. Unicast: Set the least-significant bit to 0.
	// 2. Address is locally administered: Set the second-least-significant bit (U/L) to 1.
	hw[0] = 0x02
	// The first 24 bits of the MAC represent the Organizationally Unique Identifier (OUI).
	// Since this address is locally administered, we can do whatever we want as long as
	// it doesn't conflict with other addresses.
	hw[1] = 0x42
	// Fill the remaining 4 bytes based on the input
	if ip == nil {
		rand.Read(hw[2:])
	} else {
		copy(hw[2:], ip.To4())
	}
	return hw
}

// GenerateRandomMAC returns a new 6-byte(48-bit) hardware address (MAC)
// that is not multicast and has the local assignment bit set.
func GenerateRandomMAC() net.HardwareAddr {
	hw := make(net.HardwareAddr, 6)
	rand.Read(hw)
	hw[0] &= 0xfe // Unicast: clear multicast bit
	hw[0] |= 0x02 // Locally administered: set local assignment bit
	return hw
}

// GenerateRandomName returns a string of the specified length, created by joining the prefix to random hex characters.
// The length must be strictly larger than len(prefix), or an error will be returned.
func GenerateRandomName(prefix string, length int) (string, error) {
	if length <= len(prefix) {
		return "", fmt.Errorf("invalid length %d for prefix %s", length, prefix)
	}

	// We add 1 here as integer division will round down, and we want to round up.
	b := make([]byte, (length-len(prefix)+1)/2)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}

	// By taking a slice here, we ensure that the string is always the correct length.
	return (prefix + hex.EncodeToString(b))[:length], nil
}

// ReverseIP accepts a V4 or V6 IP string in the canonical form and returns a reversed IP in
// the dotted decimal form . This is used to setup the IP to service name mapping in the optimal
// way for the DNS PTR queries.
func ReverseIP(IP string) string {
	var reverseIP []string

	if net.ParseIP(IP).To4() != nil {
		reverseIP = strings.Split(IP, ".")
		l := len(reverseIP)
		for i, j := 0, l-1; i < l/2; i, j = i+1, j-1 {
			reverseIP[i], reverseIP[j] = reverseIP[j], reverseIP[i]
		}
	} else {
		reverseIP = strings.Split(IP, ":")

		// Reversed IPv6 is represented in dotted decimal instead of the typical
		// colon hex notation
		for key := range reverseIP {
			if reverseIP[key] == "" { // expand the compressed 0s
				reverseIP[key] = strings.Repeat("0000", 8-strings.Count(IP, ":"))
			} else if len(reverseIP[key]) < 4 { // 0-padding needed
				reverseIP[key] = strings.Repeat("0", 4-len(reverseIP[key])) + reverseIP[key]
			}
		}

		reverseIP = strings.Split(strings.Join(reverseIP, ""), "")

		l := len(reverseIP)
		for i, j := 0, l-1; i < l/2; i, j = i+1, j-1 {
			reverseIP[i], reverseIP[j] = reverseIP[j], reverseIP[i]
		}
	}

	return strings.Join(reverseIP, ".")
}

var (
	v6ListenableCached bool
	v6ListenableOnce   sync.Once
)

// IsV6Listenable returns true when `[::1]:0` is listenable.
// IsV6Listenable returns false mostly when the kernel was booted with `ipv6.disable=1` option.
func IsV6Listenable() bool {
	v6ListenableOnce.Do(func() {
		ln, err := net.Listen("tcp6", "[::1]:0")
		if err != nil {
			// When the kernel was booted with `ipv6.disable=1`,
			// we get err "listen tcp6 [::1]:0: socket: address family not supported by protocol"
			// https://github.com/moby/moby/issues/42288
			log.G(context.TODO()).Debugf("v6Listenable=false (%v)", err)
		} else {
			v6ListenableCached = true
			ln.Close()
		}
	})
	return v6ListenableCached
}

// MustParseMAC returns a net.HardwareAddr or panic.
func MustParseMAC(s string) net.HardwareAddr {
	mac, err := net.ParseMAC(s)
	if err != nil {
		panic(err)
	}
	return mac
}

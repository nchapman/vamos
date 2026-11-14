//go:build !linux

package agent

import "net"

// The installed coordinator is Linux-only. This fallback keeps package tests
// portable while ServeSystem is never invoked outside a managed Linux host.
func peerUID(net.Conn) (int, error) { return -1, nil }

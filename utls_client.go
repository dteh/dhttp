package http

import (
	"net"

	tls "github.com/refraction-networking/utls"
)

// unencryptedUTLSConn wraps an unencrypted net.Conn in a utls *UConn so the
// client-side h2 unencrypted upgrade path can hand the connection to the
// HTTP/2 transport. Counterpart of unencryptedTLSConn in server.go; kept here
// so that server.go does not need to import utls.
func unencryptedUTLSConn(c net.Conn) *tls.UConn {
	return tls.UClient(unencryptedNetConnInTLSConn{conn: c}, nil, tls.ClientHelloID{})
}

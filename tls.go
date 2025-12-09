package main

import (
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// ClientHello names for verbose output
var clientHelloNames = map[int]string{
	0: "RandomNoALPN",
	1: "Chrome_Auto",
	2: "Firefox_Auto",
	3: "iOS_Auto",
	4: "Edge_Auto",
	5: "Safari_Auto",
}

// getClientHelloID - returns utls ClientHelloID for given mode
func getClientHelloID(cliMode int) utls.ClientHelloID {
	switch cliMode {
	case 1:
		return utls.HelloChrome_Auto
	case 2:
		return utls.HelloFirefox_Auto
	case 3:
		return utls.HelloIOS_Auto
	case 4:
		return utls.HelloEdge_Auto
	case 5:
		return utls.HelloSafari_Auto
	default:
		return utls.HelloRandomizedNoALPN
	}
}

// tlsHandshake - performs TLS handshake, reports metrics in verbose mode
// Note: Does NOT retry - caller must create new TCP conn for each retry
func tlsHandshake(conn net.Conn, host string, cliMode int, timeout time.Duration, logger LogWriter, threadID int) (*utls.UConn, error) {
	var startTime time.Time
	if verbose {
		startTime = time.Now()
		helloName := clientHelloNames[cliMode]
		logger.Write(fmt.Sprintf("[V] TLS handshake: host=%s, hello=%s\n", host, helloName))
	}

	helloID := getClientHelloID(cliMode)

	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	tlsConf := &utls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
		NextProtos:         []string{"http/1.1"},
	}

	tlsConn := utls.UClient(conn, tlsConf, helloID)
	err := tlsConn.Handshake()

	if err != nil {
		if verbose {
			logger.Write(fmt.Sprintf("[V] TLS handshake failed: %v\n", err))
			stats.ReportTLSRetry(threadID)
		}
		return nil, err
	}

	// Report successful handshake (verbose only)
	if verbose {
		latencyUs := uint32(time.Since(startTime).Microseconds())
		stats.ReportTLS(threadID, latencyUs, 1)

		state := tlsConn.ConnectionState()
		logger.Write(fmt.Sprintf("[V] TLS handshake ok: version=0x%04x, cipher=0x%04x\n",
			state.Version, state.CipherSuite))
	}

	return tlsConn, nil
}

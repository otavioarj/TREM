package main

import (
	"fmt"
	"net"
	"os"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/pkcs12"
)

// ClientHello names for verbose output
var clientHelloNames = map[int]string{
	0: "Random(No)ALPN",
	1: "Chrome_Auto",
	2: "Firefox_Auto",
	3: "iOS_Auto",
	4: "Edge_Auto",
	5: "Safari_Auto",
}

// getClientHelloID - returns utls ClientHelloID for given mode
func getClientHelloID(cliMode int, h2 bool) utls.ClientHelloID {
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
		if h2 {
			return utls.HelloRandomizedALPN
		}
		return utls.HelloRandomizedNoALPN // HTTP/1.1 no ALPN
	}
}

// loadPKCS12Certificate - Loads clientTLS/mTLS Certificate
// used for tlsHandshakeDo as "Certificates" chain
func loadPKCS12Certificate(certPath, password string) (utls.Certificate, error) {
	fileData, err := os.ReadFile(certPath)
	if err != nil {
		return utls.Certificate{}, fmt.Errorf("cannot read PKCS#12: %v", err)
	}

	privateKey, certData, err := pkcs12.Decode(fileData, password)
	if err != nil {
		return utls.Certificate{}, fmt.Errorf("cannot decode PKCS#12: %v", err)
	}

	certDer := certData.Raw
	tlsCert := utls.Certificate{
		Certificate: [][]byte{certDer},
		PrivateKey:  privateKey,
	}

	return tlsCert, nil
}

// tlsHandshakeDo - TLS handshake with ALPN based on h2 flag
// h2=false: ALPN http/1.1 | h2=true: ALPN h2
func tlsHandshakeDo(conn net.Conn, host string, cliMode int, timeout time.Duration, logger LogWriter, threadID int, tlsCert *utls.Certificate,
	h2 bool) (*utls.UConn, error) {
	alpn := []string{"http/1.1"}
	if h2 {
		alpn = []string{"h2"}
	}

	var startTime time.Time
	if verbose {
		startTime = time.Now()
		helloName := clientHelloNames[cliMode]
		logger.Write(fmt.Sprintf("[V] TLS: host=%s, hello=%s, alpn=%v\n", host, helloName, alpn))
	}

	helloID := getClientHelloID(cliMode, h2)

	conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})

	tlsConf := &utls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
		NextProtos:         alpn,
	}

	// ClientCertificate (mTLS) if provided
	if tlsCert != nil {
		tlsConf.Certificates = []utls.Certificate{*tlsCert}
	}

	tlsConn := utls.UClient(conn, tlsConf, helloID)
	err := tlsConn.Handshake()

	if err != nil {
		if verbose {
			logger.Write(fmt.Sprintf("[V] TLS failed: %v\n", err))
			stats.ReportTLSRetry(threadID)
		}
		return nil, err
	}

	if verbose {
		latencyUs := uint32(time.Since(startTime).Microseconds())
		stats.ReportTLS(threadID, latencyUs, 1)

		state := tlsConn.ConnectionState()
		logger.Write(fmt.Sprintf("[V] TLS ok: ver=0x%04x, cipher=0x%04x, alpn=%s\n",
			state.Version, state.CipherSuite, state.NegotiatedProtocol))
	}

	return tlsConn, nil
}

package freeswitch

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"text/template"
	"time"
)

//go:embed conf/freeswitch.xml.tmpl
var confFS embed.FS

// Settings are the values the config template is rendered with. Every port
// sits in the interop peer band (6000-8191), below the SIP band the harness
// hands VoiceBlender instances.
type Settings struct {
	IP          string
	Domain      string
	SIPPort     int
	TLSPort     int
	AuthPort    int
	FlapPort    int
	ESLPort     int
	ESLPassword string
	RTPMin      int
	RTPMax      int
	LogLevel    string
	Users       map[string]string
	FlapProfile string
}

// DefaultSettings is the layout the interop suite expects.
func DefaultSettings() Settings {
	users := map[string]string{}
	for i := 1000; i <= 1009; i++ {
		users[fmt.Sprint(i)] = "interop"
	}
	return Settings{
		IP:          "127.0.0.1",
		Domain:      "127.0.0.1",
		SIPPort:     7060,
		TLSPort:     7061,
		AuthPort:    7062,
		FlapPort:    7063,
		ESLPort:     7021,
		ESLPassword: "ClueCon",
		RTPMin:      7100,
		RTPMax:      7999,
		LogLevel:    "info",
		Users:       users,
		FlapProfile: flapProfile,
	}
}

// Ports lists every port the settings bind, for a free-port check.
func (s Settings) Ports() []int {
	return []int{s.SIPPort, s.TLSPort, s.AuthPort, s.FlapPort, s.ESLPort}
}

func renderConfig(s Settings) ([]byte, error) {
	tmpl, err := template.ParseFS(confFS, "conf/freeswitch.xml.tmpl")
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, s); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteConfDir lays out a FreeSWITCH conf dir under dir: freeswitch.xml, a
// self-signed TLS identity for the TLS listener, and an empty gateways dir.
func WriteConfDir(dir string, s Settings) error {
	xml, err := renderConfig(s)
	if err != nil {
		return err
	}
	for _, sub := range []string{"tls", "gateways"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "freeswitch.xml"), xml, 0o644); err != nil {
		return err
	}
	certPEM, keyPEM, err := selfSignedCert(s.IP)
	if err != nil {
		return err
	}
	agent := append(append([]byte{}, certPEM...), keyPEM...)
	if err := os.WriteFile(filepath.Join(dir, "tls", "agent.pem"), agent, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "tls", "cafile.pem"), certPEM, 0o644)
}

var gatewayTmpl = template.Must(template.New("gw").Parse(`<include>
  <gateway name="{{.Name}}">
    <param name="proxy" value="{{.Registrar}}"/>
    <param name="realm" value="{{.Registrar}}"/>
    <param name="from-domain" value="{{.Domain}}"/>
    <param name="username" value="{{.User}}"/>
    <param name="password" value="{{.Password}}"/>
    <param name="extension" value="{{.User}}"/>
    <param name="register" value="true"/>
    <param name="register-transport" value="udp"/>
    <param name="expire-seconds" value="{{.Expires}}"/>
    <param name="retry-seconds" value="5"/>
    <param name="ping" value="0"/>
  </gateway>
</include>
`))

type gatewaySpec struct {
	Name, Registrar, Domain, User, Password string
	Expires                                 int
}

func renderGateway(g gatewaySpec) ([]byte, error) {
	var buf bytes.Buffer
	err := gatewayTmpl.Execute(&buf, g)
	return buf.Bytes(), err
}

func selfSignedCert(ip string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: ip},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  []net.IP{net.ParseIP(ip)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), nil
}

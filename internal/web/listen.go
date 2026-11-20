package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrRoot — HTTP-сервер RamPlus никогда не работает от root.
var ErrRoot = errors.New("ramplus-web не запускается от root: используйте пользователя ramplus-web (systemctl start ramplus-web)")

// CheckNotRoot — отказ запускаться от root.
func CheckNotRoot(euid int) error {
	if euid == 0 {
		return ErrRoot
	}
	return nil
}

// IsLocal — адрес слушает только loopback (127.0.0.0/8, ::1, localhost).
func IsLocal(listen string) (bool, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false, fmt.Errorf("--listen %q: ожидается адрес:порт (%v)", listen, err)
	}
	if strings.EqualFold(host, "localhost") {
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false, nil // имя — может разрешиться во внешний адрес
	}
	return ip.IsLoopback(), nil
}

// ValidateListen — не-локальный адрес только с TLS.
func ValidateListen(listen string, withTLS bool) error {
	local, err := IsLocal(listen)
	if err != nil {
		return err
	}
	if !local && !withTLS {
		return fmt.Errorf("--listen %s доступен не только с этого компьютера: нужен TLS (--tls-cert и --tls-key или --tls-self-signed); для удалённого доступа без TLS используйте ssh -L 7781:localhost:7781", listen)
	}
	return nil
}

// ListenHost — хост из адреса, если это конкретный адрес (для списка
// разрешённых Host); "" — 0.0.0.0, :: или пусто.
func ListenHost(listen string) string {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() || host == "" {
		return ""
	}
	return host
}

// SelfSigned — самоподписанный сертификат (ECDSA P-256, год) для hosts;
// возвращает и SHA-256 отпечаток для сверки в браузере.
func SelfSigned(hosts []string, now time.Time) (tls.Certificate, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return tls.Certificate{}, "", err
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "RamPlus (self-signed)"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, h := range append([]string{"localhost", "127.0.0.1", "::1"}, hosts...) {
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	sum := sha256.Sum256(der)
	fp := strings.ToUpper(hex.EncodeToString(sum[:]))
	var parts []string
	for i := 0; i < len(fp); i += 2 {
		parts = append(parts, fp[i:i+2])
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, strings.Join(parts, ":"), nil
}

// TLSConfig — TLS 1.2+.
func TLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
}

// HTTPServer — http.Server с таймаутами (срок записи задаётся на запрос:
// у SSE его нет).
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slogLogger(s),
	}
}

func slogLogger(s *Server) *log.Logger { return slog.NewLogLogger(s.cfg.Log.Handler(), slog.LevelWarn) }

package raop

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
)

// This is the public, protocol-wide AirPort compatibility key from the MIT
// MiCast implementation; it is not an account or installation credential.
//
//go:embed airport.pem
var airportPEM []byte

func airportKey() (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(airportPEM)
	if block == nil {
		return nil, errors.New("missing AirPort compatibility key")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}
func decode64(value string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(value + strings.Repeat("=", (4-len(value)%4)%4))
}
func appleResponse(key *rsa.PrivateKey, challenge string, localIP net.IP, mac []byte) (string, error) {
	b, err := decode64(challenge)
	if err != nil || len(b) != 16 || localIP.To4() == nil {
		return "", errors.New("invalid Apple challenge")
	}
	b = append(b, localIP.To4()...)
	b = append(b, mac...)
	for len(b) < 32 {
		b = append(b, 0)
	}
	n := key.Size()
	encoded := make([]byte, n)
	encoded[1] = 1
	for i := 2; i < n-len(b)-1; i++ {
		encoded[i] = 255
	}
	copy(encoded[n-len(b):], b)
	value := new(big.Int).Exp(new(big.Int).SetBytes(encoded), key.D, key.N)
	signed := make([]byte, n)
	value.FillBytes(signed)
	return strings.TrimRight(base64.StdEncoding.EncodeToString(signed), "="), nil
}
func decryptKey(key *rsa.PrivateKey, encoded string) ([]byte, error) {
	b, err := decode64(encoded)
	if err != nil {
		return nil, err
	}
	return rsa.DecryptOAEP(sha1.New(), rand.Reader, key, b, nil)
}

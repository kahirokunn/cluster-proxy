package userserver

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"testing"
)

func TestServerNameForCertificate(t *testing.T) {
	tests := []struct {
		name        string
		certificate *x509.Certificate
		want        string
		wantErr     bool
	}{
		{
			name:        "IP SAN",
			certificate: &x509.Certificate{IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}},
			want:        "127.0.0.1",
		},
		{
			name:        "DNS SAN",
			certificate: &x509.Certificate{DNSNames: []string{"cluster-proxy-addon-user.example.svc"}},
			want:        "cluster-proxy-addon-user.example.svc",
		},
		{
			name:        "wildcard DNS SAN",
			certificate: &x509.Certificate{DNSNames: []string{"*.example.svc"}},
			want:        "tls-health-check.example.svc",
		},
		{
			name:        "common name is not a SAN",
			certificate: &x509.Certificate{Subject: pkix.Name{CommonName: "cluster-proxy-addon-user"}},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := serverNameForCertificate(tt.certificate)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected SAN selection to fail")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected SAN selection error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("unexpected server name: got %q, want %q", got, tt.want)
			}
		})
	}
}

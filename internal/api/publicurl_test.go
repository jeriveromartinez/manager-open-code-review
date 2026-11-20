package api

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestPublicURL(t *testing.T) {
	cases := []struct {
		name    string
		host    string
		headers map[string]string
		tls     bool
		want    string
	}{
		{"plain", "qa.local:8080", nil, false, "http://qa.local:8080"},
		{"tls", "qa.example.com", nil, true, "https://qa.example.com"},
		{"forwarded", "app:8080", map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "qa.example.com"}, false, "https://qa.example.com"},
		{"first value wins", "app:8080", map[string]string{"X-Forwarded-Proto": "https, http"}, false, "https://app:8080"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/", nil)
			r.Host = tc.host
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			if got := (&server{}).requestPublicURL(r); got != tc.want {
				t.Errorf("requestPublicURL = %q, want %q", got, tc.want)
			}
		})
	}
}

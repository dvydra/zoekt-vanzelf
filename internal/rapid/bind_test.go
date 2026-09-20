package rapid

import "testing"

// The proxy serves a searchable index of every repository under Roots. On
// 0.0.0.0 that is readable by anyone on the same network, so loopback is the
// only safe default.
func TestDefaultConfigBindsLoopback(t *testing.T) {
	if got := DefaultConfig().ProxyHost; got != "127.0.0.1" {
		t.Errorf("DefaultConfig().ProxyHost = %q, want 127.0.0.1", got)
	}
}

func TestServerAddr(t *testing.T) {
	tests := []struct {
		host string
		port int
		want string
	}{
		{"127.0.0.1", 6071, "127.0.0.1:6071"},
		{"0.0.0.0", 6071, "0.0.0.0:6071"}, // explicit opt-in to publishing
		{"", 6071, "127.0.0.1:6071"},      // an unset host must not mean "every interface"
		{"::1", 6071, "[::1]:6071"},       // IPv6 gets bracketed by JoinHostPort
	}
	for _, tt := range tests {
		s := &Server{host: tt.host, port: tt.port}
		if got := s.Addr(); got != tt.want {
			t.Errorf("Addr(host=%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
}

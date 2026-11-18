package runtime

import "testing"

func TestContainerBaseURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"http://127.0.0.1:8317", "http://host.docker.internal:8317"},
		{"http://127.0.0.1:8317/v1", "http://host.docker.internal:8317/v1"},
		{"http://localhost:8317", "http://host.docker.internal:8317"},
		{"http://[::1]:8317", "http://host.docker.internal:8317"},
		{"https://api.anthropic.com", "https://api.anthropic.com"},
		{"", ""},
	} {
		if got := ContainerBaseURL(tc.in); got != tc.want {
			t.Errorf("ContainerBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

package config

import "testing"

func TestNormalizeMQTTHost(t *testing.T) {
	cases := map[string]string{
		"broker.example.com":              "broker.example.com",
		"  broker.example.com  ":          "broker.example.com",
		"https://app.example.com":         "app.example.com",
		"https://app.example.com:443/api": "app.example.com",
		"tls://broker.example.com:8883":   "broker.example.com",
		"broker.example.com/":             "broker.example.com",
		"":                                "",
		"   ":                             "",
	}

	for in, want := range cases {
		if got := normalizeMQTTHost(in); got != want {
			t.Errorf("normalizeMQTTHost(%q) = %q, want %q", in, got, want)
		}
	}
}

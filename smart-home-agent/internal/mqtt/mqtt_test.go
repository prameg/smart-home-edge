package mqtt

import (
	"errors"
	"testing"
)

func TestIsAuthError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("Connection Refused: Bad user name or password"), true},
		{errors.New("Connection Refused: Not Authorized"), true},
		{errors.New("not authorized"), true},
		{errors.New("dial tcp: connection refused"), false},
		{errors.New("i/o timeout"), false},
	}

	for _, c := range cases {
		if got := IsAuthError(c.err); got != c.want {
			t.Errorf("IsAuthError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

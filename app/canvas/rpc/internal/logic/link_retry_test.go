package logic

import (
	"errors"
	"fmt"
	"testing"

	"aigc-local-life/app/canvas/rpc/internal/javatool"
)

func TestLinkRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"network failure retries", errors.New("dial tcp: connection refused"), true},
		{"5xx retries", &javatool.Error{HTTPStatus: 503}, true},
		{"429 retries", &javatool.Error{HTTPStatus: 429}, true},
		{"401 expired token does not retry", &javatool.Error{HTTPStatus: 401, Code: 2001}, false},
		{"404 task not found does not retry", &javatool.Error{HTTPStatus: 404}, false},
		{"wrapped 4xx does not retry", fmt.Errorf("wrap: %w", &javatool.Error{HTTPStatus: 400}), false},
	}
	for _, c := range cases {
		if got := linkRetryable(c.err); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

package gate

import (
	"crypto/subtle"
	"net/http"

	"agent-forge/internal/store"
)

func NewHandler(s *store.Store, tokens map[string]string, ownerToken string) http.Handler {
	return newHandlerForTest(s, tokens, ownerToken, DefaultOptions())
}

func NewHandlerWithOptions(s *store.Store, tokens map[string]string, ownerToken string, options Options) (http.Handler, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return newHandlerForTest(s, tokens, ownerToken, options), nil
}

func newHandlerForTest(s *store.Store, tokens map[string]string, ownerToken string, options Options) http.Handler {
	for workerToken := range tokens {
		if subtle.ConstantTimeCompare([]byte(ownerToken), []byte(workerToken)) == 1 {
			ownerToken = ""
		}
	}
	return newServer(s, tokens, ownerToken, options).routes()
}

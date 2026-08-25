package gate

import (
	"crypto/sha256"
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
	ownerDigest := sha256.Sum256([]byte(ownerToken))
	for workerToken := range tokens {
		workerDigest := sha256.Sum256([]byte(workerToken))
		if subtle.ConstantTimeCompare(ownerDigest[:], workerDigest[:]) == 1 {
			ownerToken = ""
		}
	}
	return newServer(s, tokens, ownerToken, options).routes()
}

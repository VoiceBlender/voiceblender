package api

import (
	"fmt"
	"net/http"
)

// apiError carries an HTTP-style status code so do* methods can signal
// error severity to both HTTP handlers and the WS command dispatcher.
type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(code int, format string, args ...interface{}) *apiError {
	return &apiError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// handleAPIError writes the error as an HTTP response. *apiError uses its
// Code; other errors become 500.
func handleAPIError(w http.ResponseWriter, err error) {
	if ae, ok := err.(*apiError); ok {
		writeError(w, ae.Code, ae.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

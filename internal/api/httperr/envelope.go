package httperr

import (
	"net/http"
	"time"
)

// Envelope is the mandatory shape of every JSON success response with a body.
// `status` carries the numeric HTTP status code, matching the semantics of the
// member of the same name in RFC 9457 problem documents — state strings such as
// "success" are forbidden by the constitution.
type Envelope[T any] struct {
	Status    int       `json:"status" example:"200" doc:"HTTP status code of this response."`
	Data      T         `json:"data" doc:"The response payload."`
	Timestamp time.Time `json:"timestamp" doc:"When the response was produced (RFC 3339, UTC)."`
}

// Response wraps an Envelope as a huma output. Handlers return it directly;
// they never marshal a body themselves.
type Response[T any] struct {
	Body Envelope[T]
}

// Respond builds an enveloped response with an explicit status code, which must
// match the operation's DefaultStatus.
func Respond[T any](status int, data T) *Response[T] {
	return &Response[T]{Body: Envelope[T]{
		Status:    status,
		Data:      data,
		Timestamp: nowFunc(),
	}}
}

// OK builds a 200 response.
func OK[T any](data T) *Response[T] { return Respond(http.StatusOK, data) }

// Created builds a 201 response.
func Created[T any](data T) *Response[T] { return Respond(http.StatusCreated, data) }

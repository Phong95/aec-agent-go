package toolhub

import "fmt"

// Output helpers. They build the same Output values (and so the same encoded
// bytes) as the struct literals they replace: Text leaves IsError and Retryable
// nil, so neither key is written; Error sets isError true and leaves retryable
// out (retryable, the default); NotRetryable writes retryable false.

// Text returns a successful output with the given content.
func Text(s string) Output {
	return Output{Content: s}
}

// Error returns an error output (isError true) with the given content. It stays
// retryable; chain NotRetryable when retrying cannot help.
func Error(msg string) Output {
	isErr := true
	return Output{Content: msg, IsError: &isErr}
}

// Errorf is Error with a fmt.Sprintf message.
func Errorf(format string, a ...any) Output {
	return Error(fmt.Sprintf(format, a...))
}

// NotRetryable returns o with Retryable set to false: an error result the
// harness must not retry (a blocked URL, a user who already answered, an
// argument the model has to change).
func (o Output) NotRetryable() Output {
	retry := false
	o.Retryable = &retry
	return o
}

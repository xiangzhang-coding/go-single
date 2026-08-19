package testsupport

import "context"

// AllowAllAuthAttempts explicitly disables authentication rate limits in tests
// whose seam is unrelated to public authentication resource budgets.
type AllowAllAuthAttempts struct{}

func (AllowAllAuthAttempts) AllowLogin(context.Context, string, string) (bool, error) {
	return true, nil
}

func (AllowAllAuthAttempts) AllowRegister(context.Context, string, string) (bool, error) {
	return true, nil
}

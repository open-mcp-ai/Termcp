package auth

import "context"

type oauthCredentialKey struct{}

func withOAuthCredential(ctx context.Context) context.Context {
	return context.WithValue(ctx, oauthCredentialKey{}, true)
}

// IsOAuthCredential reports whether this request was admitted by the external
// OAuth verifier. MCP transports use it to distinguish a refresh of the same
// Agent's OAuth authority from a static Token rotation, which must not transfer
// an existing session to a new credential.
func IsOAuthCredential(ctx context.Context) bool {
	value, _ := ctx.Value(oauthCredentialKey{}).(bool)
	return value
}

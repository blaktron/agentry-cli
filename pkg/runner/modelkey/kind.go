package modelkey

import "strings"

// Kind is what an Anthropic credential is, read from its shape — the one
// provider whose keys come in two kinds with different powers: a Console
// API key drives any client; a subscription OAuth token (`claude
// setup-token`) is accepted by Claude Code alone, by Anthropic's terms
// and by the API. Every other provider's key is KindAPI. The kind is
// derived where it is needed and recorded nowhere.
type Kind string

const (
	// KindAPI is an API key of the provider's usual shape.
	KindAPI Kind = "api"
	// KindSubscription is an Anthropic subscription OAuth token.
	KindSubscription Kind = "subscription"
	// KindUnknown is an Anthropic credential of a shape this build does
	// not recognise (an admin key, a future format); callers treat it as
	// an API key and let the endpoint answer.
	KindUnknown Kind = "unknown"
)

// The Anthropic prefixes, as the Console and `claude setup-token` mint
// them.
const (
	anthropicAPIPrefix          = "sk-ant-api"
	anthropicSubscriptionPrefix = "sk-ant-oat"
)

// KindOf classifies key for provider, so that a form can say which runs
// a credential can drive before anything is stored, and the models
// listing can send an Anthropic token in the header shape it needs
// (agentry-cli#170); what each kind may drive at run time is
// agentry-cli#171's.
func KindOf(provider, key string) Kind {
	if strings.ToLower(strings.TrimSpace(provider)) != "anthropic" {
		return KindAPI
	}
	key = strings.TrimSpace(key)
	switch {
	case strings.HasPrefix(key, anthropicSubscriptionPrefix):
		return KindSubscription
	case strings.HasPrefix(key, anthropicAPIPrefix):
		return KindAPI
	default:
		return KindUnknown
	}
}

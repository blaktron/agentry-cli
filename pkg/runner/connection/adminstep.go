package connection

import (
	"errors"
	"fmt"
	"strings"

	"github.com/blaktron/agentry/pkg/connector"

	"github.com/blaktron/agentry-cli/pkg/mcpauth"
)

// AdminStep is what to tell a person whose consent the vendor refused
// because an administrator must approve Agentry's app first: the
// connector's step, with the link to send the administrator. It is "" for
// any other failure, for a connector without an admin step, and when the
// link cannot be made. publicClientID is the Agentry app the consent used,
// and scopes are what it asked for.
func AdminStep(err error, k connector.Connector, publicClientID string, scopes []string) string {
	var oe *mcpauth.OAuthError
	if !errors.As(err, &oe) || !k.NeedsAdmin(oe.Code, oe.Description) {
		return ""
	}
	switch k.AdminStep.Kind {
	case connector.AdminStepEntraConsent:
		link, lerr := connector.EntraAdminConsentURL(publicClientID, scopes)
		if lerr != nil {
			return ""
		}
		return fmt.Sprintf("Your organization needs an administrator to approve Agentry's app before you can connect %s. "+
			"Send your administrator this link. It approves the app for your whole organization, and the page it ends on may not load, "+
			"which is expected. Once they have approved it, connect again:\n  %s", k.Name, link)
	}
	return ""
}

// ConnectorAtIssuer is the connector with an admin step whose public client
// signs in at issuer, for a sign-in that knows the issuer and the client
// it was given but not the declared server (a runhost making a connection
// for a device, which matched the server itself).
func ConnectorAtIssuer(issuer string) (connector.Connector, bool) {
	for _, k := range connector.Builtin().All() {
		if k.Issuer != "" && k.Issuer == strings.TrimRight(issuer, "/") && k.HasPublicClient() && k.AdminStep != nil {
			return k, true
		}
	}
	return connector.Connector{}, false
}

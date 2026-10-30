package mcpauth

import (
	"net/url"
	"regexp"
	"strings"
)

// Microsoft Entra ID's v2.0 authorities publish OpenID discovery that the
// general checks refuse, though they serve a sign-in correctly:
//
//   - the multi-tenant authorities (/common/v2.0, /organizations/v2.0) name
//     the issuer https://login.microsoftonline.com/{tenantid}/v2.0, the
//     placeholder and all, and /consumers/v2.0 names the tenant id of
//     personal Microsoft accounts; only a tenant-id authority names itself;
//   - no authority lists code_challenge_methods_supported, though every one
//     takes PKCE with S256;
//   - none publishes RFC 8414 metadata (only OpenID discovery), names a
//     revocation endpoint (RFC 7009), or takes the resource parameter
//     (RFC 8707): it refuses a request carrying one (AADSTS9010010), since
//     a token's audience comes from the scopes.
//
// The profile below relaxes exactly those checks, and only for an authority
// on Entra's own host; every other issuer is held to the general rules. In
// exchange its endpoints must be on that host too.
const EntraHost = "login.microsoftonline.com"

// entraTemplateTenant is the placeholder the multi-tenant authorities name
// in their issuer.
const entraTemplateTenant = "{tenantid}"

// entraConsumersTenant is the tenant of personal Microsoft accounts, which
// the consumers authority names in its issuer.
const entraConsumersTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"

// entraTenantID is an Entra tenant id.
var entraTenantID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// entraTenant is the tenant segment of an Entra v2.0 authority
// (https://login.microsoftonline.com/<tenant>/v2.0), lowercased, and
// whether issuer is one. A port, user info, another host, a host that only
// starts or ends with Entra's, or any other path is not.
func entraTenant(issuer string) (string, bool) {
	u, err := url.Parse(strings.TrimRight(issuer, "/"))
	if err != nil || u.Scheme != "https" || u.User != nil || !strings.EqualFold(u.Host, EntraHost) || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || parts[1] != "v2.0" || parts[0] == "" {
		return "", false
	}
	return strings.ToLower(parts[0]), true
}

// IsEntra reports whether issuer is a Microsoft Entra v2.0 authority, which
// takes no resource parameter and offers no revocation.
func IsEntra(issuer string) bool {
	_, ok := entraTenant(issuer)
	return ok
}

// entraIssuerFor reports whether published is an issuer the Entra
// authority asked may name for itself: its own address, the {tenantid}
// placeholder for a multi-tenant one, the personal accounts' tenant for
// consumers.
func entraIssuerFor(asked, published string) bool {
	tenant, ok := entraTenant(asked)
	if !ok {
		return false
	}
	got, ok := entraTenant(published)
	if !ok {
		return false
	}
	if got == tenant {
		return true
	}
	switch tenant {
	case "common", "organizations":
		return got == entraTemplateTenant
	case "consumers":
		return got == entraConsumersTenant
	}
	return false
}

// entraRedirectIssuer reports whether iss, on a redirect, may come from the
// Entra authority asked: itself, or for a multi-tenant one the tenant the
// person signed in on, and for consumers the personal accounts' tenant.
func entraRedirectIssuer(asked, iss string) bool {
	tenant, ok := entraTenant(asked)
	if !ok {
		return false
	}
	got, ok := entraTenant(iss)
	if !ok {
		return false
	}
	switch {
	case got == tenant:
		return true
	case tenant == "common":
		return entraTenantID.MatchString(got)
	case tenant == "organizations":
		return entraTenantID.MatchString(got) && got != entraConsumersTenant
	case tenant == "consumers":
		return got == entraConsumersTenant
	}
	return false
}

// entraEndpointsOnHost reports whether every endpoint md names is on
// Entra's host: the relaxed checks hold only while the sign-in stays there.
func entraEndpointsOnHost(md ServerMetadata) bool {
	for _, e := range []string{md.AuthorizationEndpoint, md.TokenEndpoint, md.RegistrationEndpoint, md.RevocationEndpoint} {
		if e == "" {
			continue
		}
		u, err := url.Parse(e)
		if err != nil || u.Scheme != "https" || u.User != nil || !strings.EqualFold(u.Host, EntraHost) {
			return false
		}
	}
	return true
}

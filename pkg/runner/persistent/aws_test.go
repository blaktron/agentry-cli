package persistent

import (
	"strings"
	"testing"

	"github.com/blaktron/agentry/pkg/manifest"
)

func TestAWSServerNoteNamesAWSsOwnServerForOtherAWSServers(t *testing.T) {
	for _, srv := range []manifest.MCPServer{
		{PyPI: "awslabs-aws-api-mcp-server", Version: "1.5.6"},
		{PyPI: "awslabs.aws-documentation-mcp-server", Version: "1.2.2"},
		{PyPI: "mcp-proxy-for-aws", Version: "1.8.0"},
		{NPM: "@yawlabs/aws-mcp", Version: "2.6.0"},
		{URL: "https://knowledge-mcp.global.api.aws/mcp", Auth: manifest.AuthNone},
		{URL: "https://example.execute-api.us-east-1.amazonaws.com/mcp", Auth: manifest.AuthSecret},
	} {
		note := AWSServerNote(srv)
		if !strings.Contains(note, AWSMCPServerURL) || !strings.Contains(note, "agentry agent connect") {
			t.Errorf("%s: note %q does not name AWS's own server and how it signs in", srv.SourceID(), note)
		}
	}
}

func TestAWSServerNoteIsSilentForAWSsOwnServerAndOthers(t *testing.T) {
	for _, srv := range []manifest.MCPServer{
		{URL: AWSMCPServerURL, Auth: manifest.AuthOAuth},
		{URL: "https://aws-mcp.eu-west-1.api.aws/mcp", Auth: manifest.AuthNone},
		{URL: "https://AWS-MCP.ap-southeast-2.api.aws/mcp", Auth: manifest.AuthNone},
		{NPM: "@gnaws/mcp", Version: "0.2.0"},
		{NPM: "murphys-laws-mcp", Version: "1.2.1"},
		{PyPI: "workspace-mcp", Version: "2.0.1"},
		{URL: "https://mcp.linear.app/mcp", Auth: manifest.AuthOAuth},
		{URL: "https://aws-mcp.example.com/mcp", Auth: manifest.AuthNone},
	} {
		if note := AWSServerNote(srv); note != "" {
			t.Errorf("%s: got a note, want none: %q", srv.SourceID(), note)
		}
	}
}

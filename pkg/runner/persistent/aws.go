package persistent

// AWS's own MCP server for agents, the AWS MCP Server of the Agent Toolkit
// for AWS: a remote server AWS runs at one address per Region. A person who
// gives an agent some other server for AWS is told about it once, when the
// server is added, and nothing is changed or refused.

import (
	"strings"
	"unicode"

	"github.com/blaktron/agentry/pkg/manifest"
)

// AWSMCPServerURL is the AWS MCP Server's address in us-east-1.
const AWSMCPServerURL = "https://aws-mcp.us-east-1.api.aws/mcp"

// AWSServerNote is the line shown when srv is a server for AWS other than
// AWS's own, "" for AWS's own server and for any server not for AWS.
func AWSServerNote(srv manifest.MCPServer) string {
	if isAWSMCPServer(srv) || !isForAWS(srv) {
		return ""
	}
	return "AWS maintains its own server for agents, the AWS MCP Server of the Agent Toolkit for AWS, " +
		"at " + AWSMCPServerURL + " (or the same address in another Region it serves): " +
		"with auth none it answers AWS's documentation, skills and Regions; with auth oauth, signed in with " +
		"`agentry agent connect`, it works in your account with your IAM permissions"
}

// isAWSMCPServer says whether srv is AWS's own server, in any Region.
func isAWSMCPServer(srv manifest.MCPServer) bool {
	host := remoteHost(srv)
	region, ok := strings.CutPrefix(host, "aws-mcp.")
	return ok && strings.HasSuffix(region, ".api.aws") && !strings.Contains(strings.TrimSuffix(region, ".api.aws"), ".")
}

// isForAWS says whether srv is a server for AWS: a remote one on an AWS
// host, or a package whose name has "aws" or "awslabs" as one of its words.
func isForAWS(srv manifest.MCPServer) bool {
	if srv.IsRemote() {
		host := remoteHost(srv)
		return strings.HasSuffix(host, ".api.aws") || strings.HasSuffix(host, ".amazonaws.com")
	}
	words := strings.FieldsFunc(strings.ToLower(srv.NPM+" "+srv.PyPI), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for _, w := range words {
		if w == "aws" || w == "awslabs" {
			return true
		}
	}
	return false
}

// remoteHost is a remote server's host name, lower-cased; "" for a package
// server.
func remoteHost(srv manifest.MCPServer) string {
	return strings.ToLower(srv.RemoteHost())
}
